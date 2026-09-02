# Continuum — Low-Latency Pipeline Phases

Build a real **XDP → AF_XDP → userspace** path first, then grow it into an HFT-style
market-data / strategy / order lab. Each phase is a complete, testable slice.
**Do not start a later phase until the current one is agreed and implemented.**

```text
                         Linux host
┌──────────────────────────────────────────────────────────┐
│   NIC                                                    │
│    │                                                     │
│    ▼                                                     │
│  XDP program                                              │
│    │                                                     │
│    ├── unwanted packet ──────► XDP_DROP                  │
│    │                                                     │
│    └── interesting packet ───► XSKMAP                    │
│                                  │                       │
│                                  ▼                       │
│                              AF_XDP ring                 │
│                                  │                       │
│                                  ▼                       │
│                         userspace trading engine         │
└──────────────────────────────────────────────────────────┘
```

Stack for the early phases:

- **C** for the XDP/eBPF program
- **libbpf** to load it
- **AF_XDP** for userspace RX (and later TX)
- a **UDP port filter** in XDP
- a userspace loop that parses Ethernet / IPv4 / UDP

---

## Phase map

```text
Phase 1  Ingress path          XDP filter + XSKMAP + AF_XDP + UMEM + parse
Phase 2  Hot-path hygiene      no printf, batch RX, counters
Phase 3  Locality              pin threads, 1 XSK per RX queue, RSS
Phase 4  Fast path             native XDP, zero-copy, busy-poll, hugepages
Phase 5  Market-data parser    binary messages, no hot-path alloc
Phase 6  Order book            in-memory book from incremental updates
Phase 7  Strategy              signal → order intent
Phase 8  Egress                AF_XDP TX order builder → NIC
Phase 9  Measurement lab       per-stage latency, timestamps, benches
Phase 10 Compare / harden      DPDK vs AF_XDP, NUMA, isolation
```

---

## Phase 1 — Smallest real ingress pipeline

**Goal:** a packet that matches a UDP port is *not* dropped as a toy. It is
redirected out of the kernel stack into shared UMEM and parsed in userspace.

```text
NIC → XDP (UDP port filter) → XSKMAP[rx_queue] → AF_XDP → UMEM → parse Ethernet/IP/UDP
```

**In scope**

- `xdp_filter.c`: Ethernet / IPv4 / UDP parse, dest-port match, `bpf_redirect_map(&xsks, queue, XDP_DROP)`
- `trader.c`: allocate UMEM, create AF_XDP socket, load BPF object, insert socket FD into `xsks`
- prime the **fill ring** (otherwise RX never starts)
- SKB mode + `XDP_COPY` so it runs on any NIC / veth
- parse headers and print ports / payload length (learning path)

**Out of scope:** zero-copy, native driver XDP, pinning, book, strategy, TX.

**Why the XSKMAP exists**

```text
queue 0 ──► AF_XDP socket 0
queue 1 ──► AF_XDP socket 1
```

Userspace must do `xsks[queue] = xsk_fd`. XDP only redirects; it does not invent
the socket.

**Done when:** a UDP packet to the filter port on the bound interface appears
in userspace; other traffic is dropped by XDP and never hits the socket.

---

## Phase 2 — Hot-path hygiene

**Goal:** the receive loop is a packet processor, not a logger.

**In scope**

- remove `printf` from the per-packet path
- batch: `xsk_ring_cons__peek(..., 64, ...)` then process the batch
- recycle frames to the fill ring in batches
- counters (`packets`, `bytes`, `drops`) printed on a timer / signal

**Done when:** the same Phase 1 path runs without I/O in the inner loop, and a
periodic stats line is the only console output.

---

## Phase 3 — CPU and queue locality

**Goal:** one RX queue, one AF_XDP socket, one pinned thread. No locks on the
packet path between queues.

```text
NIC RSS → RX0 / RX1 / RX2
            │     │     │
           CPU4  CPU5  CPU6
            │     │     │
           XSK0  XSK1  XSK2
```

**In scope**

- `pthread_setaffinity_np` (or equivalent) per RX thread
- one XSK + XSKMAP entry per queue
- document / script RSS + queue count (`ethtool -L` / `-X`)

**Out of scope:** isolated cpusets, irqaffinity, full production isolation
(those land in Phase 10).

**Done when:** N queues can run N threads with no shared mutable state on RX.

---

## Phase 4 — Fast path (still AF_XDP)

**Goal:** leave the portable SKB/copy path and use the NIC’s real XDP path.

**In scope**

- native / driver XDP when the NIC supports it; SKB as fallback
- `XDP_ZEROCOPY` when the driver allows it
- busy-poll (`SO_BUSY_POLL` / `SO_PREFER_BUSY_POLL`)
- page-aligned UMEM; hugepages for frames

**Done when:** the same filter+parse path can run in copy *or* zero-copy, and
mode is explicit (flag / config), not accidental.

---

## Phase 5 — Market-data parser

**Goal:** userspace turns UDP payload into typed events. Still no book.

**In scope**

- fixed binary (or simple length-prefixed) market-data format
- parse in place on UMEM frames
- no malloc / printf on the hot path
- invalid frames increment a counter and are recycled

**Done when:** a synthetic feed produces a stream of typed events
(add / modify / delete / trade) with a count of parse failures.

---

## Phase 6 — Order book

**Goal:** those events update an in-memory book.

**In scope**

- price-level book per instrument
- deterministic apply of incremental updates
- top-of-book (and maybe a few levels) readable by the strategy
- snapshot vs incremental only if needed for the chosen feed

**Out of scope:** persistence, GUI, multi-venue matching.

**Done when:** a recorded or synthetic tape rebuilds a book that matches a
known end state.

---

## Phase 7 — Strategy

**Goal:** book state produces an *order intent*, not a wire packet yet.

**In scope**

- one simple, deterministic strategy (e.g. join/cancel at top, or a spread)
- intent struct: side, price, size, instrument
- no syscalls, no locks shared with other queues if possible

**Done when:** given a book sequence, the strategy emits a known intent
sequence (unit-testable).

---

## Phase 8 — Order egress (AF_XDP TX)

**Goal:** intent becomes an Ethernet/IP/UDP (or exchange) frame on the TX ring.

```text
strategy → order builder → AF_XDP TX ring → NIC
                              │
                              ▼
                         completion ring
```

**In scope**

- build frames into UMEM
- submit TX, reap completion ring
- keep RX fill/RX path working on the same or a paired socket

**Done when:** a generated order is observed on the wire (veth / second
namespace / packet dump) and TX completions are accounted.

---

## Phase 9 — Measurement lab

**Goal:** every stage has a number, not a feeling.

```text
NIC → XDP                 = X
XDP → AF_XDP              = Y
AF_XDP → parser           = Z
parser → book/strategy    = …
strategy → TX             = …
```

**In scope**

- software timestamps at stage boundaries
- hardware timestamps if the NIC exposes them
- load generator + recorder
- drop / overrun counters (fill ring empty, RX full)

**Done when:** a single command prints a per-stage breakdown for a fixed
offered load.

---

## Phase 10 — Compare and harden

**Goal:** the project is a low-latency networking lab, not an eBPF tutorial.

**In scope (pick, do not boil the ocean)**

- AF_XDP zero-copy vs DPDK on the same host
- NUMA placement of UMEM and threads
- isolated CPUs, IRQ pinning, nohz_full (documented, optional)
- multi-NIC / failover only if useful

**Done when:** there is a written comparison of at least two RX paths
(e.g. SKB-copy vs native zero-copy) on the same benchmark from Phase 9.

---

## Rules for how we build

1. One phase at a time. Talk through the phase, then implement only that phase.
2. Phase 1 stays portable (SKB + copy) so a veth pair is enough to test.
3. Later phases may require a real NIC; that is expected, not a surprise.
4. Benchmark before and after each optimization phase (2, 3, 4, 9).
5. No production trading claims: this is a lab pipeline.

When you want to start, say **Phase 1** and we implement only that slice.
