#!/bin/bash
set -u
cd "$(dirname "$0")"
bash run_2core_wg.sh
bash run_2core_tmasque.sh
echo "[2core] VPN blocks DONE"
