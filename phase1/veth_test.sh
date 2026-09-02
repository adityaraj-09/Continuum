#!/usr/bin/env bash
# Phases 2–4: counters only, pinned worker, explicit XDP/bind/UMEM mode.
# UDP/9000 increments packets; UDP/9001 does not.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
cd "$DIR"

NS_A=c8m-a
NS_B=c8m-b
VETH_A=c8a
VETH_B=c8b
IP_A=10.67.0.1
IP_B=10.67.0.2

cleanup() {
	if [[ -n "${TPID:-}" ]]; then
		kill "$TPID" 2>/dev/null || true
		wait "$TPID" 2>/dev/null || true
	fi
	ip netns del "$NS_A" 2>/dev/null || true
	ip netns del "$NS_B" 2>/dev/null || true
}
trap cleanup EXIT

if [[ ! -x ./trader || ! -x ./udp_send || ! -f ./xdp_filter.o ]]; then
	echo "build first: make -C phase1" >&2
	exit 1
fi

# Best-effort hugepages so Phase 4 can take the hugepage UMEM path.
sysctl -w vm.nr_hugepages=8 >/dev/null 2>&1 || true

ip netns del "$NS_A" 2>/dev/null || true
ip netns del "$NS_B" 2>/dev/null || true

ip netns add "$NS_A"
ip netns add "$NS_B"
ip link add "$VETH_A" type veth peer name "$VETH_B"
ip link set "$VETH_A" netns "$NS_A"
ip link set "$VETH_B" netns "$NS_B"

ip netns exec "$NS_A" ip addr add "$IP_A/24" dev "$VETH_A"
ip netns exec "$NS_B" ip addr add "$IP_B/24" dev "$VETH_B"
ip netns exec "$NS_A" ip link set "$VETH_A" up
ip netns exec "$NS_B" ip link set "$VETH_B" up
ip netns exec "$NS_A" ip link set lo up
ip netns exec "$NS_B" ip link set lo up

MAC_A=$(ip netns exec "$NS_A" cat /sys/class/net/$VETH_A/address)
MAC_B=$(ip netns exec "$NS_B" cat /sys/class/net/$VETH_B/address)
ip netns exec "$NS_A" ip neigh replace "$IP_B" lladdr "$MAC_B" dev "$VETH_A"
ip netns exec "$NS_B" ip neigh replace "$IP_A" lladdr "$MAC_A" dev "$VETH_B"

QUEUES=1
if command -v ethtool >/dev/null && ip netns exec "$NS_A" ethtool -L "$VETH_A" combined 2 >/dev/null; then
	QUEUES=2
fi

LOG=$(mktemp)
ip netns exec "$NS_A" ./trader \
	--queues "$QUEUES" \
	--cpu-base 0 \
	--busy-poll \
	--stats-ms 0 \
	"$VETH_A" >"$LOG" 2>&1 &
TPID=$!

ready=0
for _ in $(seq 1 50); do
	if grep -q "listening" "$LOG"; then
		ready=1
		break
	fi
	if ! kill -0 "$TPID" 2>/dev/null; then
		break
	fi
	sleep 0.1
done

if [[ "$ready" -ne 1 ]]; then
	echo "trader failed to start" >&2
	cat "$LOG" >&2
	exit 1
fi

dump_stats() {
	kill -USR1 "$TPID"
	sleep 0.2
}

# Phase 2: wrong port must not increment the counter.
ip netns exec "$NS_B" ./udp_send -n 3 "$IP_A" 9001 "phase-miss"
sleep 0.3
dump_stats

if grep -q '^packet:' "$LOG"; then
	echo "FAIL: per-packet printf still on the hot path" >&2
	cat "$LOG" >&2
	exit 1
fi

miss_packets=$(grep '^stats q=' "$LOG" | sed -n 's/.*packets=\([0-9]*\).*/\1/p' | awk '{s+=$1} END{print s+0}')
if [[ "$miss_packets" -ne 0 ]]; then
	echo "FAIL: UDP/9001 incremented packets ($miss_packets)" >&2
	cat "$LOG" >&2
	exit 1
fi

HIT=20
ip netns exec "$NS_B" ./udp_send -n "$HIT" "$IP_A" 9000 "phase-hit"
sleep 0.4
dump_stats

hit_packets=$(grep '^stats q=' "$LOG" | sed -n 's/.*packets=\([0-9]*\).*/\1/p' | awk '{s+=$1} END{print s+0}')
if [[ "$hit_packets" -lt "$HIT" ]]; then
	echo "FAIL: expected >= $HIT UDP/9000 packets, got $hit_packets" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -q 'pinned_cpu=0' "$LOG"; then
	echo "FAIL: worker was not pinned" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -qE 'xdp=(native|skb)' "$LOG"; then
	echo "FAIL: XDP mode not reported" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -qE 'bind=(zerocopy|copy)' "$LOG"; then
	echo "FAIL: bind mode not reported" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -qE 'umem=(hugepage|heap)' "$LOG"; then
	echo "FAIL: UMEM mode not reported" >&2
	cat "$LOG" >&2
	exit 1
fi

echo "---- trader log ----"
cat "$LOG"
echo "PASS: $hit_packets UDP/9000 counted, 9001 dropped, no hot-path printf, queues=$QUEUES"
