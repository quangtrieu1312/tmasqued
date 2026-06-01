#!/usr/bin/env bash
# Convenience alias for: tmasquectl role assign <role> <resource>...
source /etc/tmasqued/scripts/helper.sh
exec bash "$SCRIPT_DIR/tmasquectl.sh" role assign "$@"
