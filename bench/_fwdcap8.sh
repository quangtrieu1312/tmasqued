#!/bin/bash
# Forward-path A/B/C cap probe on the 8-core fleet (6 clients -> single target).
# Same idea as _fwdcap.sh but: GW=198.18.0.130 (8c ubuntu/sudo, ens3, rss Combined=8),
# 6 clients each uploading to 198.18.5.80 on its own port (5201..5206). Pushes aggregate
# forward (= upload) to find the 8-core GW ceiling. Reports agg Gbit, GW core-equivalents
# (of 8), target inner-TCP OFO%. tunnel inner 3422 (WAN eff 3506) / tcp / rss.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh

OUT=/tmp/fwdcap8.out; : > "$OUT"
TGT_IP=198.18.5.80
PSWEEP=(4 8)
DUR=${DUR:-15}

log(){ echo "$@" | tee -a "$OUT"; }

deploy(){ # $1=mode (default|vhost|gso)
  local mode="$1" extra=""
  case "$mode" in
    vhost) extra=$'      - FORWARD_TUN_VHOST=1\n      - FORWARD_UPLOAD_RESEQ=1';;
    gso)   extra=$'      - FORWARD_TUN_GSO=1\n      - FORWARD_UPLOAD_RESEQ=1';;
    default) extra="";;
  esac
  $SSH "$GW_SSH" "cat > ~/tmasqued/docker-compose.yml <<'YAML'
services:
  tmasqued:
    build: .
    cap_add:
      - NET_ADMIN
      - NET_RAW
      - SYS_ADMIN
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
      memlock:
        soft: -1
        hard: -1
    command: /run.sh
volumes:
    db:
YAML
    cd ~/tmasqued
    ${GW_SUDO} docker compose down >/dev/null 2>&1
    ${GW_SUDO} ip link set $GW_IF mtu 3506 2>/dev/null
    ${GW_SUDO} ethtool -L $GW_IF combined ${GW_RSS_COMBINED} 2>/dev/null
    ${GW_SUDO} docker compose up -d >/dev/null 2>&1
    sleep 14
    echo 00 | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
  "
}

# reconnect a client tunnel after gw restart; $1=ssh-dest $2=sudo
ensure(){
  local dest="$1" su="$2"
  $SSH "$dest" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 14); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 3; done
  return 1
}

rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }

verify(){ # log which alt device is live + env (best-effort; device is lazy until traffic)
  local devs env
  devs=$($SSH "$GW_SSH" "ip -o link show | grep -oE 'tmvhost[0-9]+|tmfwd[0-9]+|tun[0-9]+' | sort -u | tr '\n' ' '")
  env=$($SSH "$GW_SSH" "${GW_SUDO} docker exec \$(${GW_SUDO} docker ps -q|head -1) env 2>/dev/null | grep -E 'FORWARD_' | tr '\n' ' '")
  log "  [verify $1] env={ $env} devs(pre-traffic)={ $devs}"
}

measure(){ # $1=mode $2=P
  local mode="$1" P="$2" i pids="" sum=0
  # ensure target listeners
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  $SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/fc8_gw &
  i=0
  for c in "${CLIENTS[@]}"; do
    set -- $c; local dest="$1" port="$9"
    $SSH "$dest" "iperf3 -c $TGT_IP -p $port -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/fc8_$i &
    pids="$pids $!"; i=$((i+1))
  done
  wait
  local line="" tot=0
  i=0
  for c in "${CLIENTS[@]}"; do
    local r=$(rate /tmp/fc8_$i); tot=$(awk "BEGIN{print $tot+$r}")
    line="$line c$i=$r"; i=$((i+1))
  done
  local ofo gw aggG
  ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  # mpstat 'all' Average row: core-equivalents busy across 8 cores = 8*(100-idle)/100
  gw=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 8*(100-$NF)/100}' /tmp/fc8_gw)
  aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
  log "$(printf '  %-7s P=%-3s AGG=%6.2f G  gw=%s/8 cores  tgt-OFO=%s  [%s ]' "$mode" "$P" "$aggG" "$gw" "$ofo" "$line")"
}

log "=== 8-core forward-path cap probe (inner3422/tcp/rss8, 6 clients, $(date -u '+%Y-%m-%d %H:%M')) ==="
for mode in default vhost gso; do
  log "--- deploying mode=$mode ---"
  deploy "$mode"
  verify "$mode"
  # reconnect all 6 clients
  allok=1
  for c in "${CLIENTS[@]}"; do set -- $c; ensure "$1" "$5" || { log "  !! client $1 tunnel down"; allok=0; }; done
  [ $allok -ne 1 ] && log "  (continuing with whatever clients are up)"
  for P in "${PSWEEP[@]}"; do measure "$mode" "$P"; done
done
log "=== restoring baseline compose (vhost) ==="
$SSH "$GW_SSH" "cp ~/tmasqued/docker-compose.yml.fwdcapbak ~/tmasqued/docker-compose.yml; cd ~/tmasqued; ${GW_SUDO} docker compose up -d >/dev/null 2>&1"
log "=== DONE ==="
