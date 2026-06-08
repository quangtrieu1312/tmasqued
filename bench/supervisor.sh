#!/bin/bash
# Keep the 8-core run alive across wt0 drops / process death until it completes.
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh
while ! grep -q 'VPN blocks DONE' results-8core/vpn.run.log 2>/dev/null; do
  # match the run AND its measure children, so we never relaunch while work is still in flight
  if ! pgrep -f 'run_8core_vpn.sh|run_wg.sh|run_tmasque.sh|measure_retry.sh|bash measure.sh' >/dev/null 2>&1; then
    if timeout 8 $SSH "$GW_SSH" 'echo ok' >/dev/null 2>&1; then
      echo "[supervisor] run dead + wt0 up -> relaunching (resume)" >> results-8core/vpn.run.log
      TRIES=3 setsid flock -n /tmp/8core-run.lock bash run_8core_vpn.sh >> results-8core/vpn.run.log 2>&1 </dev/null &
    fi
  fi
  sleep 150
done
