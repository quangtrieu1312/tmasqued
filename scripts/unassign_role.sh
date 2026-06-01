#!/usr/bin/env bash
# Convenience alias for: tmasquectl client unassign <client> <role>...
source /etc/tmasqued/scripts/helper.sh
exec bash "$SCRIPT_DIR/tmasquectl.sh" client unassign "$@"
