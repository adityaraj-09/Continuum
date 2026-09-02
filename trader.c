#define _GNU_SOURCE

#include <arpa/inet.h>
#include <errno.h>
#include <getopt.h>
#include <inttypes.h>
#include <limits.h>
#include <net/if.h>
#include <poll.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <time.h>
#include <unistd.h>

#include <netinet/in.h>

#include <linux/if_ether.h>
#include <linux/if_link.h>
#include <linux/if_xdp.h>
#include <linux/ip.h>
#include <linux/udp.h>

#include <bpf/bpf.h>
#include <bpf/libbpf.h>
#include <xdp/xsk.h>

#ifndef SO_BUSY_POLL
#define SO_BUSY_POLL 46
#endif
#ifndef SO_PREFER_BUSY_POLL
#define SO_PREFER_BUSY_POLL 69
#endif
#ifndef SO_BUSY_POLL_BUDGET
#define SO_BUSY_POLL_BUDGET 70
#endif

#define FRAME_SIZE	2048
#define NUM_FRAMES	4096
#define RX_BATCH	64
#define FILL_SIZE	2048
#define COMP_SIZE	2048
#define RX_SIZE		2048
#define TX_SIZE		2048
#define MARKET_DATA_PORT 9000
#define HUGEPAGE_SIZE	(2UL * 1024 * 1024)
#define MAX_QUEUES	64

struct cfg {
	const char *ifname;
	uint32_t nqueues;
	int cpu_base;
	bool force_skb;
	bool force_native;
	bool force_copy;
	bool force_zerocopy;
	bool force_hugepage;
	bool no_hugepage;
	bool busy_poll;
	int poll_ms;
	int stats_ms;
	int busy_us;
	bool dump;
};

struct stats {
	uint64_t packets;
	uint64_t bytes;
	uint64_t empty;
	uint64_t wakeup;
	uint64_t bad;
};

struct worker {
	pthread_t tid;
	uint32_t queue;
	int cpu;
	struct xsk_socket *xsk;
	struct xsk_umem *umem;
	struct xsk_ring_cons rx;
	struct xsk_ring_prod fq;
	struct xsk_ring_prod tx;
	struct xsk_ring_cons cq;
	void *buffer;
	size_t buffer_size;
	bool hugepage;
	int xsk_fd;
	struct stats stats;
};

struct app {
	struct cfg cfg;
	struct bpf_object *obj;
	int ifindex;
	int map_fd;
	int prog_fd;
	uint32_t xdp_flags;
	uint16_t bind_flags;
	bool zerocopy;
	bool hugepage;
	char xdp_note[128];
	char bind_note[128];
	char umem_note[128];
	struct worker workers[MAX_QUEUES];
};

static volatile sig_atomic_t stop;
static volatile sig_atomic_t want_stats;
static struct app app;

static void on_stop(int sig)
{
	(void)sig;
	stop = 1;
}

static void on_stats(int sig)
{
	(void)sig;
	want_stats = 1;
}

static int libbpf_print(enum libbpf_print_level level, const char *fmt,
			va_list args)
{
	if (level > LIBBPF_WARN)
		return 0;
	return vfprintf(stderr, fmt, args);
}

static void usage(const char *argv0)
{
	fprintf(stderr,
		"usage: %s [options] <interface>\n"
		"\n"
		"  --queues N          AF_XDP sockets / RX queues (default 1)\n"
		"  --cpu-base N        pin thread i to CPU N+i (default 0, -1 = off)\n"
		"  --skb / --native    force XDP mode (default: native, then SKB)\n"
		"  --copy / --zerocopy force bind mode (default: zc, then copy)\n"
		"  --hugepage          require hugepage UMEM\n"
		"  --no-hugepage       skip hugepage attempt\n"
		"  --busy-poll         SO_BUSY_POLL (default on)\n"
		"  --no-busy-poll      poll/sleep on idle RX\n"
		"  --poll-ms N         idle poll timeout (default 0 if busy-poll else 1000)\n"
		"  --stats-ms N        periodic counters (default 1000, 0 = signal/exit only)\n"
		"  --dump              Phase 1 per-packet printf (debug only)\n",
		argv0);
}

static int parse_args(int argc, char **argv, struct cfg *c)
{
	int poll_set = 0;

	memset(c, 0, sizeof(*c));
	c->nqueues = 1;
	c->cpu_base = 0;
	c->busy_poll = true;
	c->stats_ms = 1000;
	c->busy_us = 20;
	c->poll_ms = -1;

	static const struct option opts[] = {
		{"queues", required_argument, NULL, 'q'},
		{"cpu-base", required_argument, NULL, 'c'},
		{"skb", no_argument, NULL, 'S'},
		{"native", no_argument, NULL, 'N'},
		{"copy", no_argument, NULL, 'C'},
		{"zerocopy", no_argument, NULL, 'Z'},
		{"hugepage", no_argument, NULL, 'H'},
		{"no-hugepage", no_argument, NULL, 'h'},
		{"busy-poll", no_argument, NULL, 'B'},
		{"no-busy-poll", no_argument, NULL, 'b'},
		{"poll-ms", required_argument, NULL, 'p'},
		{"stats-ms", required_argument, NULL, 's'},
		{"dump", no_argument, NULL, 'd'},
		{0, 0, 0, 0},
	};

	int opt;

	while ((opt = getopt_long(argc, argv, "", opts, NULL)) != -1) {
		switch (opt) {
		case 'q':
			c->nqueues = (uint32_t)atoi(optarg);
			break;
		case 'c':
			c->cpu_base = atoi(optarg);
			break;
		case 'S':
			c->force_skb = true;
			break;
		case 'N':
			c->force_native = true;
			break;
		case 'C':
			c->force_copy = true;
			break;
		case 'Z':
			c->force_zerocopy = true;
			break;
		case 'H':
			c->force_hugepage = true;
			break;
		case 'h':
			c->no_hugepage = true;
			break;
		case 'B':
			c->busy_poll = true;
			break;
		case 'b':
			c->busy_poll = false;
			break;
		case 'p':
			c->poll_ms = atoi(optarg);
			poll_set = 1;
			break;
		case 's':
			c->stats_ms = atoi(optarg);
			break;
		case 'd':
			c->dump = true;
			break;
		default:
			usage(argv[0]);
			return -1;
		}
	}

	if (optind != argc - 1) {
		usage(argv[0]);
		return -1;
	}

	c->ifname = argv[optind];
	if (!c->nqueues || c->nqueues > MAX_QUEUES) {
		fprintf(stderr, "queues must be 1..%d\n", MAX_QUEUES);
		return -1;
	}
	if (c->force_skb && c->force_native) {
		fprintf(stderr, "--skb and --native are exclusive\n");
		return -1;
	}
	if (c->force_copy && c->force_zerocopy) {
		fprintf(stderr, "--copy and --zerocopy are exclusive\n");
		return -1;
	}
	if (c->force_hugepage && c->no_hugepage) {
		fprintf(stderr, "--hugepage and --no-hugepage are exclusive\n");
		return -1;
	}
	if (!poll_set)
		c->poll_ms = c->busy_poll ? 0 : 1000;
	return 0;
}

static int locate_bpf_object(char *out, size_t n)
{
	char self[PATH_MAX];
	ssize_t m = readlink("/proc/self/exe", self, sizeof(self) - 1);

	if (m > 0) {
		self[m] = '\0';
		char *slash = strrchr(self, '/');

		if (slash) {
			*slash = '\0';
			snprintf(out, n, "%s/xdp_filter.o", self);
			if (access(out, R_OK) == 0)
				return 0;
		}
	}

	snprintf(out, n, "xdp_filter.o");
	return access(out, R_OK) == 0 ? 0 : -1;
}

static int iface_rx_queues(const char *ifname)
{
	char path[256];
	int n = 0;

	for (int i = 0; i < MAX_QUEUES; i++) {
		snprintf(path, sizeof(path),
			 "/sys/class/net/%s/queues/rx-%d", ifname, i);
		if (access(path, F_OK) != 0)
			break;
		n++;
	}
	return n;
}

static int pin_cpu(int cpu)
{
	cpu_set_t set;

	CPU_ZERO(&set);
	CPU_SET(cpu, &set);
	if (pthread_setaffinity_np(pthread_self(), sizeof(set), &set)) {
		fprintf(stderr, "pin cpu %d: %s\n", cpu, strerror(errno));
		return -1;
	}
	return 0;
}

static bool parse_ok(void *packet, uint32_t len, uint16_t *src, uint16_t *dst,
		     uint32_t *payload_len)
{
	struct ethhdr *eth = packet;

	if (len < sizeof(*eth))
		return false;
	if (ntohs(eth->h_proto) != ETH_P_IP)
		return false;

	struct iphdr *ip = (struct iphdr *)(eth + 1);

	if ((uint8_t *)ip + sizeof(*ip) > (uint8_t *)packet + len)
		return false;
	if (ip->protocol != IPPROTO_UDP || ip->ihl < 5)
		return false;

	struct udphdr *udp =
		(struct udphdr *)((uint8_t *)ip + ip->ihl * 4);

	if ((uint8_t *)udp + sizeof(*udp) > (uint8_t *)packet + len)
		return false;

	uint8_t *payload = (uint8_t *)(udp + 1);
	uint32_t header_len = (uint32_t)(payload - (uint8_t *)packet);

	*src = ntohs(udp->source);
	*dst = ntohs(udp->dest);
	*payload_len = len > header_len ? len - header_len : 0;
	return true;
}

static void count_packet(struct worker *w, void *packet, uint32_t len)
{
	uint16_t src = 0, dst = 0;
	uint32_t payload_len = 0;

	if (!parse_ok(packet, len, &src, &dst, &payload_len)) {
		w->stats.bad++;
		return;
	}

	w->stats.packets++;
	w->stats.bytes += len;

	if (app.cfg.dump) {
		printf("packet: %u bytes  %u -> %u  payload=%u bytes\n",
		       len, src, dst, payload_len);
		fflush(stdout);
	}
}

static int prime_fill_ring(struct worker *w, uint32_t n_frames)
{
	uint32_t idx;
	uint32_t reserved = xsk_ring_prod__reserve(&w->fq, n_frames, &idx);

	if (reserved != n_frames) {
		fprintf(stderr, "q%u fill prime: reserved %u / %u\n",
			w->queue, reserved, n_frames);
		if (!reserved)
			return -1;
	}

	for (uint32_t i = 0; i < reserved; i++)
		*xsk_ring_prod__fill_addr(&w->fq, idx + i) =
			(uint64_t)i * FRAME_SIZE;

	xsk_ring_prod__submit(&w->fq, reserved);
	return 0;
}

static void recycle_frames(struct worker *w, const uint64_t *addrs, uint32_t n)
{
	uint32_t idx;
	uint32_t done = 0;

	while (done < n) {
		uint32_t want = n - done;
		uint32_t reserved = xsk_ring_prod__reserve(&w->fq, want, &idx);

		if (!reserved) {
			if (xsk_ring_prod__needs_wakeup(&w->fq)) {
				recvfrom(w->xsk_fd, NULL, 0, MSG_DONTWAIT,
					 NULL, NULL);
				w->stats.wakeup++;
			}
			continue;
		}

		for (uint32_t i = 0; i < reserved; i++)
			*xsk_ring_prod__fill_addr(&w->fq, idx + i) =
				addrs[done + i];

		xsk_ring_prod__submit(&w->fq, reserved);
		done += reserved;
	}
}

static int alloc_umem_area(struct worker *w, bool try_huge, bool require_huge)
{
	size_t need = (size_t)FRAME_SIZE * NUM_FRAMES;
	size_t size = need;

	w->buffer = NULL;
	w->hugepage = false;

	if (try_huge) {
		size = (need + HUGEPAGE_SIZE - 1) & ~(HUGEPAGE_SIZE - 1);
		w->buffer = mmap(NULL, size, PROT_READ | PROT_WRITE,
				 MAP_PRIVATE | MAP_ANONYMOUS | MAP_HUGETLB,
				 -1, 0);
		if (w->buffer != MAP_FAILED) {
			w->buffer_size = size;
			w->hugepage = true;
			return 0;
		}
		w->buffer = NULL;
		if (require_huge) {
			fprintf(stderr, "hugepage mmap(%zu): %s\n",
				size, strerror(errno));
			return -1;
		}
	}

	if (posix_memalign(&w->buffer, getpagesize(), need)) {
		perror("posix_memalign");
		return -1;
	}
	w->buffer_size = need;
	memset(w->buffer, 0, need);
	return 0;
}

static void free_umem_area(struct worker *w)
{
	if (!w->buffer)
		return;
	if (w->hugepage)
		munmap(w->buffer, w->buffer_size);
	else
		free(w->buffer);
	w->buffer = NULL;
}

static int load_bpf(struct app *a, const char *obj_path)
{
	struct bpf_program *prog;
	struct bpf_map *map;
	int err;

	a->obj = bpf_object__open_file(obj_path, NULL);
	if (!a->obj) {
		fprintf(stderr, "bpf_object__open_file(%s): %s\n",
			obj_path, strerror(errno));
		return -1;
	}

	err = bpf_object__load(a->obj);
	if (err) {
		fprintf(stderr, "bpf_object__load: %s\n", strerror(-err));
		return -1;
	}

	prog = bpf_object__find_program_by_name(a->obj, "xdp_filter");
	if (!prog) {
		fprintf(stderr, "program xdp_filter not found\n");
		return -1;
	}

	map = bpf_object__find_map_by_name(a->obj, "xsks");
	if (!map) {
		fprintf(stderr, "map xsks not found\n");
		return -1;
	}

	a->prog_fd = bpf_program__fd(prog);
	a->map_fd = bpf_map__fd(map);
	return 0;
}

static int attach_xdp(struct app *a)
{
	int err;

	if (!a->cfg.force_skb) {
		err = bpf_xdp_attach(a->ifindex, a->prog_fd,
				     XDP_FLAGS_DRV_MODE, NULL);
		if (!err) {
			a->xdp_flags = XDP_FLAGS_DRV_MODE;
			snprintf(a->xdp_note, sizeof(a->xdp_note), "native");
			return 0;
		}
		snprintf(a->xdp_note, sizeof(a->xdp_note),
			 "skb (native failed: %s)", strerror(-err));
		if (a->cfg.force_native) {
			fprintf(stderr, "native XDP attach: %s\n",
				strerror(-err));
			return -1;
		}
	}

	err = bpf_xdp_attach(a->ifindex, a->prog_fd, XDP_FLAGS_SKB_MODE, NULL);
	if (err) {
		fprintf(stderr, "skb XDP attach: %s\n", strerror(-err));
		return -1;
	}
	a->xdp_flags = XDP_FLAGS_SKB_MODE;
	if (a->cfg.force_skb)
		snprintf(a->xdp_note, sizeof(a->xdp_note), "skb");
	return 0;
}

static int enable_busy_poll(int fd, int busy_us)
{
	int prefer = 1;
	int budget = RX_BATCH;
	int ok = 0;

	if (setsockopt(fd, SOL_SOCKET, SO_BUSY_POLL, &busy_us,
		       sizeof(busy_us)) == 0)
		ok++;
	if (setsockopt(fd, SOL_SOCKET, SO_PREFER_BUSY_POLL, &prefer,
		       sizeof(prefer)) == 0)
		ok++;
	if (setsockopt(fd, SOL_SOCKET, SO_BUSY_POLL_BUDGET, &budget,
		       sizeof(budget)) == 0)
		ok++;
	return ok;
}

static int create_worker_socket(struct app *a, struct worker *w)
{
	struct xsk_umem_config umem_cfg = {
		.fill_size = FILL_SIZE,
		.comp_size = COMP_SIZE,
		.frame_size = FRAME_SIZE,
		.frame_headroom = 0,
	};
	struct xsk_socket_config xsk_cfg = {
		.rx_size = RX_SIZE,
		.tx_size = TX_SIZE,
		.libxdp_flags = XSK_LIBXDP_FLAGS__INHIBIT_PROG_LOAD,
		.xdp_flags = a->xdp_flags,
		.bind_flags = XDP_USE_NEED_WAKEUP,
	};
	uint16_t try_flags[2];
	int ntry = 0;
	int err = -1;

	if (a->cfg.force_copy) {
		try_flags[ntry++] = XDP_COPY | XDP_USE_NEED_WAKEUP;
	} else {
		try_flags[ntry++] = XDP_ZEROCOPY | XDP_USE_NEED_WAKEUP;
		if (!a->cfg.force_zerocopy)
			try_flags[ntry++] = XDP_COPY | XDP_USE_NEED_WAKEUP;
	}

	err = xsk_umem__create(&w->umem, w->buffer, w->buffer_size,
			       &w->fq, &w->cq, &umem_cfg);
	if (err) {
		fprintf(stderr, "q%u UMEM: %s\n", w->queue, strerror(-err));
		return -1;
	}

	if (prime_fill_ring(w, FILL_SIZE))
		return -1;

	for (int i = 0; i < ntry; i++) {
		xsk_cfg.bind_flags = try_flags[i];
		err = xsk_socket__create(&w->xsk, a->cfg.ifname, w->queue,
					 w->umem, &w->rx, &w->tx, &xsk_cfg);
		if (!err) {
			a->bind_flags = try_flags[i];
			a->zerocopy = (try_flags[i] & XDP_ZEROCOPY) != 0;
			if (a->zerocopy)
				snprintf(a->bind_note, sizeof(a->bind_note),
					 "zerocopy");
			else if (i > 0)
				snprintf(a->bind_note, sizeof(a->bind_note),
					 "copy (zerocopy failed)");
			else
				snprintf(a->bind_note, sizeof(a->bind_note),
					 "copy");
			break;
		}
	}

	if (err) {
		fprintf(stderr, "q%u XSK: %s\n", w->queue, strerror(-err));
		return -1;
	}

	w->xsk_fd = xsk_socket__fd(w->xsk);
	err = bpf_map_update_elem(a->map_fd, &w->queue, &w->xsk_fd, 0);
	if (err) {
		fprintf(stderr, "xsks[%u] = fd %d: %s\n",
			w->queue, w->xsk_fd, strerror(errno));
		return -1;
	}

	if (a->cfg.busy_poll)
		enable_busy_poll(w->xsk_fd, a->cfg.busy_us);

	return 0;
}

static void *rx_loop(void *arg)
{
	struct worker *w = arg;
	uint64_t recycle[RX_BATCH];
	struct pollfd pfd = {
		.fd = w->xsk_fd,
		.events = POLLIN,
	};

	if (w->cpu >= 0 && pin_cpu(w->cpu))
		return NULL;

	printf("worker q=%u tid=%d pinned_cpu=%d fd=%d\n",
	       w->queue, (int)gettid(), w->cpu, w->xsk_fd);
	fflush(stdout);

	while (!stop) {
		uint32_t idx_rx;
		uint32_t received =
			xsk_ring_cons__peek(&w->rx, RX_BATCH, &idx_rx);

		if (!received) {
			w->stats.empty++;
			if (xsk_ring_prod__needs_wakeup(&w->fq)) {
				recvfrom(w->xsk_fd, NULL, 0, MSG_DONTWAIT,
					 NULL, NULL);
				w->stats.wakeup++;
			}
			poll(&pfd, 1, app.cfg.poll_ms);
			continue;
		}

		for (uint32_t i = 0; i < received; i++) {
			const struct xdp_desc *desc =
				xsk_ring_cons__rx_desc(&w->rx, idx_rx + i);
			void *packet =
				xsk_umem__get_data(w->buffer, desc->addr);

			count_packet(w, packet, desc->len);
			recycle[i] = desc->addr;
		}

		recycle_frames(w, recycle, received);
		xsk_ring_cons__release(&w->rx, received);
	}

	return NULL;
}

static void print_stats(const struct app *a)
{
	uint64_t packets = 0, bytes = 0, empty = 0, wakeup = 0, bad = 0;

	for (uint32_t i = 0; i < a->cfg.nqueues; i++) {
		const struct worker *w = &a->workers[i];

		printf("stats q=%u cpu=%d packets=%" PRIu64 " bytes=%" PRIu64
		       " empty=%" PRIu64 " wakeup=%" PRIu64 " bad=%" PRIu64
		       "\n",
		       w->queue, w->cpu, w->stats.packets, w->stats.bytes,
		       w->stats.empty, w->stats.wakeup, w->stats.bad);
		packets += w->stats.packets;
		bytes += w->stats.bytes;
		empty += w->stats.empty;
		wakeup += w->stats.wakeup;
		bad += w->stats.bad;
	}

	if (a->cfg.nqueues > 1)
		printf("stats total packets=%" PRIu64 " bytes=%" PRIu64
		       " empty=%" PRIu64 " wakeup=%" PRIu64 " bad=%" PRIu64
		       "\n",
		       packets, bytes, empty, wakeup, bad);
	fflush(stdout);
}

static void cleanup(struct app *a)
{
	if (a->ifindex && a->xdp_flags)
		bpf_xdp_detach(a->ifindex, a->xdp_flags, NULL);

	for (uint32_t i = 0; i < a->cfg.nqueues; i++) {
		struct worker *w = &a->workers[i];

		if (w->xsk)
			xsk_socket__delete(w->xsk);
		if (w->umem)
			xsk_umem__delete(w->umem);
		free_umem_area(w);
	}

	if (a->obj)
		bpf_object__close(a->obj);
}

int main(int argc, char **argv)
{
	char obj_path[PATH_MAX];
	int ncpu;
	int hw_rx;
	bool try_huge;
	struct timespec last, now;

	if (parse_args(argc, argv, &app.cfg))
		return 1;

	app.ifindex = (int)if_nametoindex(app.cfg.ifname);
	if (!app.ifindex) {
		perror("if_nametoindex");
		return 1;
	}

	hw_rx = iface_rx_queues(app.cfg.ifname);
	if (hw_rx <= 0)
		hw_rx = 1;
	if ((int)app.cfg.nqueues > hw_rx) {
		fprintf(stderr,
			"%s has %d RX queue(s); asked for %u\n",
			app.cfg.ifname, hw_rx, app.cfg.nqueues);
		return 1;
	}

	ncpu = (int)sysconf(_SC_NPROCESSORS_ONLN);
	if (app.cfg.cpu_base >= 0 &&
	    app.cfg.cpu_base + (int)app.cfg.nqueues > ncpu) {
		fprintf(stderr, "cpu-base %d + %u queues exceeds %d CPUs\n",
			app.cfg.cpu_base, app.cfg.nqueues, ncpu);
		return 1;
	}

	if (locate_bpf_object(obj_path, sizeof(obj_path))) {
		fprintf(stderr, "cannot find xdp_filter.o\n");
		return 1;
	}

	libbpf_set_print(libbpf_print);
	signal(SIGINT, on_stop);
	signal(SIGTERM, on_stop);
	signal(SIGUSR1, on_stats);

	if (load_bpf(&app, obj_path))
		return 1;

	if (attach_xdp(&app)) {
		cleanup(&app);
		return 1;
	}

	try_huge = !app.cfg.no_hugepage;
	for (uint32_t i = 0; i < app.cfg.nqueues; i++) {
		struct worker *w = &app.workers[i];

		w->queue = i;
		w->cpu = app.cfg.cpu_base < 0 ? -1 : app.cfg.cpu_base + (int)i;
		if (alloc_umem_area(w, try_huge, app.cfg.force_hugepage)) {
			cleanup(&app);
			return 1;
		}
		if (create_worker_socket(&app, w)) {
			cleanup(&app);
			return 1;
		}
	}

	app.hugepage = app.workers[0].hugepage;
	if (app.hugepage)
		snprintf(app.umem_note, sizeof(app.umem_note), "hugepage");
	else if (try_huge)
		snprintf(app.umem_note, sizeof(app.umem_note),
			 "heap (hugepage failed)");
	else
		snprintf(app.umem_note, sizeof(app.umem_note), "heap");

	for (uint32_t i = 0; i < app.cfg.nqueues; i++) {
		if (pthread_create(&app.workers[i].tid, NULL, rx_loop,
				   &app.workers[i])) {
			perror("pthread_create");
			stop = 1;
			cleanup(&app);
			return 1;
		}
	}

	printf("listening on %s queues=%u udp=%u xdp=%s bind=%s umem=%s busy_poll=%d\n",
	       app.cfg.ifname, app.cfg.nqueues, MARKET_DATA_PORT,
	       app.xdp_note, app.bind_note, app.umem_note,
	       app.cfg.busy_poll ? 1 : 0);
	fflush(stdout);

	clock_gettime(CLOCK_MONOTONIC, &last);
	while (!stop) {
		if (want_stats) {
			want_stats = 0;
			print_stats(&app);
		}

		if (app.cfg.stats_ms > 0) {
			clock_gettime(CLOCK_MONOTONIC, &now);
			long dt = (now.tv_sec - last.tv_sec) * 1000 +
				  (now.tv_nsec - last.tv_nsec) / 1000000L;

			if (dt >= app.cfg.stats_ms) {
				print_stats(&app);
				last = now;
			}
		}

		usleep(20 * 1000);
	}

	for (uint32_t i = 0; i < app.cfg.nqueues; i++)
		pthread_join(app.workers[i].tid, NULL);

	print_stats(&app);
	cleanup(&app);
	return 0;
}
