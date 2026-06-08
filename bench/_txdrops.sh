#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7
echo "=== eth0 link stats BEFORE ==="
$SSH "$GW_SSH" 'doas ip -s -s link show eth0 | sed -n "/TX:/,+3p"'
$SSH "$GW_SSH" 'doas tc -s qdisc show dev eth0 2>/dev/null | grep -iE "dropped|overlimit|qdisc" | head'
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 4 -P1 2>/dev/null | awk "/sender/"'
sleep 1
echo "=== eth0 link stats AFTER ==="
$SSH "$GW_SSH" 'doas ip -s -s link show eth0 | sed -n "/TX:/,+3p"'
$SSH "$GW_SSH" 'doas tc -s qdisc show dev eth0 2>/dev/null | grep -iE "dropped|overlimit" | head'
echo "=== ethtool -S tx drops/errors ==="
$SSH "$GW_SSH" 'doas ethtool -S eth0 2>/dev/null | grep -iE "drop|error|tx_" | grep -vE ": 0$" | head -20'
