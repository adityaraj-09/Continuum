#define _GNU_SOURCE

#include <arpa/inet.h>
#include <getopt.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	const char *ip;
	const char *msg = "hello";
	int port;
	int fd;
	int count = 1;
	int sport = 0;
	int opt;
	struct sockaddr_in dst, src;
	ssize_t n;
	int sent = 0;

	while ((opt = getopt(argc, argv, "n:s:")) != -1) {
		switch (opt) {
		case 'n':
			count = atoi(optarg);
			break;
		case 's':
			sport = atoi(optarg);
			break;
		default:
			fprintf(stderr,
				"usage: %s [-n count] [-s sport] <ip> <port> [payload]\n",
				argv[0]);
			return 1;
		}
	}

	if (argc - optind < 2) {
		fprintf(stderr,
			"usage: %s [-n count] [-s sport] <ip> <port> [payload]\n",
			argv[0]);
		return 1;
	}

	ip = argv[optind];
	port = atoi(argv[optind + 1]);
	if (argc - optind >= 3)
		msg = argv[optind + 2];

	if (port <= 0 || port > 65535 || count <= 0) {
		fprintf(stderr, "invalid port or count\n");
		return 1;
	}

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
	dst.sin_port = htons((uint16_t)port);
	if (inet_pton(AF_INET, ip, &dst.sin_addr) != 1) {
		fprintf(stderr, "invalid ip: %s\n", ip);
		close(fd);
		return 1;
	}

	for (int i = 0; i < count; i++) {
		n = sendto(fd, msg, strlen(msg), 0, (struct sockaddr *)&dst,
			   sizeof(dst));
		if (n < 0) {
			perror("sendto");
			close(fd);
			return 1;
		}
		sent++;
	}

	printf("sent %d datagrams (%zu bytes each) to %s:%d\n",
	       sent, strlen(msg), ip, port);
	close(fd);
	return 0;
}
