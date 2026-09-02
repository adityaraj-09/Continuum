#!/usr/bin/env bash
# Phase 10: same tape, two RX modes, print Phase 9 latency side by side.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
cd "$DIR"

NS_A=c8m-a
NS_B=c8m-b
VETH_A=c8a
VETH_B=c8b
IP_A=10.67.0.1
IP_B=10.67.0.2
N=2000

cleanup() {
	if [[ -n "${TPID:-}" ]]; then
		kill "$TPID" 2>/dev/null || true
		wait "$TPID" 2>/dev/null || true
	fi
	ip netns del "$NS_A" 2>/dev/null || true
	ip netns del "$NS_B" 2>/dev/null || true
}
trap cleanup EXIT

sysctl -w vm.nr_hugepages=8 >/dev/null 2>&1 || true

run_mode() {
	local label=$1
	shift

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
	MAC_A=$(ip netns exec "$NS_A" cat /sys/class/net/$VETH_A/address)
	MAC_B=$(ip netns exec "$NS_B" cat /sys/class/net/$VETH_B/address)
	ip netns exec "$NS_A" ip neigh replace "$IP_B" lladdr "$MAC_B" dev "$VETH_A"
	ip netns exec "$NS_B" ip neigh replace "$IP_A" lladdr "$MAC_A" dev "$VETH_B"

	local log
	log=$(mktemp)
	ip netns exec "$NS_A" ./trader --cpu-base 0 --busy-poll --poll-ms 1 --stats-ms 0 "$@" "$VETH_A" >"$log" 2>&1 &
	TPID=$!
	for _ in $(seq 1 50); do
		grep -q listening "$log" && break
		sleep 0.1
	done

	ip netns exec "$NS_B" ./md_send add bid "$IP_A" 9000 1 100 10
	ip netns exec "$NS_B" ./md_send -n "$N" add ask "$IP_A" 9000 1 103 10
	sleep 0.4
	kill -USR1 "$TPID"
	sleep 0.2
	kill "$TPID" 2>/dev/null || true
	wait "$TPID" 2>/dev/null || true
	TPID=

	echo "==== $label ===="
	grep -E '^(listening|stats |book |latency )' "$log" || true
	rm -f "$log"
}

run_mode "skb+copy" --skb --copy --no-hugepage
run_mode "native+try-zc" 
echo "PASS: compared two RX modes on the same MD tape"
