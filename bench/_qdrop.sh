#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7; TGT=alpine@198.18.4.65
snap() { # node label
  $SSH "$1" "echo \"[$2] $3 tx_drop=\$(cat /sys/class/net/$3/statistics/tx_dropped) rx_drop=\$(cat /sys/class/net/$3/statistics/rx_dropped) rx_err=\$(cat /sys/class/net/$3/statistics/rx_errors) rx_miss=\$(cat /sys/class/net/$3/statistics/rx_missed_errors 2>/dev/null)\""
}
echo "=== BEFORE ==="
snap "$GW_SSH" gw eth0; snap "$GW_SSH" gw tun0; snap "$TGT" tgt eth0
$SSH "$GW_SSH" 'echo "softnet drops(col2) per cpu BEFORE:"; awk "{print \$2}" /proc/net/softnet_stat | tr "\n" " "; echo'
$SSH "$GW_SSH" 'doas tc -s qdisc show dev eth0 2>/dev/null | grep -oE "dropped [0-9]+|overlimits [0-9]+|requeues [0-9]+" | tr "\n" " "; echo " <- eth0 qdisc BEFORE"'
$SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 6 -P1 2>/dev/null | awk "/sender/{print \"up:\",\$7,\$8,\$9}"'
echo "=== AFTER ==="
snap "$GW_SSH" gw eth0; snap "$GW_SSH" gw tun0; snap "$TGT" tgt eth0
$SSH "$GW_SSH" 'echo "softnet drops(col2) per cpu AFTER:"; awk "{print \$2}" /proc/net/softnet_stat | tr "\n" " "; echo'
$SSH "$GW_SSH" 'doas tc -s qdisc show dev eth0 2>/dev/null | grep -oE "dropped [0-9]+|overlimits [0-9]+|requeues [0-9]+" | tr "\n" " "; echo " <- eth0 qdisc AFTER"'
echo "=== tun0 qdisc ==="
$SSH "$GW_SSH" 'doas tc -s qdisc show dev tun0 2>/dev/null | grep -oE "dropped [0-9]+|backlog [0-9a-z]+|qlen [0-9]+"'
echo "=== tun0 txqueuelen ==="
$SSH "$GW_SSH" 'cat /sys/class/net/tun0/tx_queue_len'
