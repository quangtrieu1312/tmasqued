#!/bin/bash
# Test the control-plane theory: route cn1's iperf3 CONTROL (TCP dport 5201) DIRECT (mark 0x250c
# skips tunnel table 9000), keep its UDP DATA on the tunnel. If cn1 stops failing while the GW still
# sees the UDP load, the 0.00 was the control TCP dying over the lossy tunnel (not tmasque datapath).
set -u
cd "$(dirname "$0")"; source ./fleet.sh
CN1=ubuntu@198.18.0.122
FWMARK=0x250c
CLIENTSET=("ubuntu@198.18.0.122 5201" "alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" \
           "alpine@198.18.2.75 5204" "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206")

del_mark(){ timeout 8 $SSH $CN1 "sudo iptables -t mangle -D OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK 2>/dev/null; true" >/dev/null 2>&1; }
add_mark(){ timeout 8 $SSH $CN1 "sudo iptables -t mangle -A OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK" >/dev/null 2>&1; }
gwrx(){ timeout 8 $SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/statistics/rx_bytes" 2>/dev/null; }

route_check(){
  local nom=$(timeout 8 $SSH $CN1 "ip route get $TARGET 2>/dev/null | grep -oE 'dev [a-z0-9]+' | head -1")
  local mrk=$(timeout 8 $SSH $CN1 "ip route get $TARGET mark $FWMARK 2>/dev/null | grep -oE 'dev [a-z0-9]+' | head -1")
  echo "  cn1 route to target: unmarked=[$nom]  marked($FWMARK)=[$mrk]"
}

allup(){ # one rep, reports cn1 + GW tunnel load
  local r0=$(gwrx); local tmp=$(mktemp -d) i=0
  for cp in "${CLIENTSET[@]}"; do set -- $cp
    timeout 30 $SSH "$1" "iperf3 -c $TARGET -p $2 -t10 -O2 -u -b1G -P2 -f m" >"$tmp/c$i" 2>&1 & i=$((i+1)); done
  wait; local r1=$(gwrx)
  local c0=$(grep -E '\[SUM\].*receiver' "$tmp/c0" 2>/dev/null | grep -oE '[0-9.]+ Mbits/sec' | head -1)
  local c0e=$(grep -iE 'refused|unable|error' "$tmp/c0" 2>/dev/null | head -1 | cut -c1-40)
  local ok=0; for n in 0 1 2 3 4 5; do grep -qE '\[SUM\].*receiver' "$tmp/c$n" && ok=$((ok+1)); done
  echo "  cn1(c0)=[${c0:-FAIL${c0e:+:$c0e}}]  clients_with_result=$ok/6  GW_rx_delta=$(( (r1-r0)/1000000 ))MB"
  rm -rf "$tmp"
}

del_mark
echo "=== drop leftover wg0 on cn1 so 'direct' = fabric (ens3), not WireGuard ==="
timeout 8 $SSH $CN1 "sudo ip link set wg0 down" >/dev/null 2>&1
echo "=== route check (baseline) ==="; route_check
echo "=== A) WITHOUT mark — cn1 control rides the tunnel (3 reps) ==="
for r in 1 2 3; do printf "  rep$r:"; allup; done
echo
echo "=== add mark: cn1 TCP/5201 -> DIRECT ==="; add_mark; route_check
echo "=== B) WITH mark — cn1 control direct, UDP data still tunnel (3 reps) ==="
for r in 1 2 3; do printf "  rep$r:"; allup; done
del_mark
timeout 8 $SSH $CN1 "sudo ip link set wg0 up" >/dev/null 2>&1
echo "(mark removed, wg0 restored)"
