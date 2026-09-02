#define _GNU_SOURCE

#include <arpa/inet.h>
#include <errno.h>
#include <limits.h>
#include <net/if.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
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

#define FRAME_SIZE	2048
#define NUM_FRAMES	4096
#define RX_BATCH	64
#define FILL_SIZE	2048
#define COMP_SIZE	2048
#define RX_SIZE		2048
#define TX_SIZE		2048
#define QUEUE_ID	0
#define MARKET_DATA_PORT 9000

struct app {
	struct xsk_socket *xsk;
	struct xsk_umem *umem;
	struct xsk_ring_cons rx;
	struct xsk_ring_prod fq;
	struct xsk_ring_prod tx;
	struct xsk_ring_cons cq;
	void *buffer;
	struct bpf_object *obj;
	int ifindex;
	int xsk_fd;
	__u32 queue;
};

static volatile sig_atomic_t stop;
static struct app app;

static void on_signal(int sig)
{
	(void)sig;
	stop = 1;
}

static int libbpf_print(enum libbpf_print_level level, const char *fmt,
			va_list args)
{
	if (level > LIBBPF_WARN)
		return 0;
	return vfprintf(stderr, fmt, args);
}

static void process_packet(void *packet, uint32_t len)
{
	struct ethhdr *eth = packet;

	if (len < sizeof(*eth))
		return;

	if (ntohs(eth->h_proto) != ETH_P_IP)
		return;

	struct iphdr *ip = (struct iphdr *)(eth + 1);

	if ((uint8_t *)ip + sizeof(*ip) > (uint8_t *)packet + len)
		return;

	if (ip->protocol != IPPROTO_UDP)
		return;

	if (ip->ihl < 5)
		return;

	struct udphdr *udp =
		(struct udphdr *)((uint8_t *)ip + ip->ihl * 4);

	if ((uint8_t *)udp + sizeof(*udp) > (uint8_t *)packet + len)
		return;

	uint16_t src_port = ntohs(udp->source);
	uint16_t dst_port = ntohs(udp->dest);
	uint8_t *payload = (uint8_t *)(udp + 1);
	uint32_t header_len = (uint32_t)(payload - (uint8_t *)packet);
	uint32_t payload_len = len > header_len ? len - header_len : 0;

	printf("packet: %u bytes  %u -> %u  payload=%u bytes\n",
	       len, src_port, dst_port, payload_len);
	fflush(stdout);
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

static int prime_fill_ring(struct app *a, uint32_t n_frames)
{
	uint32_t idx;
	uint32_t reserved = xsk_ring_prod__reserve(&a->fq, n_frames, &idx);

	if (reserved != n_frames) {
		fprintf(stderr, "fill ring prime: reserved %u / %u\n",
			reserved, n_frames);
		if (!reserved)
			return -1;
	}

	for (uint32_t i = 0; i < reserved; i++)
		*xsk_ring_prod__fill_addr(&a->fq, idx + i) =
			(uint64_t)i * FRAME_SIZE;

	xsk_ring_prod__submit(&a->fq, reserved);
	return 0;
}

static void recycle_frames(struct app *a, const uint64_t *addrs, uint32_t n)
{
	uint32_t idx;
	uint32_t done = 0;

	while (done < n) {
		uint32_t want = n - done;
		uint32_t reserved = xsk_ring_prod__reserve(&a->fq, want, &idx);

		if (!reserved) {
			if (xsk_ring_prod__needs_wakeup(&a->fq))
				recvfrom(a->xsk_fd, NULL, 0, MSG_DONTWAIT,
					 NULL, NULL);
			continue;
		}

		for (uint32_t i = 0; i < reserved; i++)
			*xsk_ring_prod__fill_addr(&a->fq, idx + i) =
				addrs[done + i];

		xsk_ring_prod__submit(&a->fq, reserved);
		done += reserved;
	}
}

static int load_and_attach(struct app *a, const char *obj_path)
{
	int err;
	struct bpf_program *prog;
	struct bpf_map *map;
	int prog_fd;
	int map_fd;
	int xsk_fd;

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
		fprintf(stderr, "program xdp_filter not found in %s\n",
			obj_path);
		return -1;
	}

	prog_fd = bpf_program__fd(prog);
	map = bpf_object__find_map_by_name(a->obj, "xsks");
	if (!map) {
		fprintf(stderr, "map xsks not found in %s\n", obj_path);
		return -1;
	}

	map_fd = bpf_map__fd(map);
	xsk_fd = xsk_socket__fd(a->xsk);

	err = bpf_map_update_elem(map_fd, &a->queue, &xsk_fd, 0);
	if (err) {
		fprintf(stderr, "xsks[%u] = fd %d failed: %s\n",
			a->queue, xsk_fd, strerror(errno));
		return -1;
	}

	err = bpf_xdp_attach(a->ifindex, prog_fd, XDP_FLAGS_SKB_MODE, NULL);
	if (err) {
		fprintf(stderr, "bpf_xdp_attach: %s\n", strerror(-err));
		return -1;
	}

	printf("XSKMAP: xsks[%u] = fd %d\n", a->queue, xsk_fd);
	return 0;
}

static void cleanup(struct app *a)
{
	if (a->ifindex)
		bpf_xdp_detach(a->ifindex, XDP_FLAGS_SKB_MODE, NULL);

	if (a->xsk)
		xsk_socket__delete(a->xsk);

	if (a->umem)
		xsk_umem__delete(a->umem);

	if (a->obj)
		bpf_object__close(a->obj);

	free(a->buffer);
}

int main(int argc, char **argv)
{
	const char *ifname;
	char obj_path[PATH_MAX];
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
		.xdp_flags = XDP_FLAGS_SKB_MODE,
		.bind_flags = XDP_COPY | XDP_USE_NEED_WAKEUP,
	};
	int err;
	uint64_t recycle[RX_BATCH];

	if (argc != 2) {
		fprintf(stderr, "usage: %s <interface>\n", argv[0]);
		return 1;
	}

	ifname = argv[1];
	app.queue = QUEUE_ID;
	app.ifindex = (int)if_nametoindex(ifname);
	if (!app.ifindex) {
		perror("if_nametoindex");
		return 1;
	}

	if (locate_bpf_object(obj_path, sizeof(obj_path))) {
		fprintf(stderr, "cannot find xdp_filter.o next to trader or in cwd\n");
		return 1;
	}

	libbpf_set_print(libbpf_print);
	signal(SIGINT, on_signal);
	signal(SIGTERM, on_signal);

	printf("Interface: %s (%d)  queue %u  object %s\n",
	       ifname, app.ifindex, app.queue, obj_path);

	if (posix_memalign(&app.buffer, getpagesize(),
			   (size_t)FRAME_SIZE * NUM_FRAMES)) {
		perror("posix_memalign");
		return 1;
	}

	memset(app.buffer, 0, (size_t)FRAME_SIZE * NUM_FRAMES);

	err = xsk_umem__create(&app.umem, app.buffer,
			       (uint64_t)FRAME_SIZE * NUM_FRAMES,
			       &app.fq, &app.cq, &umem_cfg);
	if (err) {
		fprintf(stderr, "UMEM creation failed: %s\n", strerror(-err));
		cleanup(&app);
		return 1;
	}

	if (prime_fill_ring(&app, FILL_SIZE)) {
		fprintf(stderr, "failed to prime fill ring\n");
		cleanup(&app);
		return 1;
	}

	err = xsk_socket__create(&app.xsk, ifname, app.queue, app.umem,
				 &app.rx, &app.tx, &xsk_cfg);
	if (err) {
		fprintf(stderr, "XSK creation failed: %s\n", strerror(-err));
		cleanup(&app);
		return 1;
	}

	app.xsk_fd = xsk_socket__fd(app.xsk);

	if (load_and_attach(&app, obj_path)) {
		cleanup(&app);
		return 1;
	}

	printf("listening on %s queue %u  UDP dest %u  (SKB + copy)\n",
	       ifname, app.queue, MARKET_DATA_PORT);
	fflush(stdout);

	while (!stop) {
		uint32_t idx_rx;
		uint32_t received;

		received = xsk_ring_cons__peek(&app.rx, RX_BATCH, &idx_rx);
		if (!received) {
			if (xsk_ring_prod__needs_wakeup(&app.fq))
				recvfrom(app.xsk_fd, NULL, 0, MSG_DONTWAIT,
					 NULL, NULL);

			struct pollfd pfd = {
				.fd = app.xsk_fd,
				.events = POLLIN,
			};
			poll(&pfd, 1, 1000);
			continue;
		}

		for (uint32_t i = 0; i < received; i++) {
			const struct xdp_desc *desc =
				xsk_ring_cons__rx_desc(&app.rx, idx_rx + i);
			void *packet = xsk_umem__get_data(app.buffer, desc->addr);

			process_packet(packet, desc->len);
			recycle[i] = desc->addr;
		}

		recycle_frames(&app, recycle, received);
		xsk_ring_cons__release(&app.rx, received);
	}

	cleanup(&app);
	return 0;
}
