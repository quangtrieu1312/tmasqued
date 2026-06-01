#!/usr/bin/env bash
# Convenience alias for: tmasquectl client assign <client> <role>...
source /etc/tmasqued/scripts/helper.sh
exec bash "$SCRIPT_DIR/tmasquectl.sh" client assign "$@"
