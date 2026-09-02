#include "engine.h"

#include <string.h>
#include <time.h>

#include <arpa/inet.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/udp.h>
#include <netinet/in.h>

uint64_t nsec_now(void)
{
	struct timespec ts;

	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (uint64_t)ts.tv_sec * 1000000000ull + (uint64_t)ts.tv_nsec;
}

void stamp_add(struct stamp *s, uint64_t ns)
{
	if (!s->n || ns < s->min_ns)
		s->min_ns = ns;
	if (ns > s->max_ns)
		s->max_ns = ns;
	s->sum_ns += ns;
	s->n++;
}

bool udp_payload(const void *packet, uint32_t len,
		 const uint8_t **payload, uint32_t *payload_len)
{
	const uint8_t *p = packet;
	const struct ethhdr *eth;
	const struct iphdr *ip;
	const struct udphdr *udp;
	uint32_t off;

	if (len < sizeof(*eth))
		return false;
	eth = (const struct ethhdr *)p;
	if (ntohs(eth->h_proto) != ETH_P_IP)
		return false;

	if (len < sizeof(*eth) + sizeof(*ip))
		return false;
	ip = (const struct iphdr *)(eth + 1);
	if (ip->protocol != IPPROTO_UDP || ip->ihl < 5)
		return false;

	off = sizeof(*eth) + (uint32_t)ip->ihl * 4;
	if (len < off + sizeof(*udp))
		return false;
	udp = (const struct udphdr *)(p + off);
	off += sizeof(*udp);
	*payload = p + off;
	*payload_len = len > off ? len - off : 0;
	(void)udp;
	return true;
}

bool md_parse(const uint8_t *payload, uint32_t payload_len,
	      struct md_event *ev)
{
	const struct md_msg *m;

	if (payload_len < sizeof(*m))
		return false;
	m = (const struct md_msg *)payload;
	if (m->magic != MD_MAGIC)
		return false;
	if (m->type < MD_ADD || m->type > MD_TRADE)
		return false;
	if (m->side > SIDE_ASK)
		return false;

	ev->type = m->type;
	ev->side = m->side;
	ev->instrument = m->instrument;
	ev->price = m->price;
	ev->size = m->size;
	ev->seq = m->seq;
	return true;
}

bool reply_path_from_rx(const void *packet, uint32_t len,
			struct reply_path *path)
{
	const uint8_t *p = packet;
	const struct ethhdr *eth;
	const struct iphdr *ip;
	const struct udphdr *udp;
	uint32_t off;

	if (len < sizeof(*eth) + sizeof(*ip) + sizeof(*udp))
		return false;
	eth = (const struct ethhdr *)p;
	ip = (const struct iphdr *)(eth + 1);
	off = sizeof(*eth) + (uint32_t)ip->ihl * 4;
	if (len < off + sizeof(*udp))
		return false;
	udp = (const struct udphdr *)(p + off);

	memcpy(path->local_mac, eth->h_dest, 6);
	memcpy(path->remote_mac, eth->h_source, 6);
	path->local_ip = ip->daddr;
	path->remote_ip = ip->saddr;
	path->local_port = udp->dest;
	path->valid = 1;
	return true;
}

void book_init(struct book *b)
{
	memset(b, 0, sizeof(*b));
}

static int find_level(const struct book_side *s, int32_t price)
{
	for (int i = 0; i < s->n; i++) {
		if (s->lv[i].price == price)
			return i;
	}
	return -1;
}

static void sort_side(struct book_side *s, int bid)
{
	for (int i = 1; i < s->n; i++) {
		struct book_level t = s->lv[i];
		int j = i;

		while (j > 0) {
			int better = bid ? s->lv[j - 1].price < t.price
					 : s->lv[j - 1].price > t.price;

			if (!better)
				break;
			s->lv[j] = s->lv[j - 1];
			j--;
		}
		s->lv[j] = t;
	}
}

static void side_upsert(struct book_side *s, int32_t price, uint32_t size,
			int bid)
{
	int i = find_level(s, price);

	if (size == 0) {
		if (i < 0)
			return;
		for (; i < s->n - 1; i++)
			s->lv[i] = s->lv[i + 1];
		s->n--;
		return;
	}

	if (i >= 0) {
		s->lv[i].size = size;
		return;
	}

	if (s->n == BOOK_LEVELS) {
		int worst = s->n - 1;

		if (bid ? price <= s->lv[worst].price
			: price >= s->lv[worst].price)
			return;
		s->n--;
	}

	s->lv[s->n].price = price;
	s->lv[s->n].size = size;
	s->n++;
	sort_side(s, bid);
}

void book_apply(struct book *b, const struct md_event *ev)
{
	struct book_side *s;
	int bid;

	b->instrument = ev->instrument;
	b->applies++;

	if (ev->type == MD_TRADE)
		return;

	bid = ev->side == SIDE_BID;
	s = bid ? &b->bid : &b->ask;

	if (ev->type == MD_DELETE)
		side_upsert(s, ev->price, 0, bid);
	else
		side_upsert(s, ev->price, ev->size, bid);
}

bool book_top(const struct book *b, int32_t *bid, uint32_t *bid_sz,
	      int32_t *ask, uint32_t *ask_sz)
{
	if (!b->bid.n || !b->ask.n)
		return false;
	if (bid)
		*bid = b->bid.lv[0].price;
	if (bid_sz)
		*bid_sz = b->bid.lv[0].size;
	if (ask)
		*ask = b->ask.lv[0].price;
	if (ask_sz)
		*ask_sz = b->ask.lv[0].size;
	return true;
}

bool strategy_decide(const struct book *b, uint32_t *seq,
		     struct order_intent *out)
{
	int32_t bid, ask;
	uint32_t bid_sz, ask_sz;

	if (!book_top(b, &bid, &bid_sz, &ask, &ask_sz))
		return false;
	if (ask - bid < 2)
		return false;

	out->side = SIDE_BUY;
	out->instrument = b->instrument;
	out->price = bid + 1;
	out->size = 1;
	out->seq = ++(*seq);
	return true;
}

static uint16_t ip_checksum(const void *buf, int len)
{
	const uint16_t *p = buf;
	uint32_t sum = 0;

	while (len > 1) {
		sum += *p++;
		len -= 2;
	}
	if (len)
		sum += *(const uint8_t *)p;
	while (sum >> 16)
		sum = (sum & 0xffff) + (sum >> 16);
	return (uint16_t)~sum;
}

int order_frame_write(uint8_t *dst, int dst_len,
		      const struct reply_path *path, uint16_t dst_port,
		      uint16_t ip_id, const struct order_intent *in)
{
	struct ethhdr *eth;
	struct iphdr *ip;
	struct udphdr *udp;
	struct order_msg *om;
	uint16_t udp_len = (uint16_t)(sizeof(*udp) + sizeof(*om));

	if (!path || !path->valid || dst_len < ORDER_FRAME_LEN)
		return -1;

	memset(dst, 0, ORDER_FRAME_LEN);
	eth = (struct ethhdr *)dst;
	ip = (struct iphdr *)(eth + 1);
	udp = (struct udphdr *)(ip + 1);
	om = (struct order_msg *)(udp + 1);

	memcpy(eth->h_dest, path->remote_mac, 6);
	memcpy(eth->h_source, path->local_mac, 6);
	eth->h_proto = htons(ETH_P_IP);

	ip->ihl = 5;
	ip->version = 4;
	ip->tos = 0;
	ip->tot_len = htons((uint16_t)(sizeof(*ip) + udp_len));
	ip->id = htons(ip_id);
	ip->frag_off = htons(0x4000);
	ip->ttl = 64;
	ip->protocol = IPPROTO_UDP;
	ip->saddr = path->local_ip;
	ip->daddr = path->remote_ip;
	ip->check = 0;
	ip->check = ip_checksum(ip, sizeof(*ip));

	udp->source = path->local_port;
	udp->dest = htons(dst_port);
	udp->len = htons(udp_len);
	udp->check = 0;

	om->magic = ORDER_MAGIC;
	om->side = in->side;
	om->instrument = in->instrument;
	om->price = in->price;
	om->size = in->size;
	om->seq = in->seq;
	return ORDER_FRAME_LEN;
}
