#!/usr/bin/env bash
# Convenience alias for: tmasquectl role create <name>
source /etc/tmasqued/scripts/helper.sh
exec bash "$SCRIPT_DIR/tmasquectl.sh" role create "$@"
