CLANG	?= clang
CC	?= gcc

BPF_CFLAGS ?= -O2 -g -target bpf -D__TARGET_ARCH_x86 -Wall \
	-I/usr/include/x86_64-linux-gnu

CFLAGS	?= -O2 -g -Wall -Wextra
LDFLAGS	?= -lxdp -lbpf -lelf -lz -lpthread

.PHONY: all clean test

all: xdp_filter.o trader udp_send

xdp_filter.o: xdp_filter.c
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

trader: trader.c
	$(CC) $(CFLAGS) -o $@ $< $(LDFLAGS)

udp_send: udp_send.c
	$(CC) $(CFLAGS) -o $@ $<

test: all
	@sudo ./veth_test.sh

clean:
	rm -f xdp_filter.o trader udp_send
