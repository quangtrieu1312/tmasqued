#!/bin/bash
# Does CLIENT GRO (TUN_GSO=true: IFF_VNET_HDR + TUNSETOFFLOAD on the client TUN, kernel
# coalesces app-TCP into GSO super-frames per read on the UPLOAD path) help?
# A/B with the SERVER HELD FIXED at default AF_XDP forward + RSS (combined=8) so the only
# variable is client-side read coalescing. 6 clients, all-up TCP jumbo, P-sweep 4/8.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh

OUT=/tmp/clientgro.out; : > "$OUT"
TGT_IP=198.18.5.80
PSWEEP=(4 8)
DUR=${DUR:-15}
log(){ echo "$@" | tee -a "$OUT"; }
rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }

measure(){ # $1=label $2=P
  local label="$1" P="$2" i tot=0 line=""
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  $SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/cg_gw &
  i=0
  for c in "${CLIENTS[@]}"; do set -- $c; local dest="$1" port="$9"
    $SSH "$dest" "iperf3 -c $TGT_IP -p $port -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/cg_c$i &
    i=$((i+1))
  done
  wait
  i=0
  for c in "${CLIENTS[@]}"; do local r=$(rate /tmp/cg_c$i); tot=$(awk "BEGIN{print $tot+$r}"); line="$line c$i=$r"; i=$((i+1)); done
  local ofo gw aggG
  ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  gw=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 8*(100-$NF)/100}' /tmp/cg_gw)
  aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
  log "$(printf '  %-14s P=%-3s AGG=%6.2f G  gw=%s/8 cores  tgt-OFO=%s  [%s ]' "$label" "$P" "$aggG" "$gw" "$ofo" "$line")"
}

# set TUN_GSO=true|false in every client's conf, then force-restart tmasque & wait for tunnel
set_gso_restart(){ # $1=true|false
  local val="$1" c dest su
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"
    $SSH "$dest" "$su sh -c 'sed -i /^TUN_GSO=/d /etc/tmasque/tmasque.conf; echo TUN_GSO=$val >> /etc/tmasque/tmasque.conf'" >/dev/null 2>&1
  done
  # force kill + relaunch all clients (config only read at startup)
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"
    $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done'" >/dev/null 2>&1 &
  done; wait
  sleep 2
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"
    $SSH "$dest" "$su sh -c 'modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1 &
  done; wait
  # wait for all routes
  local up=0 tries=0
  while [ $tries -lt 16 ]; do
    up=0
    for c in "${CLIENTS[@]}"; do set -- $c
      $SSH "$1" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && up=$((up+1))
    done
    [ $up -ge 6 ] && break; tries=$((tries+1)); sleep 3
  done
  log "  [TUN_GSO=$val] clients up: $up/6  (sample conf: $($SSH "$(set -- ${CLIENTS[4]}; echo $1)" "grep TUN_GSO /etc/tmasque/tmasque.conf"))"
}

log "=== client GRO (TUN_GSO) A/B — server FIXED default-AFXDP + RSS, 6 clients all-up TCP jumbo, $(date -u '+%H:%M') ==="
# pin server: default forward (no FORWARD_ flag) + RSS combined=8, rps off
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
  ${GW_SUDO} ethtool -L $GW_IF combined ${GW_RSS_COMBINED} 2>/dev/null
  ${GW_SUDO} docker compose up -d >/dev/null 2>&1
  sleep 14
  echo 00 | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
"
log "  server: default AF_XDP forward, combined=$($SSH "$GW_SSH" "ethtool -l $GW_IF|awk '/Current/{f=1}f&&/Combined/{print \$2;exit}'") rps=$($SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/queues/rx-0/rps_cpus")"

log "--- A: client TUN_GSO=false (baseline) ---"
set_gso_restart false
for P in "${PSWEEP[@]}"; do measure "gro-OFF" "$P"; done

log "--- B: client TUN_GSO=true (client GRO on) ---"
set_gso_restart true
for P in "${PSWEEP[@]}"; do measure "gro-ON" "$P"; done

log "=== restoring: client TUN_GSO=false + server vhost + RSS ==="
set_gso_restart false >/dev/null 2>&1
$SSH "$GW_SSH" "cp ~/tmasqued/docker-compose.yml.fwdcapbak ~/tmasqued/docker-compose.yml; cd ~/tmasqued; ${GW_SUDO} docker compose up -d >/dev/null 2>&1; sleep 8"
for c in "${CLIENTS[@]}"; do set -- $c; $SSH "$1" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" || $SSH "$1" "$5 sh -c 'modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1; done
log "=== DONE ==="
