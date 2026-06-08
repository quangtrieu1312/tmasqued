#!/bin/bash
# 6.17 re-baseline: forward all-up (6-client agg), default vs gso, regime none + rss.
# TAG (env) labels the run (copy|zc). Installs the tun+ FORWARD ACCEPT (Docker resets
# FORWARD=DROP on (re)start, and the GSO leg traverses the kernel FORWARD chain).
# Compares apples-to-apples with the 6.8 numbers. Restores standing gso/rss deploy at end.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
TAG="${TAG:-copy}"; TGT_IP=198.18.5.80; P="${P:-4}"; DUR="${DUR:-15}"
OUT=/tmp/rebaseline617_$TAG.out; : > "$OUT"
log(){ echo "$@" | tee -a "$OUT"; }

fwd_accept(){ $SSH "$GW_SSH" "sudo iptables -C FORWARD -i tun+ -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD 1 -i tun+ -j ACCEPT; sudo iptables -C FORWARD -o tun+ -j ACCEPT 2>/dev/null || sudo iptables -I FORWARD 1 -o tun+ -j ACCEPT" >/dev/null 2>&1; }

deploy(){ # $1=mode(default|gso) $2=combined $3=rps(ff|00)
  local mode="$1" comb="$2" rps="$3" extra="" gso=false
  [ "$mode" = gso ] && { extra=$'      - FORWARD_UPLOAD_RESEQ=1'; gso=true; }
  $SSH "$GW_SSH" "cat > ~/tmasqued/docker-compose.yml <<'YAML'
services:
  tmasqued:
    build: .
    cap_add: [NET_ADMIN, NET_RAW, SYS_ADMIN]
    privileged: true
    network_mode: host
    environment:
      - GOTRACEBACK=all
${extra}
    devices:
      - /dev/net/tun:/dev/net/tun
      - /dev/vhost-net:/dev/vhost-net
    volumes:
      - ./certs:/etc/tmasqued/certs
      - ./ca:/etc/tmasqued/ca
      - ./tmasqued.conf:/etc/tmasqued/tmasqued.conf
      - db:/etc/tmasqued/data
      - /sys/fs/bpf:/sys/fs/bpf
    ulimits:
      memlock: {soft: -1, hard: -1}
    command: /run.sh
volumes:
    db:
YAML
  cd ~/tmasqued
  sed -i '/^FORWARD_TUN_GSO=/d;/^FORWARD_TUN_VHOST=/d' tmasqued.conf; printf 'FORWARD_TUN_GSO=%s\nFORWARD_TUN_VHOST=false\n' $gso >> tmasqued.conf
  sudo docker compose down >/dev/null 2>&1
  sudo ip link set $GW_IF mtu 3506 2>/dev/null
  sudo ethtool -L $GW_IF combined $comb 2>/dev/null
  sudo docker compose up -d ${REBUILD:+--build} >/dev/null 2>&1
  sleep 16
  echo $rps | sudo tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1"
  fwd_accept
}

ensure(){ local dest="$1" su="$2"
  $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 15); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 2; done; return 1
}
rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }

measure(){ # $1=label
  local i tot=0 line="" up=0; local -a isup=()
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  i=0; for c in "${CLIENTS[@]}"; do set -- $c; if ensure "$1" "$5"; then isup[$i]=1; up=$((up+1)); else isup[$i]=0; log "    !! $1 tunnel down (excluded)"; fi; i=$((i+1)); done
  $SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/rb_gw &
  # only measure clients whose tunnel is actually up (else iperf3 goes DIRECT and poisons agg)
  i=0; for c in "${CLIENTS[@]}"; do set -- $c; if [ "${isup[$i]}" = 1 ]; then $SSH "$1" "iperf3 -c $TGT_IP -p $9 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/rb_$i & else : >/tmp/rb_$i; fi; i=$((i+1)); done
  wait
  i=0; for c in "${CLIENTS[@]}"; do local r=$(rate /tmp/rb_$i); tot=$(awk "BEGIN{print $tot+$r}"); line="$line c$i=$r"; i=$((i+1)); done
  local gw=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 8*(100-$NF)/100}' /tmp/rb_gw)
  local ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  local aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
  log "$(printf '  %-22s AGG=%6.2fG  gw=%s/8  OFO=%s  up=%d/6  [%s ]' "$1" "$aggG" "$gw" "$ofo" "$up" "$line")"
}

log "=== 6.17 re-baseline TAG=$TAG (fwd all-up, P=$P, $(date -u '+%F %H:%M')) ==="
for reg in "none 1 00" "rss 8 ff"; do
  set -- $reg; rname="$1"; comb="$2"; rps="$3"
  for mode in default gso; do
    log "--- $rname / $mode (combined=$comb rps=$rps) ---"
    deploy "$mode" "$comb" "$rps"
    measure "$rname:$mode"
  done
done
log "=== restoring standing deploy: gso + rss(combined=8) ==="
deploy gso 8 ff >/dev/null 2>&1
log "=== DONE TAG=$TAG ==="
