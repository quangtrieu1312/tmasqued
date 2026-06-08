#!/bin/bash
# WireGuard block: reuse the live wg0 mesh (GW gateway + MASQUERADE).
# 3 regimes x 2 MTU x 2 proto x 5 scenarios = 60 cells.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
OUT=$PWD/results-8core/wg.tsv; export OUT; touch "$OUT"

gate; $SSH "$GW_SSH" 'cd ~/tmasqued && sudo docker compose down >/dev/null 2>&1'   # XDP off for clean WG
echo "[wg] build clean WG mesh"
bash setup_wg.sh
echo "[wg] route target via wg0 on all clients"
bash stack.sh route_all wg0

for regime in none rps rss; do
  echo "[wg] === regime $regime ==="
  gate; bash stack.sh gw_regime "$regime"
  sleep 20   # ethtool NIC reinit drops the wg0 underlay; let keepalive re-handshake
  bash stack.sh route_all wg0 >/dev/null   # re-assert target->wg0 routes
  for mtu in 9000 1500; do
    bash stack.sh wg_mtu "$mtu"
    for proto in tcp udp; do
      for scen in 1up 1down half allup alldown; do
        bash stack.sh wg_ensure_healthy
        bash measure_retry.sh wg "$regime" "$mtu" "$proto" "$scen"
      done
    done
  done
done
echo "[wg] DONE -> $OUT"
