#include <arpa/inet.h>
#include <netinet/in.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv)
{
	const char *ip;
	const char *msg;
	int port;
	int fd;
	struct sockaddr_in dst;
	ssize_t n;

	if (argc < 3) {
		fprintf(stderr, "usage: %s <ip> <port> [payload]\n", argv[0]);
		return 1;
	}

	ip = argv[1];
	port = atoi(argv[2]);
	msg = argc > 3 ? argv[3] : "hello";

	if (port <= 0 || port > 65535) {
		fprintf(stderr, "invalid port: %s\n", argv[2]);
		return 1;
	}

	fd = socket(AF_INET, SOCK_DGRAM, 0);
	if (fd < 0) {
		perror("socket");
		return 1;
	}

	memset(&dst, 0, sizeof(dst));
	dst.sin_family = AF_INET;
	dst.sin_port = htons((uint16_t)port);
	if (inet_pton(AF_INET, ip, &dst.sin_addr) != 1) {
		fprintf(stderr, "invalid ip: %s\n", ip);
		close(fd);
		return 1;
	}

	n = sendto(fd, msg, strlen(msg), 0, (struct sockaddr *)&dst,
		   sizeof(dst));
	if (n < 0) {
		perror("sendto");
		close(fd);
		return 1;
	}

	printf("sent %zd bytes to %s:%d\n", n, ip, port);
	close(fd);
	return 0;
}
