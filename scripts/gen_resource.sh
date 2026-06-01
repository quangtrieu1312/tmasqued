#!/usr/bin/env bash
# Convenience alias for: tmasquectl resource create <name> <cidr>
source /etc/tmasqued/scripts/helper.sh
exec bash "$SCRIPT_DIR/tmasquectl.sh" resource create "$@"
