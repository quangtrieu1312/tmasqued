#!/bin/bash
# P8 saturation probe on the CURRENT standing deploy (gso/rss, no redeploy).
# Measures aggregate + GW cores + TARGET cores + per-client, to find what binds.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
TGT_IP=198.18.5.80; P="${P:-8}"; DUR="${DUR:-15}"
rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }
ensure(){ local dest="$1" su="$2"
  $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 15); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 2; done; return 1
}
up=0; declare -a isup=()
i=0; for c in "${CLIENTS[@]}"; do set -- $c; if ensure "$1" "$5"; then isup[$i]=1; up=$((up+1)); else isup[$i]=0; echo "  down: $1"; fi; i=$((i+1)); done
$SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
$SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
# GW + target cpu samplers
$SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/p8_gw &
$SSH "$TARGET_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/p8_tgt &
i=0; for c in "${CLIENTS[@]}"; do set -- $c; if [ "${isup[$i]}" = 1 ]; then $SSH "$1" "iperf3 -c $TGT_IP -p $9 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/p8_$i & else : >/tmp/p8_$i; fi; i=$((i+1)); done
wait
tot=0; line=""; i=0; for c in "${CLIENTS[@]}"; do r=$(rate /tmp/p8_$i); tot=$(awk "BEGIN{print $tot+$r}"); line="$line c$i=$r"; i=$((i+1)); done
gw=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 8*(100-$NF)/100}' /tmp/p8_gw)
tgt=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 4*(100-$NF)/100}' /tmp/p8_tgt)
ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
echo "P8 6-client all-up (gso/rss, 6.17): AGG=${aggG}G  GW=${gw}/8  TARGET=${tgt}/4  OFO=$ofo  up=$up/6"
echo "  per-client Mbit: [$line ]"
