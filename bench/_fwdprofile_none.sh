#!/bin/bash
# Profile the NONE-regime (combined=1, rps=00) forward funnel for the two paths the
# user cares about: GSO forward and default AF_XDP-TX ("withXDP") forward.
# For each: deploy none + ENABLE_STATISTIC=true (so :6060 pprof is live), drive the
# 6-client upload load, and MID-RUN capture:
#   - go CPU profile (25s)         -> what tmasque userspace burns on the funnel core
#   - goroutine / mutex / block    -> serial-pipeline waits
#   - per-core mpstat usr/sys/soft -> is the busy core in tmasque code or kernel RX softirq?
# Artifacts land in /tmp on the GW; pulled back by the caller. Restores baseline at end.
set -u
cd "$(dirname "$0")"; export FLEET=fleet-8core.sh; source ./fleet-8core.sh; source ./lib.sh

TGT_IP=198.18.5.80
P=${P:-4}
DUR=${DUR:-40}
PROF_SECS=${PROF_SECS:-25}
OUT=/tmp/fwdprofile_none.out; : > "$OUT"
log(){ echo "$@" | tee -a "$OUT"; }

# back up current standing compose + conf once
$SSH "$GW_SSH" "cd ~/tmasqued; [ -f docker-compose.yml.profbak ] || cp docker-compose.yml docker-compose.yml.profbak; [ -f tmasqued.conf.profbak ] || cp tmasqued.conf tmasqued.conf.profbak; sed -i 's/^ENABLE_STATISTIC=.*/ENABLE_STATISTIC=true/' tmasqued.conf; grep -q '^ENABLE_STATISTIC=' tmasqued.conf || echo 'ENABLE_STATISTIC=true' >> tmasqued.conf"

deploy(){ # $1=mode (default|gso)
  local mode="$1" extra=""
  case "$mode" in
    gso)   extra=$'      - FORWARD_TUN_GSO=1\n      - FORWARD_UPLOAD_RESEQ=1';;
    default) extra="";;
  esac
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
    ${GW_SUDO} docker compose down >/dev/null 2>&1
    ${GW_SUDO} ip link set $GW_IF mtu 3506 2>/dev/null
    ${GW_SUDO} ethtool -L $GW_IF combined 1 2>/dev/null
    ${GW_SUDO} docker compose up -d >/dev/null 2>&1
    sleep 15
    echo 00 | ${GW_SUDO} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null 2>&1
    curl -s -m3 localhost:6060/debug/pprof/ >/dev/null && echo PPROF_UP || echo PPROF_DOWN
  "
}

ensure(){ # reconnect a client tunnel; $1=ssh-dest $2=sudo
  local dest="$1" su="$2"
  $SSH "$dest" "pgrep -x tmasque >/dev/null && ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0
  $SSH "$dest" "$su sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq.log 2>&1 </dev/null &'" >/dev/null 2>&1
  local i; for i in $(seq 1 14); do $SSH "$dest" "ip route get $TGT_IP 2>/dev/null|grep -qE 'dev tun[0-9]+'" && return 0; sleep 3; done
  return 1
}

rate(){ awk '/receiver/{for(i=1;i<=NF;i++)if($i=="Mbits/sec")r=$(i-1)}END{print r+0}' "$1"; }

run(){ # $1=mode
  local mode="$1" i pids="" tot=0 line=""
  log "--- mode=$mode : deploy(none, stats on) ---"
  log "  $(deploy "$mode" | tr '\n' ' ')"
  for c in "${CLIENTS[@]}"; do set -- $c; ensure "$1" "$5" || log "  !! client $1 down"; done
  $SSH "$TARGET_SSH" "for p in 5201 5202 5203 5204 5205 5206; do ss -tln 2>/dev/null|grep -q \":\$p \"||iperf3 -s -p \$p -D >/dev/null 2>&1; done" >/dev/null 2>&1
  $SSH "$TARGET_SSH" "nstat -n >/dev/null 2>&1"
  # start load (DUR), per-core mpstat, then mid-run grab the CPU profile + snapshots
  i=0
  for c in "${CLIENTS[@]}"; do set -- $c; $SSH "$1" "iperf3 -c $TGT_IP -p $9 -t $DUR -O2 -P$P -f m 2>/dev/null" >/tmp/fp_$i & pids="$pids $!"; i=$((i+1)); done
  $SSH "$GW_SSH" "LC_ALL=C mpstat -P ALL 1 $PROF_SECS 2>/dev/null" >/tmp/fp_mpstat_$mode &
  sleep 6   # let it ramp to steady state
  $SSH "$GW_SSH" "
    curl -s -m$((PROF_SECS+8)) 'localhost:6060/debug/pprof/profile?seconds=$PROF_SECS' -o /tmp/prof_${mode}_cpu.pb.gz
    curl -s 'localhost:6060/debug/pprof/goroutine?debug=2' -o /tmp/prof_${mode}_goroutine.txt
    curl -s 'localhost:6060/debug/pprof/mutex'  -o /tmp/prof_${mode}_mutex.pb.gz
    curl -s 'localhost:6060/debug/pprof/block'  -o /tmp/prof_${mode}_block.pb.gz
    ls -l /tmp/prof_${mode}_*.* 2>/dev/null
  "
  wait
  i=0; for c in "${CLIENTS[@]}"; do local r=$(rate /tmp/fp_$i); tot=$(awk "BEGIN{print $tot+$r}"); line="$line c$i=$r"; i=$((i+1)); done
  local aggG ofo
  aggG=$(awk "BEGIN{printf \"%.2f\", $tot/1000}")
  ofo=$($SSH "$TARGET_SSH" "nstat 2>/dev/null|awk '/TCPOFOQueue/{o=\$2}/TcpInSegs/{s=\$2}END{if(s>0)printf \"%.1f%%\",100*o/s;else print 0}'")
  log "  RESULT mode=$mode P=$P AGG=${aggG}G tgt-OFO=$ofo [$line ]"
  # per-core busy split (usr/sys/soft) from the Average rows; flag the busiest core
  log "  per-core (Average usr/sys/soft, core-equivalents):"
  awk '/^Average:/ && $3 ~ /^[0-9]+$/ {printf "    cpu%-2s usr=%5.1f sys=%5.1f soft=%5.1f idle=%5.1f\n",$3,$4,$6,$9,$NF}' /tmp/fp_mpstat_$mode | tee -a "$OUT"
}

log "=== NONE-regime forward profile (gso vs default/withXDP), P=$P DUR=$DUR prof=${PROF_SECS}s $(date -u '+%F %H:%M') ==="
for m in gso default; do run "$m"; done

# grab the matching binary (for local symbolization) from the running container
$SSH "$GW_SSH" "cid=\$(${GW_SUDO} docker ps -q|head -1); ${GW_SUDO} docker cp \$cid:/usr/local/bin/tmasque /tmp/tmasque.bin 2>/dev/null; ls -l /tmp/tmasque.bin"

log "=== restoring baseline (combined=$GW_RSS_COMBINED, standing compose, stats off) ==="
$SSH "$GW_SSH" "cd ~/tmasqued; cp docker-compose.yml.profbak docker-compose.yml; cp tmasqued.conf.profbak tmasqued.conf; ${GW_SUDO} docker compose down >/dev/null 2>&1; ${GW_SUDO} ethtool -L $GW_IF combined ${GW_RSS_COMBINED} 2>/dev/null; ${GW_SUDO} docker compose up -d >/dev/null 2>&1"
log "=== DONE ==="
