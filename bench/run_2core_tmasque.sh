#!/bin/bash
# 2-core tmasque: tmasqued container on Alpine gw (XDP-native), regime sweep (none/rps/rss=Combined2).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
OUT=$PWD/results-2core/tmasque.tsv; export OUT; touch "$OUT"
mkdir -p results-2core
for mtu in 9000 1500; do for regime in none rps rss; do
  echo "[2core-tmq] --- regime $regime mtu $mtu ---"
  gate; bash stack.sh gw_tmasque_config "$regime" "$mtu"
  gate; bash stack.sh tmasque_up_all
  for proto in tcp udp; do for scen in 1up 1down half allup alldown; do
    gate; bash stack.sh tmasque_ensure_healthy
    gate; bash measure_retry.sh tmasque "$regime" "$mtu" "$proto" "$scen"
  done; done
done; done
echo "[2core-tmq] DONE -> $OUT"
