# Continuum

XDP → AF_XDP → userspace receive path, built in phases.

UDP dest **9000** is redirected to AF_XDP. Everything else is `XDP_DROP`.
Attach only on a dedicated/test interface.

```text
sudo apt-get install -y clang libbpf-dev libxdp-dev pkg-config iproute2 ethtool
make -C phase1
sudo make -C phase1 test
sudo ./phase1/trader [options] <interface>
```

`trader` always tries the fast path and **prints what it actually got**:

```text
listening ... xdp=native|skb  bind=zerocopy|copy  umem=hugepage|heap  busy_poll=1
```

| Phase | What the binary does |
|---|---|
| 2 | No per-packet `printf`. Batch RX/FILL. `packets=` counters (1s, SIGUSR1, exit). |
| 3 | `--queues N` → one socket, one UMEM, one thread per RX queue. `--cpu-base N` pins thread *i* to CPU N+i. |
| 4 | Try native XDP, zero-copy, hugepage UMEM, `SO_BUSY_POLL`. Fall back and say so. |

```text
./phase1/trader --queues 2 --cpu-base 4 --native --zerocopy --hugepage eth0
./phase1/trader --skb --copy --no-hugepage --no-busy-poll veth0
```
