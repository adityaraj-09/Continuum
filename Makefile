CLANG	?= clang
CC	?= gcc

BPF_CFLAGS ?= -O2 -g -target bpf -D__TARGET_ARCH_x86 -Wall \
	-I/usr/include/x86_64-linux-gnu

CFLAGS	?= -O2 -g -Wall -Wextra
LDFLAGS	?= -lxdp -lbpf -lelf -lz -lpthread

.PHONY: all clean test check bench

all: xdp_filter.o trader udp_send md_send order_recv engine_test

xdp_filter.o: xdp_filter.c
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@

trader: trader.c engine.c engine.h md.h
	$(CC) $(CFLAGS) -o $@ trader.c engine.c $(LDFLAGS)

udp_send: udp_send.c
	$(CC) $(CFLAGS) -o $@ $<

md_send: md_send.c md.h
	$(CC) $(CFLAGS) -o $@ md_send.c

order_recv: order_recv.c md.h
	$(CC) $(CFLAGS) -o $@ order_recv.c

engine_test: engine_test.c engine.c engine.h md.h
	$(CC) $(CFLAGS) -o $@ engine_test.c engine.c

check: engine_test
	./engine_test

test: all check
	@sudo ./veth_test.sh

bench: all
	@sudo ./bench.sh

clean:
	rm -f xdp_filter.o trader udp_send md_send order_recv engine_test
