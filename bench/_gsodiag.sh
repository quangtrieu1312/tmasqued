#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
GW=alpine@198.18.0.113; TGT=alpine@198.18.4.65; C0=alpine@198.18.5.7
echo "=== gw GSO counters BEFORE ==="
$SSH "$GW" 'wget -qO- http://127.0.0.1:6060/debug/vars 2>/dev/null | tr , "\n" | grep -iE "gso_"'
$SSH "$TGT" 'nstat -n >/dev/null 2>&1'
B_RX=$($SSH "$TGT" 'cat /sys/class/net/eth0/statistics/rx_packets')
B_RXD=$($SSH "$TGT" 'cat /sys/class/net/eth0/statistics/rx_dropped')
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 6 -P1 2>/dev/null | awk "/sender/"'
A_RX=$($SSH "$TGT" 'cat /sys/class/net/eth0/statistics/rx_packets')
A_RXD=$($SSH "$TGT" 'cat /sys/class/net/eth0/statistics/rx_dropped')
echo "=== gw GSO counters AFTER ==="
$SSH "$GW" 'wget -qO- http://127.0.0.1:6060/debug/vars 2>/dev/null | tr , "\n" | grep -iE "gso_"'
echo "=== target eth0 rx delta: pkts=$((A_RX-B_RX)) dropped=$((A_RXD-B_RXD)) ==="
echo "=== target nstat (Tcp/Ip/Csum errors+segs) ==="
$SSH "$TGT" 'nstat 2>/dev/null | grep -iE "InSegs|InReceives|HdrErr|Discard|Csum|PAWS|Drop|Reasm|InErrs"'
