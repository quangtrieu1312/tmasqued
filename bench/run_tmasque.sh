#!/bin/bash
# tmasque block: 3 regimes x 2 MTU x 2 proto x 5 scenarios = 60 cells.
# Each (mtu,regime) reconfigures the GW (container restart to detach/reattach XDP at the new
# NIC combined + MTU) and restarts the clients (so they re-derive inner MTU and reconnect).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
OUT=$PWD/results-8core/tmasque.tsv; export OUT; touch "$OUT"

for mtu in 9000 1500; do
  echo "[tmq] ===== WAN $mtu ====="
  bash stack.sh client_wan_mtu "$mtu"
  for regime in rss none rps; do
    echo "[tmq] --- regime $regime / mtu $mtu ---"
    gate; bash stack.sh gw_tmasque_config "$regime" "$mtu"
    bash stack.sh tmasque_up_all
    for proto in tcp udp; do
      for scen in 1up 1down half allup alldown; do
        bash stack.sh tmasque_ensure_healthy
        bash measure_retry.sh tmasque "$regime" "$mtu" "$proto" "$scen"
      done
    done
  done
done
echo "[tmq] DONE -> $OUT"
