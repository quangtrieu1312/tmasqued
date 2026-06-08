#!/bin/bash
# Trace the none/1500/udp multi-client 0.00 failure: snapshot client tmasque state,
# run the cell, then post-mortem (procs alive? tun up? logs).
set -u
cd "$(dirname "$0")"; source ./fleet.sh
SCEN="${1:-allup}"

snap() { # label
  echo "=== $1 ==="
  for c in "${CLIENTS[@]}"; do
    set -- $c; local dest="$1" ip="$2" su="$5"
    timeout 8 $SSH "$dest" 'p=""; for d in /proc/[0-9]*; do [ "$(readlink $d/exe 2>/dev/null)" = /usr/local/bin/tmasque ] && p="$p ${d#/proc/}"; done; echo "pid:[$p ] tun:$(ip -br link show 2>/dev/null | grep -oE "^tun[0-9]+" | tr "\n" ",") route:$(ip route get '"$TARGET"' 2>/dev/null | grep -oE "dev [a-z0-9]+" | head -1)"' 2>/dev/null | sed "s/^/  [$ip] /"
  done
}

snap "BEFORE"
echo "=== RUN none/1500 udp $SCEN ==="
OUT=/dev/null timeout 130 bash measure.sh tmasque none 1500 udp "$SCEN"
snap "AFTER"
echo "=== client tmasque log tails (last 8) ==="
for c in "${CLIENTS[@]}"; do
  set -- $c; dest="$1"; ip="$2"; su="$5"
  echo "  --- [$ip] ---"
  timeout 8 $SSH "$dest" "$su tail -8 /tmp/tmq-bench.log 2>/dev/null" 2>/dev/null | sed "s/^/    /"
done
echo "=== GW container log tail (errors during run) ==="
timeout 10 $SSH "$GW_SSH" 'sudo docker logs --since 3m $(sudo docker ps -q|head -1) 2>&1 | grep -iE "error|panic|fatal|drop|reset|fail" | tail -15'
