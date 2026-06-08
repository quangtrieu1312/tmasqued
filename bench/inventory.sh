#!/bin/bash
# Inventory stale state across the fleet (read-only).
set -u
cd "$(dirname "$0")"; source ./fleet.sh

probe() { # $1=ssh dest  $2=label  $3=sudo
  local s="$1" l="$2" su="$3"
  timeout 14 $SSH "$s" "
    echo \"wg:\$($su wg show 2>/dev/null | head -1 || echo none)\"
    echo \"vpnif:\$(ip -br link show 2>/dev/null | grep -iE 'wg0|tun0|utun' | tr '\n' ',' )\"
    echo \"tmasque:\$(pgrep -x tmasque >/dev/null 2>&1 && echo running || echo no)\"
    echo \"xtrarules:\$(ip rule show 2>/dev/null | grep -vE 'lookup (main|default|local)' | tr '\n' ';')\"
  " 2>&1 | sed "s/^/[$l] /"
}

probe "$GW_SSH" "GW" "sudo"
probe "$TARGET_SSH" "TGT" "sudo"
for c in "${CLIENTS[@]}"; do
  set -- $c
  probe "$1" "${2}" "$5"
done
# GW docker + NIC state
echo "--- GW datapath ---"
timeout 12 $SSH "$GW_SSH" 'echo "docker: $(docker ps --format "{{.Names}}" 2>/dev/null | tr "\n" "," )"; echo "combined: $(ethtool -l '"$GW_IF"' 2>/dev/null | awk "/Current/{f=1} f&&/Combined/{print \$2; exit}")"; echo "mtu: $(ip link show '"$GW_IF"' | grep -o "mtu [0-9]*")"; echo "rps: $(cat /sys/class/net/'"$GW_IF"'/queues/rx-0/rps_cpus)"'
