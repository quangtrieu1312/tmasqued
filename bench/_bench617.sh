#!/bin/bash
# 6.17 tmasque matrix: TCP, WAN 9000 (inner 3422), GSO forward, P8.
# 5 scenarios x {none,rps,rss}. Emits parseable rows to /tmp/bench617.out.
# Restores standing gso/rss deploy at the end.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh
TGT_IP=198.18.5.80; P=8; DUR="${DUR:-15}"
OUT=/tmp/bench617.out; : > "$OUT"
log(){ echo "$@" | tee -a "$OUT"; }

deploy(){ # $1=combined $2=rps
  local comb="$1" rps="$2"
  $SSH "$GW_SSH" "cat > ~/tmasqued/docker-compose.yml <<'YAML'
services:
  tmasqued:
    build: .
    cap_add: [NET_ADMIN, NET_RAW, SYS_ADMIN]
    privileged: true
    network_mode: host
    environment:
      - GOTRACEBACK=all
      - FORWARD_UPLOAD_RESEQ=1
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
  sed -i '/^FORWARD_TUN_GSO=/d;/^FORWARD_TUN_VHOST=/d' tmasqued.conf; printf 'FORWARD_TUN_GSO=true\nFORWARD_TUN_VHOST=false\n' >> tmasqued.conf
  sudo docker compose down >/dev/null 2>&1
  sudo ip link set $GW_IF mtu 3506 2>/dev/null
  sudo ethtool -L $GW_IF combined $comb 2>/dev/null
  sudo docker compose up -d --build >/dev/null 2>&1
  sleep 16
  echo $rps | sudo tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1"
  # FORWARD ACCEPT for the GSO upload path (tun+) is installed by tmasque's postup; nothing to add here.
}
ensure(){ local dest="$1" su="$2"
  $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 15); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 2; done; return 1
}
rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }
# run iperf3 on a set of client indices; $1=label-of-direction-list as "idx:dir ..." dir=u|d
# returns agg Gbit via /tmp echo
flows(){ # args: list "idx dir" pairs
  local specs=("$@") pids="" i=0
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  $SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/b6_gw &
  for s in "${specs[@]}"; do
    set -- $s; local idx="$1" dir="$2"
    local c=(${CLIENTS[$idx]}); local dest="${c[0]}" port="${c[8]}"
    local R=""; [ "$dir" = d ] && R="-R"
    $SSH "$dest" "iperf3 -c $TGT_IP -p $port -t $DUR -O2 -P$P $R -f m 2>/dev/null" >/tmp/b6_$idx &
  done
  wait
  local tot=0
  for s in "${specs[@]}"; do set -- $s; local idx="$1"; local r=$(rate /tmp/b6_$idx); tot=$(awk "BEGIN{print $tot+$r}"); done
  GWCORES=$(awk '/^Average:/ && $2=="all"{printf "%.1f", 8*(100-$NF)/100}' /tmp/b6_gw)
  AGG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
}

scen(){ # $1=regime-name $2=scenario-name $3...=flow specs
  local reg="$1" name="$2"; shift 2
  flows "$@"
  log "ROW	$reg	$name	$AGG	$GWCORES"
}

ALL=( "ubuntu@198.18.0.122 sudo" "alpine@198.18.0.113 doas" "alpine@198.18.5.7 doas" "alpine@198.18.2.75 doas" "alpine@198.18.4.65 doas" "alpine@198.18.3.140 doas" )

for regspec in "none 1 00" "rps 1 ff" "rss 8 ff"; do
  set -- $regspec; reg="$1"; comb="$2"; rps="$3"
  log "=== regime $reg (combined=$comb rps=$rps) ==="
  deploy "$comb" "$rps"
  i=0; for c in "${ALL[@]}"; do set -- $c; ensure "$1" "$2" || log "  down c$i $1"; i=$((i+1)); done
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  scen "$reg" "1up"      "0 u"
  scen "$reg" "1down"    "0 d"
  scen "$reg" "half"     "0 u" "1 u" "2 u" "3 d" "4 d" "5 d"
  scen "$reg" "allup"    "0 u" "1 u" "2 u" "3 u" "4 u" "5 u"
  scen "$reg" "alldown"  "0 d" "1 d" "2 d" "3 d" "4 d" "5 d"
done
log "=== restore standing gso/rss ==="
deploy 8 ff >/dev/null 2>&1
log "=== DONE ==="
