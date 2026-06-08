#!/bin/bash
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh
while ! grep -q '\[2core\] VPN blocks DONE' results-2core/vpn.run.log 2>/dev/null; do
  if ! pgrep -f 'run_2core_vpn.sh|run_2core_wg.sh|run_2core_tmasque.sh|measure_retry.sh|bash measure.sh' >/dev/null 2>&1; then
    if timeout 8 $SSH "$GW_SSH" 'echo ok' >/dev/null 2>&1; then
      echo "[supervisor] run dead + reachable -> relaunching (resume)" >> results-2core/vpn.run.log
      TRIES=3 setsid flock -n /tmp/2core-run.lock bash run_2core_vpn.sh >> results-2core/vpn.run.log 2>&1 </dev/null &
    fi
  fi
  sleep 150
done
