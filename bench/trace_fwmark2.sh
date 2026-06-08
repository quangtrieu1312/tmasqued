#!/bin/bash
# Clean control-plane test. 5 Alpine clients saturate the tmasque tunnel; cn1 runs ONE LIGHT test
# (-b200M, won't starve its own mesh). Compare cn1 success with its control TCP on-tunnel vs direct.
# wg0 is downed on cn1 so 'direct' = fabric (ens3), not the leftover WireGuard mesh.
set -u
cd "$(dirname "$0")"; source ./fleet.sh
CN1=ubuntu@198.18.0.122; FWMARK=0x250c
ALP=("alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" "alpine@198.18.2.75 5204" \
     "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206")

del_mark(){ timeout 8 $SSH $CN1 "sudo iptables -t mangle -D OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK 2>/dev/null; true" >/dev/null 2>&1; }
add_mark(){ timeout 8 $SSH $CN1 "sudo iptables -t mangle -A OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK" >/dev/null 2>&1; }
flood_bg(){ for cp in "${ALP[@]}"; do set -- $cp; timeout 30 $SSH "$1" "iperf3 -c $TARGET -p $2 -t20 -u -b1G -P2 >/dev/null 2>&1" & done; }
cn1_test(){
  local out=$(timeout 22 $SSH $CN1 "iperf3 -c $TARGET -p 5201 -t8 -O2 -u -b200M -P2 -f m" 2>&1)
  local sum=$(echo "$out" | grep -E '\[SUM\].*receiver' | grep -oE '[0-9.]+ Mbits/sec' | head -1)
  local err=$(echo "$out" | grep -iE 'error|refused|unable|busy' | head -1 | cut -c1-55)
  echo "    cn1: [${sum:-FAIL${err:+: $err}}]"
}

bash target_servers.sh >/dev/null 2>&1
timeout 8 $SSH $CN1 "sudo ip link set wg0 down" >/dev/null 2>&1
echo "marked route = $(timeout 8 $SSH $CN1 "ip route get $TARGET mark $FWMARK 2>/dev/null | grep -oE 'dev [a-z0-9]+' | head -1")   cn1 reachable(no flood)=$(timeout 6 $SSH $CN1 echo OK 2>/dev/null || echo NO)"

echo "=== A) cn1 control via TUNNEL (unmarked) during 5-alpine flood — 2 reps ==="
del_mark
for r in 1 2; do flood_bg; sleep 4; cn1_test; wait; sleep 2; done
echo "=== B) cn1 control DIRECT (marked, fabric) during 5-alpine flood — 2 reps ==="
add_mark
for r in 1 2; do flood_bg; sleep 4; cn1_test; wait; sleep 2; done
del_mark
timeout 8 $SSH $CN1 "sudo ip link set wg0 up" >/dev/null 2>&1
echo "(mark removed, wg0 restored)"
