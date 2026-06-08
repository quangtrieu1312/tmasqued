#!/bin/bash
# 2-core DIRECT baseline: clients -> target over the fabric (gateway NOT in path).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
OUT=$PWD/results-2core/direct.tsv; export OUT; touch "$OUT"
mkdir -p results-2core
# clean target servers, route each client's target via fabric
$SSH "$TARGET_SSH" 'doas pkill -9 iperf3 2>/dev/null' 2>/dev/null
gate; bash stack.sh route_all fabric >/dev/null
for mtu in 9000 1500; do for proto in tcp udp; do for scen in 1up 1down half allup alldown; do
  gate; bash measure_retry.sh direct direct "$mtu" "$proto" "$scen"
done; done; done
echo "[2core-direct] DONE -> $OUT"
