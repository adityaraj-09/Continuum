# Continuum

Phase 1: NIC → XDP UDP filter → XSKMAP → AF_XDP → UMEM → userspace parse.

Unwanted packets are `XDP_DROP`. UDP dest **9000** is redirected to an AF_XDP
socket on RX **queue 0** (SKB + copy so it runs on veth).

```text
sudo apt-get install -y clang libbpf-dev libxdp-dev pkg-config
make -C phase1
sudo make -C phase1 test          # veth pair: 9000 hits, 9001 drops
sudo ./phase1/trader <interface>  # attach on a dedicated/test NIC
```

Do not attach this on the NIC you use for SSH: non-matching IPv4 is dropped.

Need `CAP_SYS_ADMIN` / root, `clang -target bpf`, libbpf, and libxdp.
