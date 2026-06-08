#!/bin/bash
# 2-core WireGuard: Alpine gateway wg0 mesh + MASQUERADE, regime sweep (none/rps/rss=Combined2).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
OUT=$PWD/results-2core/wg.tsv; export OUT; touch "$OUT"
mkdir -p results-2core
# WG block needs XDP off (container down) for a clean kernel-WG baseline
gate; $SSH "$GW_SSH" 'cd ~/tmasqued && doas docker compose down >/dev/null 2>&1'
echo "[2core-wg] build clean WG mesh"; gate; bash setup_wg.sh
gate; bash stack.sh route_all wg0 >/dev/null
for regime in none rps rss; do
  echo "[2core-wg] --- regime $regime ---"
  gate; bash stack.sh gw_regime "$regime"; sleep 12
  gate; bash stack.sh route_all wg0 >/dev/null
  for mtu in 9000 1500; do
    gate; bash stack.sh wg_mtu "$mtu" >/dev/null
    for proto in tcp udp; do for scen in 1up 1down half allup alldown; do
      gate; bash stack.sh wg_ensure_healthy
      gate; bash measure_retry.sh wg "$regime" "$mtu" "$proto" "$scen"
    done; done
  done
done
echo "[2core-wg] DONE -> $OUT"
