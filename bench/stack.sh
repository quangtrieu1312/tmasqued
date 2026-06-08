#!/bin/bash
# Stack / regime / MTU control helpers. Source then call.
set -u
cd "$(dirname "$0")"; source "${FLEET:-./fleet-8core.sh}"

# --- route the target through the right datapath on every client ---
# logical dev: "fabric" -> per-client WAN iface (ens3/eth0); else literal (wg0/tun0)
route_all() { # $1 = fabric | wg0 | tun0   (routes EACH client's own target, field 8)
  local want="$1" c su dest ip wanif dev tgt
  for c in "${CLIENTS[@]}"; do
    set -- $c; dest="$1"; ip="$2"; su="$5"; wanif="$6"; tgt="$8"
    [ "$want" = fabric ] && dev="$wanif" || dev="$want"
    $SSH "$dest" "$su ip route replace $tgt dev $dev" >/dev/null 2>&1 \
      && echo "  [$ip] route $tgt -> $dev" || echo "  [$ip] route FAILED ($dev)"
  done
}

# --- NIC regime on the GW (WG/kernel path: no XDP attached, free to ethtool) ---
# rss  = Combined max (8), RPS off
# rps  = Combined 1, RPS ff (spread softirq across cores)
# none = Combined 1, RPS off (single-core funnel)
gw_regime() { # $1 = rss|rps|none   (assumes no tmasqued container OR container already stopped)
  local r="$1" comb rps
  case "$r" in
    rss)  comb=${GW_RSS_COMBINED:-8}; rps=00 ;;
    rps)  comb=1; rps=ff ;;
    none) comb=1; rps=00 ;;
    *) echo "bad regime $r" >&2; return 2 ;;
  esac
  $SSH "$GW_SSH" "
    ${GW_SUDO:-sudo} ethtool -L $GW_IF combined $comb 2>&1 | grep -iv '^$' || true
    echo $rps | ${GW_SUDO:-sudo} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null
    cur=\$(ethtool -l $GW_IF | awk '/Current/{f=1} f&&/Combined/{print \$2; exit}')
    echo \"  regime=$r requested combined=$comb -> actual=\$cur  rps=$rps\"
  "
}

# --- MTU on the WG tunnel (all wg0 ifaces) ---
wg_mtu() { # $1 = 9000|1500  -> wg0 mtu 8920|1420
  local m=$1 wm; [ "$m" = 9000 ] && wm=8920 || wm=1420
  $SSH "$GW_SSH" "${GW_SUDO:-sudo} ip link set wg0 mtu $wm" 2>/dev/null
  local c su dest
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"
    $SSH "$dest" "$su ip link set wg0 mtu $wm" >/dev/null 2>&1
  done
  echo "  wg0 mtu -> $wm (WAN $m)"
}

# --- tmasque GW reconfigure: stop container (detach XDP), set ens3 MTU + NIC combined,
#     restart container (re-attach XDP, derive inner MTU), set RPS, re-assert role->target.
gw_tmasque_config() { # $1=regime(rss|none|rps)  $2=mtu(9000|1500)
  local regime="$1" mtu="$2" comb rps
  case "$regime" in rss) comb=${GW_RSS_COMBINED:-8}; rps=00;; rps) comb=1; rps=ff;; none) comb=1; rps=00;; *) echo "bad regime $regime" >&2; return 2;; esac
  $SSH "$GW_SSH" "
    cd ~/tmasqued
    ${GW_SUDO:-sudo} docker compose down >/dev/null 2>&1
    ${GW_SUDO:-sudo} ip link set $GW_IF mtu $mtu
    ${GW_SUDO:-sudo} ethtool -L $GW_IF combined $comb 2>/dev/null
    ${GW_SUDO:-sudo} docker compose up -d >/dev/null 2>&1
    sleep 10
    echo $rps | ${GW_SUDO:-sudo} tee /sys/class/net/$GW_IF/queues/rx-0/rps_cpus >/dev/null
    C=\$(${GW_SUDO:-sudo} docker ps -q | head -1)
    D=\"${GW_SUDO:-sudo} docker exec \$C tmasquectl\"
    for r in cn1 cn2 cn3 cn4 cn5 cn6; do \$D role assign \$r ttgt >/dev/null 2>&1; done
    eff=\$(${GW_SUDO:-sudo} docker logs \$C 2>&1 | grep -oE 'combined channels=[0-9]+|WAN\\(eff\\)=[0-9]+ -> inner=[0-9]+' | tail -2 | tr '\n' ' ')
    echo \"  [GW] regime=$regime mtu=$mtu -> \$eff rps=$rps\"
  "
}

# --- client WAN (underlay) MTU: tmasque derives inner from min(WAN,3506) ---
client_wan_mtu() { # $1=9000|1500
  local m="$1" c dest su wanif
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"; wanif="$6"
    $SSH "$dest" "$su ip link set $wanif mtu $m" >/dev/null 2>&1
  done
  echo "  client WAN mtu -> $m"
}

# --- tmasque client lifecycle ---
# robust kill by /proc/*/exe (busybox pkill is unreliable; pkill -f self-matches ssh)
tmasque_stop_one() { # dest su  -- SIGKILL by exe (clean readlink extraction; works on busybox too)
  $SSH "$1" "$2 sh -c 'for d in /proc/[0-9]*; do [ \"\$(readlink \$d/exe 2>/dev/null)\" = /usr/local/bin/tmasque ] && kill -9 \${d#/proc/} 2>/dev/null; done; sleep 1; for t in \$(ip -br link show | grep -oE \"^tun[0-9]+\"); do ip link del \$t 2>/dev/null; done'" >/dev/null 2>&1
}
tmasque_start_one() { # dest su  -- ensure tun module (Alpine drops it), then launch detached
  $SSH "$1" "$2 sh -c 'modprobe tun 2>/dev/null; setsid /usr/local/bin/tmasque >/tmp/tmq-bench.log 2>&1 </dev/null &' " >/dev/null 2>&1
}
tmasque_up_all() {
  local c dest su ip
  # stop everything first, verify dead (avoids stale tun0->tun1 doubling)
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"; tmasque_stop_one "$dest" "$su"; done
  sleep 3
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"; tmasque_stop_one "$dest" "$su"; done
  sleep 2
  for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"; tmasque_start_one "$dest" "$su"; done
  # per-client readiness: retry until target routes via a tunX dev (settle the reconnect blip)
  local allok=1
  for c in "${CLIENTS[@]}"; do
    set -- $c; dest="$1"; ip="$2"; su="$5"; local tgt="$8"
    local r="" tries=0
    while [ $tries -lt 12 ]; do
      r=$(timeout 6 $SSH "$dest" "ip route get $tgt 2>/dev/null | head -1 | grep -oE 'dev tun[0-9]+'" 2>/dev/null)
      [ -n "$r" ] && break
      tries=$((tries+1)); sleep 3
    done
    if [ -n "$r" ]; then echo "  [$ip] $tgt -> $r (after ${tries}x)"; else echo "  [$ip] NOT READY"; allok=0; fi
  done
  [ $allok -eq 1 ] && echo "  ALL CLIENTS READY" || echo "  WARNING: some clients not ready"
}
# per-cell health: restart any client whose tmasque died or whose target route isn't via tunX
tmasque_ensure_healthy() {
  local c dest su tgt ip fixed=0
  for c in "${CLIENTS[@]}"; do
    set -- $c; dest="$1"; ip="$2"; su="$5"; tgt="$8"
    local ok=$(timeout 8 $SSH "$dest" "pgrep -x tmasque >/dev/null 2>&1 && ip route get $tgt 2>/dev/null | grep -qE 'dev tun[0-9]+' && echo ok" 2>/dev/null)
    if [ "$ok" != ok ]; then tmasque_stop_one "$dest" "$su"; tmasque_start_one "$dest" "$su"; fixed=1; fi
  done
  if [ "$fixed" = 1 ]; then
    # poll each client until the target routes via a tunX (tmasque installs its own route in
    # table 9000; do NOT force dev tun0 — a restarted client may come up as tun1 -> black hole).
    for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; tgt="$8"
      local tr=0
      while [ $tr -lt 8 ]; do
        timeout 6 $SSH "$dest" "ip route get $tgt 2>/dev/null | grep -qE 'dev tun[0-9]+'" >/dev/null 2>&1 && break
        tr=$((tr+1)); sleep 3
      done
    done
  fi
}
# per-cell WG health: re-assert route + refresh handshake; rebuild the mesh ONLY if a client's wg0
# interface is actually gone (NOT on a transient route blip / ssh timeout -> avoids rebuild thrash).
wg_ensure_healthy() {
  local c dest su tgt rebuild=0
  for c in "${CLIENTS[@]}"; do
    set -- $c; dest="$1"; su="$5"; tgt="$8"
    local st=$(timeout 6 $SSH "$dest" "if ip link show wg0 >/dev/null 2>&1; then $su ip route replace $tgt dev wg0 2>/dev/null; ping -c1 -W2 10.9.0.1 >/dev/null 2>&1; echo HAVE; else echo GONE; fi" 2>/dev/null)
    [ "$st" = GONE ] && rebuild=1
  done
  [ "$rebuild" = 1 ] && bash "$(dirname "$0")/setup_wg.sh" >/dev/null 2>&1
}
tmasque_down_all() {
  local c dest su; for c in "${CLIENTS[@]}"; do set -- $c; dest="$1"; su="$5"
    tmasque_stop_one "$dest" "$su"; done; echo "  all tmasque clients stopped"
}

"$@"
