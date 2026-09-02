#!/usr/bin/env bash
# Phase 1: UDP/9000 must reach userspace; UDP/9001 must not.
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
# XDP_DROP skips ARP; pin neighbors so unicast UDP still goes out.
ip netns exec "$NS_A" ip neigh replace "$IP_B" lladdr "$MAC_B" dev "$VETH_A"
ip netns exec "$NS_B" ip neigh replace "$IP_A" lladdr "$MAC_A" dev "$VETH_B"

LOG=$(mktemp)
ip netns exec "$NS_A" ./trader "$VETH_A" >"$LOG" 2>&1 &
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

for _ in 1 2 3; do
	ip netns exec "$NS_B" ./udp_send "$IP_A" 9000 "phase1-hit"
	ip netns exec "$NS_B" ./udp_send "$IP_A" 9001 "phase1-miss"
done

sleep 0.8
kill "$TPID" 2>/dev/null || true
wait "$TPID" 2>/dev/null || true
TPID=

echo "---- trader log ----"
cat "$LOG"

if ! grep -qE '-> 9000' "$LOG"; then
	echo "FAIL: UDP/9000 never reached userspace" >&2
	exit 1
fi

if grep -qE '-> 9001' "$LOG"; then
	echo "FAIL: UDP/9001 was forwarded to AF_XDP" >&2
	exit 1
fi

echo "PASS: UDP/9000 hit AF_XDP; UDP/9001 was dropped"
