#!/bin/bash
# Core measurement primitive (fleet-agnostic; per-core gateway CPU).
# Assumes the stack (direct/wg/tmasque), MTU and NIC regime are ALREADY configured.
# Usage: FLEET=fleet-8core.sh measure.sh <stack> <regime> <mtu> <proto> <scenario>
#   scenario: 1up 1down half allup alldown
# Emits one TSV row: stack regime mtu proto scen aggG cores_sum cores_max loss detail percore
set -u
cd "$(dirname "$0")"; source "${FLEET:-./fleet-8core.sh}"
STACK="$1" REGIME="$2" MTU="$3" PROTO="$4" SCEN="$5"
DUR=10; OMIT=2
N=${#CLIENTS[@]}

# --- which client indices upload / download for this scenario (adapts to client count N) ---
ups=(); downs=()
case "$SCEN" in
  1up)     ups=(0) ;;
  1down)   downs=(0) ;;
  allup)   for ((i=0;i<N;i++)); do ups+=($i); done ;;
  alldown) for ((i=0;i<N;i++)); do downs+=($i); done ;;
  half)    h=$((N/2)); for ((i=0;i<h;i++)); do ups+=($i); done; for ((i=h;i<N;i++)); do downs+=($i); done ;;
  *) echo "bad scenario $SCEN" >&2; exit 2 ;;
esac

# --- fresh, exactly-6 plain iperf3 -s -D servers per target each cell ---
# (respawn loops accumulate wedged servers under churn -> port conflicts -> NA; plain fresh is clean.
#  killing+restarting between cells also recovers any server a prior UDP-flood cell crashed.)
for t in "${TARGETS_SSH[@]}"; do
  $SSH "$t" 'for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null | grep -q ":$p " || { iperf3 -s -p $p -D >/dev/null 2>&1; sleep 0.4; }; done' >/dev/null 2>&1 &
done
wait; sleep 1

# --- iperf3 client command (uses that client's own target+port) ---
client_cmd() { # tgt_ip port dir
  local tip="$1" port="$2" dir="$3" extra="" size=""
  [ "$dir" = down ] && extra="-R"
  if [ "$STACK" = direct ]; then
    if [ "$PROTO" = tcp ]; then size="-M $((MTU-40))"; else size="-l $((MTU-28))"; fi
  fi
  if [ "$PROTO" = tcp ]; then
    echo "iperf3 -c $tip -p $port -t $DUR -O $OMIT -P 8 $extra $size -f m"
  else
    echo "iperf3 -c $tip -p $port -t $DUR -O $OMIT -u -b 1G -P 2 $extra $size -f m"
  fi
}

tmp=$(mktemp -d)
# --- start GW per-core CPU sampler (LC_ALL=C: avoid comma-decimal locale corrupting the math) ---
$SSH "$GW_SSH" "LC_ALL=C mpstat -P ALL 1 $DUR" > "$tmp/gw.mpstat" 2>/dev/null &
GWPID=$!

launch() { # idx dir
  local idx="$1" dir="$2" c="${CLIENTS[$1]}"
  set -- $c
  local dest="$1" tip="$8" port="$9"
  $SSH "$dest" "$(client_cmd "$tip" "$port" "$dir")" > "$tmp/c${idx}.${dir}" 2>"$tmp/c${idx}.err" &
}
for i in "${ups[@]}";   do launch "$i" up;   done
for i in "${downs[@]}"; do launch "$i" down; done
wait

# --- parse receiver Mbit/s per client; aggregate; UDP loss avg ---
agg=0; loss_sum=0; loss_n=0; detail=""
collect() { # idx dir
  local idx="$1" dir="$2"; local f="$tmp/c${idx}.${dir}" m
  [ -s "$f" ] || { detail+="c$idx:NA "; return; }
  # unit-anchored receiver rate; iperf3 connect/errors print to stdout (this file) WITHOUT a
  # [SUM]..receiver line -> exit 1 -> record NA (not a false 0.00 that would look "clean").
  m=$(awk '/\[SUM\]/ && /receiver/ {for(i=1;i<=NF;i++) if($i=="Mbits/sec"){v=$(i-1); found=1}} END{if(!found)exit 1; print v+0}' "$f") \
    || { detail+="c$idx:NA "; return; }
  if [ "$PROTO" = udp ]; then
    local l=$(awk '/\[SUM\]/ && /receiver/ {for(i=1;i<=NF;i++) if($i ~ /^\([0-9.]+%\)$/){gsub(/[()%]/,"",$i);print $i;exit}}' "$f")
    [ -n "$l" ] && { loss_sum=$(awk "BEGIN{print $loss_sum+$l}"); loss_n=$((loss_n+1)); }
  fi
  agg=$(awk "BEGIN{print $agg+$m}")
  detail+="c$idx:$(awk "BEGIN{printf \"%.2f\",$m/1000}") "
}
for i in "${ups[@]}";   do collect "$i" up;   done
for i in "${downs[@]}"; do collect "$i" down; done

# --- gateway per-core CPU: sum of cores busy, peak single-core %, per-core CSV ---
wait $GWPID 2>/dev/null
read gwsum gwmax gwpc < <(awk '
  /^Average:/ && $2 ~ /^[0-9]+$/ { n++; busy=100-$NF; sum+=busy/100; if(busy>mx)mx=busy; pc=pc sprintf("%.0f|",busy) }
  END { if(n==0){print "NA NA NA"; exit} printf "%.2f %.0f %s", sum, mx, pc }
' "$tmp/gw.mpstat")
gwpc=${gwpc%|}

agg_g=$(awk "BEGIN{printf \"%.2f\", $agg/1000}")
lossavg="-"; [ "$loss_n" -gt 0 ] && lossavg=$(awk "BEGIN{printf \"%.1f\", $loss_sum/$loss_n}")

row=$(printf "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s" \
  "$STACK" "$REGIME" "$MTU" "$PROTO" "$SCEN" "$agg_g" "${gwsum:-NA}" "${gwmax:-NA}" "$lossavg" "$detail" "${gwpc:-}")
echo "$row"
[ -n "${OUT:-}" ] && echo "$row" >> "$OUT"
rm -rf "$tmp"
