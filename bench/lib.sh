#!/bin/bash
# Common helpers for wt0-drop resilience + resumability.
# (wt0 = the user's laptop VPN to the testbed; drops ~every 30 min, needs manual reconnect.
#  Only the SSH control plane rides wt0 — testbed-internal iperf3 traffic does not.)

# gate: block until the gateway is reachable again (poll through wt0 outages).
gate() {
  local n=0
  while ! timeout 8 $SSH "$GW_SSH" 'echo ok' >/dev/null 2>&1; do
    n=$((n+1))
    [ $((n%4)) -eq 1 ] && echo "[gate] wt0/SSH to $GW_SSH down — waiting for reconnect (try $n)..." >&2
    sleep 30
  done
}

# cell_done <tsvfile> <stack> <regime> <mtu> <proto> <scen>
# returns 0 (true) if a CLEAN row (agg>0, no client NA) already exists -> skip on resume.
cell_done() {
  [ -f "$1" ] || return 1
  awk -F'\t' -v s="$2" -v r="$3" -v m="$4" -v p="$5" -v sc="$6" '
    $1==s && $2==r && $3==m && $4==p && $5==sc { if ($6+0>0 && index($0,":NA")==0) f=1 }
    END { exit !f }' "$1"
}
