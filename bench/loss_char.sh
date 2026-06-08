#!/bin/bash
# Single-flow loss/reorder characterization (2-core, jumbo). Enables ENABLE_STATISTIC,
# restarts the gw, runs a 30s single-client 8-stream flow each direction, and samples the
# gateway [STATISTIC] line (pre-reseq ooo% = upload reorder at server RX; dg-packer/pre-send
# retr% = retransmit pressure). Throughput here is slightly pessimistic (stats observers on).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh
set -- ${CLIENTS[0]}; C0="$1"; C0_IP="$2"; C0_SU="$5"; TGT_IP="$8"; PORT="$9"
DUR=30

statline(){ $SSH "$GW_SSH" "C=\$(doas docker ps -q|head -1); doas docker logs --tail 40 \$C 2>&1 | grep STATISTIC | tail -1" | grep -oE 'pre-reseq:[^|]*\| pre-send:[^|]*\| dg-packer:.*' ; }

echo "### 1. enable ENABLE_STATISTIC + restart gw (jumbo/rss)"
$SSH "$GW_SSH" "doas sed -i 's/^ENABLE_STATISTIC=.*/ENABLE_STATISTIC=true/' ~/tmasqued/tmasqued.conf; grep ENABLE_STATISTIC ~/tmasqued/tmasqued.conf"
bash stack.sh gw_tmasque_config rss 9000

echo "### 2. clients up"
bash stack.sh tmasque_up_all | tail -3

echo "### 3. confirm STATISTIC emitting"
sleep 3; echo -n "  statline: "; statline || echo "(none yet)"

# fresh iperf3 sinks on the target
$SSH "$TGT_IP" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null | grep -q \":\$p \" || iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1; sleep 1

for dir in up down; do
  flag=""; [ "$dir" = down ] && flag="-R"
  echo; echo "### 4.$dir  single-client 8-stream ${DUR}s ($dir)"
  echo "  BEFORE: $(statline)"
  $SSH "$C0" "iperf3 -c $TGT_IP -p $PORT -t $DUR -O2 -P8 $flag -f m 2>/dev/null | tail -4" > /tmp/lc_$dir.out 2>&1 &
  IPF=$!
  for s in 1 2 3; do sleep 8; echo "  t=$((s*8))s: $(statline)"; done
  wait $IPF
  echo "  AFTER:  $(statline)"
  echo "  RATE: $(grep -E 'SUM|receiver' /tmp/lc_$dir.out | grep receiver | tail -1)"
done
echo; echo "### NOTE: ENABLE_STATISTIC left ON. Set back to false (sed) before final benchmark numbers."