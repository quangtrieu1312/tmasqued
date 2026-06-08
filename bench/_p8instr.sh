#!/bin/bash
# Instrumented probe on the STANDING deploy (gso/rss/combined=8). Separates LOSS (Retr) from
# REORDER (UDP out-of-order). Fixed: no label clobber, robust parse, gentle non-crashing UDP.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
TGT_IP=198.18.5.80; P=8; DUR="${DUR:-15}"
ALL=( "ubuntu@198.18.0.122 sudo" "alpine@198.18.0.113 doas" "alpine@198.18.5.7 doas" "alpine@198.18.2.75 doas" "alpine@198.18.4.65 doas" "alpine@198.18.3.140 doas" )
ensure(){ local dest="$1" su="$2"
  $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 15); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 2; done; return 1
}
recv_rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }
send_retr(){ awk '/SUM/&&/sender/{print $(NF-1)+0}' "$1"; }
i=0; for c in "${ALL[@]}"; do set -- $c; ensure "$1" "$2" || echo "down c$i $1"; i=$((i+1)); done
$SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
sleep 3   # let tunnels settle

tcp_cell(){ local lbl="$1" dir="$2"; local R=""; [ "$dir" = d ] && R="-R"
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  local i=0; for c in "${ALL[@]}"; do local dest=${c%% *}; $SSH "$dest" "iperf3 -c $TGT_IP -p $((5201+i)) -t $DUR -O2 -P$P $R 2>/dev/null" >/tmp/pi_$i & i=$((i+1)); done
  wait
  local tot=0 retr=0 per=""
  i=0; for c in "${ALL[@]}"; do local r=$(recv_rate /tmp/pi_$i); local rt=$(send_retr /tmp/pi_$i)
    tot=$(awk "BEGIN{print $tot+$r}"); retr=$((retr+${rt:-0})); per="$per ${r%.*}"; i=$((i+1)); done
  local ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  printf "  %-12s AGG=%6.2fG  Retr=%-8d OFO=%-6s  per:[%s ]\n" "$lbl" "$(awk "BEGIN{print $tot/1000}")" "$retr" "$ofo" "$per"
}

# UDP: gentle 800M x2 streams/client = 1.6G offered/client (~9.6G total, near capacity, low loss),
# -P2 to avoid the -P8 segfault. Reports agg, loss%, and OOO datagrams (the real reorder signal).
udp_cell(){ local lbl="$1"
  local i=0; for c in "${ALL[@]}"; do local dest=${c%% *}; $SSH "$dest" "iperf3 -c $TGT_IP -p $((5201+i)) -t $DUR -O2 -P2 -u -b800M 2>/dev/null" >/tmp/pu_$i & i=$((i+1)); done
  wait
  local tot=0 lost=0 total=0 ooo=0
  i=0; for c in "${ALL[@]}"; do
    local line=$(grep -E 'SUM.*receiver' /tmp/pu_$i | tail -1)
    local r=$(echo "$line" | awk '{for(j=1;j<=NF;j++)if($j=="Mbits/sec")print $(j-1)}')
    tot=$(awk "BEGIN{print $tot+${r:-0}}")
    local o=$(grep -oiE '[0-9]+ \(.*received out-of-order' /tmp/pu_$i | grep -oE '^[0-9]+' | head -1)
    [ -z "$o" ] && o=$(grep -oiE 'OOO/Total.*' /tmp/pu_$i | head -1)
    ooo=$((ooo+${o:-0}))
    local lt=$(echo "$line" | grep -oE '[0-9]+/[0-9]+' | head -1); local l=${lt%/*}; local t=${lt#*/}
    lost=$((lost+${l:-0})); total=$((total+${t:-0})); i=$((i+1)); done
  local lp=$(awk "BEGIN{if($total>0)printf \"%.1f%%\",100*$lost/$total; else print 0}")
  printf "  %-12s AGG=%6.2fG  loss=%-6s OOO_datagrams=%d (of %d)\n" "$lbl" "$(awk "BEGIN{print $tot/1000}")" "$lp" "$ooo" "$total"
}

echo "=== INSTRUMENTED v2 (rss/gso, 6.17 clean fabric, P8 tcp / P2-b800M udp) ==="
tcp_cell "TCP allup"   u
tcp_cell "TCP alldown" d
udp_cell "UDP allup"
echo "=== DONE-INSTR ==="
