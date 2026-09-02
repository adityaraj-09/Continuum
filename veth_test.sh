#!/usr/bin/env bash
# Full pipeline: filter, counters, pin, mode, parser, book, strategy, TX, latency.
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
	if [[ -n "${OPID:-}" ]]; then
		kill "$OPID" 2>/dev/null || true
		wait "$OPID" 2>/dev/null || true
	fi
	ip netns del "$NS_A" 2>/dev/null || true
	ip netns del "$NS_B" 2>/dev/null || true
}
trap cleanup EXIT

if [[ ! -x ./trader || ! -x ./udp_send || ! -x ./md_send || ! -x ./order_recv || ! -f ./xdp_filter.o ]]; then
	echo "build first: make" >&2
	exit 1
fi

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
if command -v ethtool >/dev/null && ip netns exec "$NS_A" ethtool -L "$VETH_A" combined 2 >/dev/null 2>&1; then
	QUEUES=2
fi

LOG=$(mktemp)
ip netns exec "$NS_A" ./trader \
	--queues "$QUEUES" \
	--cpu-base 0 \
	--busy-poll \
	--poll-ms 1 \
	--stats-ms 0 \
	--order-port 9002 \
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

OLOG=$(mktemp)
ip netns exec "$NS_B" ./order_recv -p 9002 -n 1 -t 3000 >"$OLOG" 2>&1 &
OPID=$!
for _ in $(seq 1 30); do
	grep -q listening "$OLOG" && break
	sleep 0.05
done

ip netns exec "$NS_B" ./md_send add bid "$IP_A" 9000 1 100 10
ip netns exec "$NS_B" ./md_send add ask "$IP_A" 9000 1 103 10
sleep 0.5
dump_stats

wait "$OPID" || true
OPID=

if ! grep -q 'book q=0 inst=1 bid=100 x 10 ask=103 x 10' "$LOG"; then
	echo "FAIL: book did not reach 100/103" >&2
	cat "$LOG" >&2
	cat "$OLOG" >&2
	exit 1
fi

intents=$(grep '^stats q=' "$LOG" | tail -1 | sed -n 's/.*intents=\([0-9]*\).*/\1/p')
tx=$(grep '^stats q=' "$LOG" | tail -1 | sed -n 's/.* tx=\([0-9]*\).*/\1/p')
if [[ "${intents:-0}" -lt 1 || "${tx:-0}" -lt 1 ]]; then
	echo "FAIL: expected intent+TX, intents=$intents tx=$tx" >&2
	cat "$LOG" >&2
	cat "$OLOG" >&2
	exit 1
fi

if ! grep -q 'order: BUY inst=1 px=101 sz=1' "$OLOG"; then
	echo "FAIL: order not received on UDP/9002" >&2
	cat "$OLOG" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -q 'latency parse' "$LOG" || ! grep -q 'latency tx' "$LOG"; then
	echo "FAIL: missing Phase 9 latency lines" >&2
	cat "$LOG" >&2
	exit 1
fi

if ! grep -q 'locality if=' "$LOG"; then
	echo "FAIL: missing Phase 10 locality line" >&2
	cat "$LOG" >&2
	exit 1
fi

echo "---- trader log ----"
cat "$LOG"
echo "---- order_recv ----"
cat "$OLOG"
echo "PASS: filter+book+BUY 101 TX+latency queues=$QUEUES"
