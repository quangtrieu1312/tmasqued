#!/bin/bash
# Controlled same-session pass: tmasque(FORWARD_TUN_GSO) vs WireGuard, BOTH under the
# rps regime (combined=1, rps=ff) on the 8-core GW, 6 clients, all-up TCP, jumbo.
# Identical P-sweep (4,8) + duration so the comparison is apples-to-apples.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh

OUT=/tmp/gsovswg_rps.out; : > "$OUT"
TGT_IP=198.18.5.80
PSWEEP=(4 8)
DUR=${DUR:-15}
log(){ echo "$@" | tee -a "$OUT"; }
rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }

# --- shared 6-client all-up measurement (path determined by current routing) ---
measure(){ # $1=label $2=P
  local label="$1" P="$2" i tot=0 line=""
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  $SSH "$GW_SSH" "LC_ALL=C mpstat 1 $DUR 2>/dev/null" >/tmp/gw_rps &
  i=0
  for c in "${CLIENTS[@]}"; do set -- $c; local dest="$1" port="$9"
    $SSH "$dest" "iperf3 -c $TGT_IP -p $port -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/gw_c$i &
    i=$((i+1))
  done
  wait
  i=0
  for c in "${CLIENTS[@]}"; do local r=$(rate /tmp/gw_c$i); tot=$(awk "BEGIN{print $tot+$r}"); line="$line c$i=$r"; i=$((i+1)); done
  local ofo gw aggG
  ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  gw=$(awk '/^Average:/ && $2=="all"{printf "%.2f", 8*(100-$NF)/100}' /tmp/gw_rps)
  aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
  log "$(printf '  %-12s P=%-3s AGG=%6.2f G  gw=%s/8 cores  tgt-OFO=%s  [%s ]' "$label" "$P" "$aggG" "$gw" "$ofo" "$line")"
}

ensure_tmq(){ local dest="$1" su="$2"
  $SSH "$dest" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 14); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 3; done; return 1
}
stop_tmq(){ for c in "${CLIENTS[@]}"; do set -- $c; $SSH "$1" "$5 sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; for t in \$(ip -br link show|grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done'" >/dev/null 2>&1 & done; wait; }
stop_wg(){ for c in "${CLIENTS[@]}"; do set -- $c; $SSH "$1" "$5 ip link del wg0 2>/dev/null" >/dev/null 2>&1 & done; wait; }

log "=== gso vs WG @ rps (combined=1,rps=ff), 8c GW, 6 clients, all-up TCP jumbo, $(date -u '+%H:%M') ==="

# ---------------- Phase A: tmasque FORWARD_TUN_GSO @ rps ----------------
log "--- Phase A: tmasque-gso @ rps ---"
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
      - FORWARD_TUN_GSO=1
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
      memlock:
        soft: -1
        hard: -1
    command: /run.sh
volumes:
    db:
YAML
  cd ~/tmasqued
  ${GW_SUDO} docker compose down >/dev/null 2>&1
  ${GW_SUDO} ethtool -L $GW_IF combined 1 2>/dev/null
  ${GW_SUDO} docker compose up -d >/dev/null 2>&1
  sleep 14
  echo ff | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
"
for c in "${CLIENTS[@]}"; do set -- $c; ensure_tmq "$1" "$5" || log "  !! tmq client $1 down"; done
log "  [verify] gso env={ $($SSH "$GW_SSH" "${GW_SUDO} docker exec \$(${GW_SUDO} docker ps -q|head -1) env 2>/dev/null|grep FORWARD_|tr '\n' ' '")} rps=$($SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/queues/rx-0/rps_cpus")"
for P in "${PSWEEP[@]}"; do measure "tmq-gso" "$P"; done

# ---------------- Phase B: WireGuard @ rps ----------------
log "--- Phase B: WireGuard @ rps ---"
stop_tmq
$SSH "$GW_SSH" "cd ~/tmasqued && ${GW_SUDO} docker compose down >/dev/null 2>&1"   # XDP off for clean WG
bash setup_wg.sh >/tmp/gw_wgsetup.log 2>&1
bash stack.sh gw_regime rps >>"$OUT" 2>&1
bash stack.sh wg_mtu 9000 >>"$OUT" 2>&1
bash stack.sh route_all wg0 >/dev/null 2>&1
bash stack.sh wg_ensure_healthy
log "  [verify] wg peers handshaked: $($SSH "$GW_SSH" "${GW_SUDO} wg show wg0 latest-handshakes 2>/dev/null|awk '{print \$2}'|grep -c '[1-9]'") rps=$($SSH "$GW_SSH" "cat /sys/class/net/$GW_IF/queues/rx-0/rps_cpus")"
for P in "${PSWEEP[@]}"; do measure "wireguard" "$P"; done

# ---------------- Restore: tmasque (vhost) + RSS ----------------
log "=== restoring: tmasque vhost + RSS(combined=8) ==="
stop_wg
$SSH "$GW_SSH" "cp ~/tmasqued/docker-compose.yml.fwdcapbak ~/tmasqued/docker-compose.yml
  cd ~/tmasqued
  ${GW_SUDO} ethtool -L $GW_IF combined ${GW_RSS_COMBINED} 2>/dev/null
  echo 00 | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
  ${GW_SUDO} docker compose up -d >/dev/null 2>&1
  sleep 10"
for c in "${CLIENTS[@]}"; do set -- $c; ensure_tmq "$1" "$5" >/dev/null 2>&1; done
log "=== DONE ==="
