#define _GNU_SOURCE

#include <arpa/inet.h>
#include <getopt.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/time.h>
#include <unistd.h>

#include "md.h"

int main(int argc, char **argv)
{
	int port = ORDER_PORT;
	int expect = 0;
	int timeout_ms = 2000;
	int opt;
	int fd;
	int got = 0;
	struct sockaddr_in addr;
	struct timeval tv;

	while ((opt = getopt(argc, argv, "p:n:t:")) != -1) {
		switch (opt) {
		case 'p':
			port = atoi(optarg);
			break;
		case 'n':
			expect = atoi(optarg);
			break;
		case 't':
			timeout_ms = atoi(optarg);
			break;
		default:
			fprintf(stderr,
				"usage: %s [-p port] [-n expect] [-t timeout_ms]\n",
				argv[0]);
			return 1;
		}
	}

	fd = socket(AF_INET, SOCK_DGRAM, 0);
	if (fd < 0) {
		perror("socket");
		return 1;
	}

	memset(&addr, 0, sizeof(addr));
	addr.sin_family = AF_INET;
	addr.sin_port = htons((uint16_t)port);
	addr.sin_addr.s_addr = htonl(INADDR_ANY);
	if (bind(fd, (struct sockaddr *)&addr, sizeof(addr))) {
		perror("bind");
		close(fd);
		return 1;
	}

	tv.tv_sec = timeout_ms / 1000;
	tv.tv_usec = (timeout_ms % 1000) * 1000;
	setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof(tv));

	printf("order_recv listening :%d\n", port);
	fflush(stdout);

	for (;;) {
		struct order_msg msg;
		ssize_t n = recv(fd, &msg, sizeof(msg), 0);

		if (n < 0)
			break;
		if (n < (ssize_t)sizeof(msg) || msg.magic != ORDER_MAGIC)
			continue;

		got++;
		printf("order: %s inst=%u px=%d sz=%u seq=%u\n",
		       msg.side == SIDE_BUY ? "BUY" : "SELL",
		       msg.instrument, msg.price, msg.size, msg.seq);
		fflush(stdout);
		if (expect > 0 && got >= expect)
			break;
	}

	printf("order_recv got=%d\n", got);
	close(fd);
	return expect > 0 && got < expect ? 1 : 0;
}
