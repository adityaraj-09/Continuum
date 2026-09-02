# Continuum

Low-latency packet path: **XDP → AF_XDP → userspace**, grown in phases toward
an HFT-style market-data / strategy / order lab.

See **[PHASES.md](PHASES.md)** for the full layout. Nothing is implemented
until a phase is explicitly started.

```text
NIC → XDP filter → XSKMAP → AF_XDP → userspace engine
```
