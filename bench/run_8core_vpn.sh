#!/bin/bash
# WG then tmasque on the 8-core env (sequential; both reuse the 6-client fleet).
set -u
cd "$(dirname "$0")"
echo "=== [8core] WireGuard block ==="; bash run_wg.sh
echo "=== [8core] tmasque block ==="; bash run_tmasque.sh
echo "=== [8core] VPN blocks DONE ==="
