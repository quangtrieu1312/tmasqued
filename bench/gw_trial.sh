#!/bin/bash
# Apply gateway container env override(s), restart via the tested gw_tmasque_config recipe
# (jumbo/rss, re-derive inner MTU, re-assert roles), verify, then probe P=1 and P=8.
# Usage: bash gw_trial.sh "LABEL" "KEY1=VAL1 KEY2=VAL2 ..."   (empty 2nd arg = baseline env)
set -u
cd "$(dirname "$0")"; export FLEET=fleet-2core.sh; source ./fleet-2core.sh
GW="$GW_SSH"; LABEL="$1"; EXTRA="${2:-}"
envarr="GOTRACEBACK=all"; for kv in $EXTRA; do envarr="$envarr, $kv"; done
echo "################ TRIAL: $LABEL  env=[$envarr] ################"
$SSH "$GW" "cd ~/tmasqued; [ -f docker-compose.yml.orig ] || cp docker-compose.yml docker-compose.yml.orig; sed -i 's|^\( *environment:\).*|\1 [$envarr]|' docker-compose.yml; grep -n environment: docker-compose.yml"
bash stack.sh gw_tmasque_config rss 9000
echo "  -- verify container env + inner MTU --"
$SSH "$GW" "C=\$(doas docker ps -q|head -1); doas docker exec \$C env 2>/dev/null | grep -iE 'FORWARD|TUN_|XDP_|PACING|QUIC_GO|TUNNEL_' | tr '\n' ' '; echo; doas docker logs \$C 2>&1 | grep -oE 'inner=[0-9]+' | tail -1"
P=1 DUR=15 bash probe.sh "$LABEL-P1"
P=8 DUR=15 bash probe.sh "$LABEL-P8"