#!/bin/bash
# Forward-path A/B/C cap probe on the 2-core fleet.
# Sweeps the 3 forward egress paths (default AF_XDP-TX / vhost / tun-GSO) and for each
# pushes both clients' aggregate UPLOAD (= the forward path) with rising parallelism to
# find the 2-core gateway's ceiling. Reports agg Gbit, GW core-equivalents (mpstat),
# target inner-TCP OFO%. jumbo 9000 / tcp / rss(Combined=2).
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh; source ./lib.sh

OUT=/tmp/fwdcap.out; : > "$OUT"
C0=$(set -- ${CLIENTS[0]}; echo $1); C1=$(set -- ${CLIENTS[1]}; echo $1)
TGT_IP=198.18.4.65
PSWEEP=(8 16 32)
DUR=${DUR:-15}

log(){ echo "$@" | tee -a "$OUT"; }

# Write the gw compose with a given extra-env block, recreate the container, pin jumbo+rss.
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
    image: tmasqued-tmasqued:latest
    cap_add: [NET_ADMIN, NET_RAW, SYS_ADMIN]
    privileged: true
    network_mode: host
    environment:
      - GOTRACEBACK=all
${extra}
    devices: [\"/dev/net/tun:/dev/net/tun\"]
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
    ${GW_SUDO} docker compose down >/dev/null 2>&1
    ${GW_SUDO} ip link set $GW_IF mtu 9000
    ${GW_SUDO} ethtool -L $GW_IF combined ${GW_RSS_COMBINED} 2>/dev/null
    ${GW_SUDO} docker compose up -d >/dev/null 2>&1
    sleep 12
    echo 00 | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
  "
}

# Prove which egress is live by the host device it creates (+ env in the container).
verify(){ # $1=mode
  local devs env
  devs=$($SSH "$GW_SSH" "ip -o link show | grep -oE 'tmvhost[0-9]+|tmfwd[0-9]+|tmnapi[0-9]+|tun[0-9]+' | sort -u | tr '\n' ' '")
  env=$($SSH "$GW_SSH" "${GW_SUDO} docker exec \$(${GW_SUDO} docker ps -q|head -1) env 2>/dev/null | grep -E 'FORWARD_' | tr '\n' ' '")
  log "  [verify $1] host-devs={ $devs} container-env={ $env}"
}

ensure(){ # bring a client tunnel up (reconnect after gw restart)
  $SSH "$1" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$1" "doas sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 12); do $SSH "$1" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 3; done
  return 1
}

rate(){ printf '%s' "$1" | awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}'; }

measure(){ # $1=mode $2=P
  local mode="$1" P="$2"
  $SSH "$TARGET_SSH" "for p in 5201 5202; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  $SSH "$GW_SSH" "LC_ALL=C mpstat -P ALL 1 $DUR 2>/dev/null" >/tmp/fc_gw &
  $SSH "$C0" "iperf3 -c $TGT_IP -p 5201 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/fc0 &
  $SSH "$C1" "iperf3 -c $TGT_IP -p 5202 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/fc1 &
  wait
  local r0 r1 ofo gw agg
  r0=$(rate "$(cat /tmp/fc0)"); r1=$(rate "$(cat /tmp/fc1)")
  ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{i=\$2}END{if(i>0)printf \"%.1f%%\",100*o/i;else print 0}'")
  # core-equivalents busy = sum over cpus of (100-idle)/100
  gw=$(awk '/^Average:/ && $2 ~ /^[0-9]+$/ {n++; s+=(100-$NF)/100} END{printf "%.2f", s}' /tmp/fc_gw)
  agg=$(awk "BEGIN{print ($r0+$r1)/1000}")
  log "$(printf '  %-7s P=%-3s c0=%-6s c1=%-6s AGG=%6.2f G  gw=%s/2 cores  tgt-OFO=%s' "$mode" "$P" "$r0" "$r1" "$agg" "$gw" "$ofo")"
}

log "=== forward-path cap probe (jumbo9000/tcp/rss, $(date -u '+%Y-%m-%d %H:%M')) ==="
for mode in default vhost gso; do
  log "--- deploying mode=$mode ---"
  deploy "$mode"
  verify "$mode"
  ensure "$C0"; ec0=$?; ensure "$C1"; ec1=$?
  if [ $ec0 -ne 0 ] || [ $ec1 -ne 0 ]; then log "  !! client tunnel did not come up (c0=$ec0 c1=$ec1) — skipping $mode"; continue; fi
  for P in "${PSWEEP[@]}"; do measure "$mode" "$P"; done
done
log "=== restoring baseline compose ==="
$SSH "$GW_SSH" "cp ~/tmasqued/docker-compose.yml.baseline ~/tmasqued/docker-compose.yml; cd ~/tmasqued; ${GW_SUDO} docker compose up -d >/dev/null 2>&1"
log "=== DONE ==="
