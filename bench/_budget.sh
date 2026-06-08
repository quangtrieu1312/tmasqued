#!/bin/bash
cd "$(dirname "$0")"; source ./fleet-2core.sh
C0=alpine@198.18.5.7
echo "=== RX IRQ affinity (25,27) + which cores softirq runs on ==="
$SSH "$GW_SSH" 'for i in 25 27; do echo "irq$i -> cpu $(cat /proc/irq/$i/smp_affinity_list 2>/dev/null)"; done'
# time_squeeze = softnet_stat col3 (hex), summed across cpus
squeeze() { $SSH "$GW_SSH" 'awk "{s=s+0; n=strtonum(\"0x\"\$3)} {tot+=n} END{}" /proc/net/softnet_stat 2>/dev/null; awk "{print \$3}" /proc/net/softnet_stat'; }
sqsum() { # arg: gw cmd output of hex lines -> decimal sum via bash
  local s=0 h; for h in $($SSH "$GW_SSH" 'awk "{print \$3}" /proc/net/softnet_stat'); do s=$((s + 16#$h)); done; echo $s
}
for cfg in "300 2000" "1500 8000" "3000 16000"; do
  budget=${cfg% *}; usecs=${cfg#* }
  $SSH "$GW_SSH" "doas sh -c 'echo $budget > /proc/sys/net/core/netdev_budget; echo $usecs > /proc/sys/net/core/netdev_budget_usecs'"
  b=$(sqsum)
  up=$($SSH "$C0" 'iperf3 -c 198.18.4.65 -p 5201 -t 8 -P1 2>/dev/null | awk "/sender/{print \$7,\$8}"')
  a=$(sqsum)
  echo "netdev_budget=$budget/$usecs  up=$up  time_squeeze +$((a-b))/8s"
done
