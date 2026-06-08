#!/bin/bash
# Measure the 5 rows against whatever stack is CURRENTLY up. Usage: _measure5.sh <label>
# Rows: 1up_P1, 1up_P8, half, allup, alldown. Appends ROW<tab>label<tab>row<tab>agg<tab>gwcores.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh
LBL="$1"; TGT=198.18.5.80; DUR="${DUR:-12}"; OUT=/tmp/measure5.out
DESTS=(); for c in "${CLIENTS[@]}"; do set -- $c; DESTS+=("$1"); done
PORTS=(5201 5202 5203 5204 5205 5206)
SSHO="ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o ConnectTimeout=8"
recv(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }
sretr(){ awk '/sender/{r=$(NF-1)}END{print r+0}' "$1"; }
$SSHO "$TARGET_SSH" "pkill -9 iperf3 2>/dev/null; sleep 2; for p in 5201 5202 5203 5204 5205 5206; do iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1; sleep 1
run_set(){ local P=$1; shift; local specs=("$@") s
  $SSHO "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/m5_gw &
  for s in "${specs[@]}"; do set -- $s; local idx=$1 dir=$2 R=""; [ "$dir" = d ] && R="-R"
    $SSHO "${DESTS[$idx]}" "iperf3 -c $TGT -p ${PORTS[$idx]} -t $DUR -O2 -P$P $R 2>/dev/null" >/tmp/m5_$idx & done
  wait; local tot=0; for s in "${specs[@]}"; do set -- $s; tot=$(awk "BEGIN{print $tot+$(recv /tmp/m5_$1)}"); done
  RT=0; for s in "${specs[@]}"; do set -- $s; RT=$((RT+$(sretr /tmp/m5_$1))); done; GW=$(awk '/^Average:/&&$2=="all"{printf "%.1f",8*(100-$NF)/100}' /tmp/m5_gw); AGG=$(awk "BEGIN{printf \"%.2f\",$tot/1000}"); }
out(){ echo "ROW	$LBL	$1	$AGG	$GW	$RT" | tee -a "$OUT"; }
run_set 1 "0 u";                               out 1up_P1
run_set 8 "0 u";                               out 1up_P8
run_set 8 "0 u" "1 u" "2 u" "3 d" "4 d" "5 d"; out half
run_set 8 "0 u" "1 u" "2 u" "3 u" "4 u" "5 u"; out allup
run_set 8 "0 d" "1 d" "2 d" "3 d" "4 d" "5 d"; out alldown
echo "DONE $LBL"
