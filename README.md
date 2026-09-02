# Continuum

XDP → AF_XDP → parse → book → strategy → AF_XDP TX.

UDP dest **9000** is market data. Everything else is `XDP_DROP`.
Orders go out UDP **9002**. Attach only on a dedicated/test interface.

```text
sudo apt-get install -y clang libbpf-dev libxdp-dev pkg-config iproute2 ethtool
make
make check              # book/strategy tape, no root
sudo make test          # veth: filter, book 100/103, BUY 101 on the wire
sudo make bench         # Phase 10: skb+copy vs native on the same tape
sudo ./trader [options] <interface>
```

`trader` tries the fast path and **prints what it actually got**:

```text
listening ... xdp=native|skb  bind=zerocopy|copy  umem=hugepage|heap
book q=0 inst=1 bid=100 x 10 ask=103 x 10
latency parse|book|strategy|tx  min/avg/max ns
```

| Phase | What it does |
|---|---|
| 1 | XDP UDP/9000 → XSKMAP → AF_XDP → UMEM |
| 2 | No per-packet `printf`. Batch RX/FILL. Counters only. |
| 3 | `--queues N`, one socket/UMEM/thread per queue, `--cpu-base N` |
| 4 | Native XDP, zero-copy, hugepages, busy-poll (fallback + print) |
| 5 | In-place `MD01` parse (add/mod/del/trade) |
| 6 | 8-level bid/ask book |
| 7 | If ask−bid ≥ 2, BUY at bid+1 |
| 8 | Build Ethernet/IP/UDP `OR01` and AF_XDP TX |
| 9 | Per-stage ns: parse, book, strategy, tx |
| 10 | Print NIC/CPU NUMA. `make bench` compares skb+copy vs native |

```text
./md_send add bid 10.67.0.1 9000 1 100 10
./md_send add ask 10.67.0.1 9000 1 103 10
./order_recv -p 9002
./trader --queues 2 --cpu-base 4 eth0
```
