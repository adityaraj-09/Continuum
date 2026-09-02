#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/udp.h>

#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#define MARKET_DATA_PORT 9000

/*
 * queue index → AF_XDP socket fd
 *
 * userspace inserts: xsks[rx_queue_index] = socket_fd
 * this program redirects: bpf_redirect_map(&xsks, queue, XDP_DROP)
 */
struct {
	__uint(type, BPF_MAP_TYPE_XSKMAP);
	__uint(max_entries, 64);
	__type(key, __u32);
	__type(value, __u32);
} xsks SEC(".maps");

SEC("xdp")
int xdp_filter(struct xdp_md *ctx)
{
	void *data = (void *)(long)ctx->data;
	void *data_end = (void *)(long)ctx->data_end;

	struct ethhdr *eth = data;

	if ((void *)(eth + 1) > data_end)
		return XDP_DROP;

	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return XDP_DROP;

	struct iphdr *ip = (void *)(eth + 1);

	if ((void *)(ip + 1) > data_end)
		return XDP_DROP;

	if (ip->protocol != IPPROTO_UDP)
		return XDP_DROP;

	/* Phase 1: IPv4 options are dropped so the verifier stays simple. */
	if (ip->ihl != 5)
		return XDP_DROP;

	struct udphdr *udp = (void *)(ip + 1);

	if ((void *)(udp + 1) > data_end)
		return XDP_DROP;

	if (udp->dest != bpf_htons(MARKET_DATA_PORT))
		return XDP_DROP;

	return bpf_redirect_map(&xsks, ctx->rx_queue_index, XDP_DROP);
}

char LICENSE[] SEC("license") = "GPL";
