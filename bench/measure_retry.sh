#!/bin/bash
# Retry wrapper: wt0-gated, resumable, NA-retrying. Re-runs a cell up to N times until clean
# (no NA, agg>0), keeping the cleanest attempt. Skips cells already done (resume). Appends to $OUT.
# Usage: FLEET=... OUT=... measure_retry.sh <stack> <regime> <mtu> <proto> <scenario>
set -u
cd "$(dirname "$0")"; source "${FLEET:-./fleet-8core.sh}"; source ./lib.sh
TRIES=${TRIES:-3}
realout="${OUT:-}"

# resume: if this cell already has a clean row, keep it and skip
if [ -n "$realout" ] && cell_done "$realout" "$@"; then
  awk -F'\t' -v s="$1" -v r="$2" -v m="$3" -v p="$4" -v sc="$5" \
    '$1==s&&$2==r&&$3==m&&$4==p&&$5==sc && $6+0>0 && index($0,":NA")==0 {print; exit}' "$realout"
  exit 0
fi

best=""; best_na=999; best_agg=0
for ((a=1;a<=TRIES;a++)); do
  gate                                  # wait through any wt0 outage before measuring
  row=$(OUT="" timeout 160 bash measure.sh "$@" 2>/dev/null)
  [ -z "$row" ] && { sleep 3; continue; }
  na=$(printf '%s' "$row" | grep -o ':NA' | wc -l | tr -d ' ')
  agg=$(printf '%s' "$row" | cut -f6)
  better=0
  if [ "$na" -lt "$best_na" ]; then better=1
  elif [ "$na" -eq "$best_na" ] && awk "BEGIN{exit !($agg>$best_agg)}"; then better=1; fi
  [ "$better" = 1 ] && { best="$row"; best_na=$na; best_agg=$agg; }
  { [ "$na" -eq 0 ] && awk "BEGIN{exit !($agg>0)}"; } && break
  sleep 3
done
# don't emit/append a blank line if every attempt failed (all-empty rows)
[ -n "$best" ] && printf '%s\n' "$best"
[ -n "$realout" ] && [ -n "$best" ] && printf '%s\n' "$best" >> "$realout"
