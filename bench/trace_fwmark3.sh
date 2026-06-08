#!/bin/bash
# Capture cn1's FULL iperf3 output with control marked-direct during the flood, + verify the
# mangle rule actually counts packets (is the control even being marked?).
set -u
cd "$(dirname "$0")"; source ./fleet.sh
CN1=ubuntu@198.18.0.122; FWMARK=0x250c
ALP=("alpine@198.18.0.113 5202" "alpine@198.18.5.7 5203" "alpine@198.18.2.75 5204" \
     "alpine@198.18.4.65 5205" "alpine@198.18.3.140 5206")
bash target_servers.sh >/dev/null 2>&1
timeout 8 $SSH $CN1 "sudo ip link set wg0 down" >/dev/null 2>&1
timeout 8 $SSH $CN1 "sudo iptables -t mangle -D OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK 2>/dev/null; sudo iptables -t mangle -A OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK" >/dev/null 2>&1
echo "marked route = $(timeout 8 $SSH $CN1 "ip route get $TARGET mark $FWMARK 2>/dev/null|grep -oE 'dev [a-z0-9]+'|head -1")"
for cp in "${ALP[@]}"; do set -- $cp; timeout 30 $SSH "$1" "iperf3 -c $TARGET -p $2 -t18 -u -b1G -P2 >/dev/null 2>&1" & done
sleep 4
echo "=== cn1 FULL iperf3 output (control marked direct, data via tunnel) ==="
timeout 22 $SSH $CN1 "iperf3 -c $TARGET -p 5201 -t8 -O2 -u -b200M -P2 -f m 2>&1" | head -25
echo "=== mangle rule packet count (did control get marked?) ==="
timeout 8 $SSH $CN1 "sudo iptables -t mangle -L OUTPUT -v -n | grep -E 'MARK|pkts' | head"
wait
timeout 8 $SSH $CN1 "sudo iptables -t mangle -D OUTPUT -p tcp --dport 5201 -j MARK --set-mark $FWMARK 2>/dev/null; sudo ip link set wg0 up" >/dev/null 2>&1
echo "(restored)"
