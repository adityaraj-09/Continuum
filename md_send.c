#define _GNU_SOURCE

#include <arpa/inet.h>
#include <getopt.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "md.h"

static int parse_type(const char *s)
{
	if (!strcmp(s, "add"))
		return MD_ADD;
	if (!strcmp(s, "mod") || !strcmp(s, "modify"))
		return MD_MODIFY;
	if (!strcmp(s, "del") || !strcmp(s, "delete"))
		return MD_DELETE;
	if (!strcmp(s, "trade"))
		return MD_TRADE;
	return -1;
}

static int parse_side(const char *s)
{
	if (!strcmp(s, "bid") || !strcmp(s, "buy"))
		return SIDE_BID;
	if (!strcmp(s, "ask") || !strcmp(s, "sell"))
		return SIDE_ASK;
	return -1;
}

int main(int argc, char **argv)
{
	int count = 1;
	int sport = 0;
	int opt;
	int fd;
	int type, side;
	struct md_msg msg;
	struct sockaddr_in dst, src;

	while ((opt = getopt(argc, argv, "n:s:")) != -1) {
		switch (opt) {
		case 'n':
			count = atoi(optarg);
			break;
		case 's':
			sport = atoi(optarg);
			break;
		default:
			goto usage;
		}
	}

	if (argc - optind < 7)
		goto usage;

	type = parse_type(argv[optind]);
	side = parse_side(argv[optind + 1]);
	if (type < 0 || side < 0)
		goto usage;

	memset(&msg, 0, sizeof(msg));
	msg.magic = MD_MAGIC;
	msg.type = (uint16_t)type;
	msg.side = (uint16_t)side;
	msg.instrument = (uint16_t)atoi(argv[optind + 4]);
	msg.price = atoi(argv[optind + 5]);
	msg.size = (uint32_t)atoi(argv[optind + 6]);

	fd = socket(AF_INET, SOCK_DGRAM, 0);
	if (fd < 0) {
		perror("socket");
		return 1;
	}

	if (sport > 0) {
		memset(&src, 0, sizeof(src));
		src.sin_family = AF_INET;
		src.sin_port = htons((uint16_t)sport);
		if (bind(fd, (struct sockaddr *)&src, sizeof(src))) {
			perror("bind");
			close(fd);
			return 1;
		}
	}

	memset(&dst, 0, sizeof(dst));
	dst.sin_family = AF_INET;
	dst.sin_port = htons((uint16_t)atoi(argv[optind + 3]));
	if (inet_pton(AF_INET, argv[optind + 2], &dst.sin_addr) != 1) {
		fprintf(stderr, "invalid ip: %s\n", argv[optind + 2]);
		close(fd);
		return 1;
	}

	for (int i = 0; i < count; i++) {
		msg.seq = (uint32_t)(i + 1);
		if (sendto(fd, &msg, sizeof(msg), 0, (struct sockaddr *)&dst,
			   sizeof(dst)) < 0) {
			perror("sendto");
			close(fd);
			return 1;
		}
	}

	printf("sent %d md %s %s inst=%u px=%d sz=%u to %s:%s\n",
	       count, argv[optind], argv[optind + 1], msg.instrument,
	       msg.price, msg.size, argv[optind + 2], argv[optind + 3]);
	close(fd);
	return 0;

usage:
	fprintf(stderr,
		"usage: %s [-n count] [-s sport] "
		"add|mod|del|trade bid|ask <ip> <port> <inst> <price> <size>\n",
		argv[0]);
	return 1;
}
