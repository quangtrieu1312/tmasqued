#!/bin/bash
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh
OUT=$PWD/results-8core/direct.tsv; export OUT; : > "$OUT"
$SSH "$GW_SSH" 'cd ~/tmasqued && sudo docker compose down >/dev/null 2>&1'   # XDP off
bash stack.sh route_all fabric >/dev/null
bash target_servers.sh >/dev/null
for mtu in 9000 1500; do for proto in tcp udp; do for scen in 1up 1down half allup alldown; do
  bash measure_retry.sh direct direct "$mtu" "$proto" "$scen"
done; done; done
echo "[direct] DONE -> $OUT"
