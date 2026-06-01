#!/usr/bin/env bash
BASE="/etc/tmasqued"
CA_DIR=$BASE/ca
CERT_DIR=$BASE/certs
SCRIPT_DIR=$BASE/scripts
DB_PATH=$BASE/tmasque.db

SERVER_CA_DIR=$CA_DIR/server
SERVER_CERT_DIR=$CERT_DIR/server

CLIENT_CA_DIR=$CA_DIR/client
CLIENT_CERT_DIR=$CERT_DIR/client

MANAGEMENT_SOCKET_PATH=/var/run/tmasqued.sock
MGMT_URL="http://tmasqued"

# --- management API helpers (sourced by gen*/assign* scripts) ---------------

# mgmt METHOD PATH [JSON_BODY] — call the management API over the unix socket.
# --fail turns a non-2xx response into a non-zero exit, so callers can detect
# API-level errors (e.g. a duplicate name rejected by the UNIQUE constraint).
mgmt() {
    if [ -n "$3" ]; then
        curl --fail -s --unix-socket "$MANAGEMENT_SOCKET_PATH" -X "$1" "$MGMT_URL$2" --data "$3"
    else
        curl --fail -s --unix-socket "$MANAGEMENT_SOCKET_PATH" -X "$1" "$MGMT_URL$2"
    fi
}

# json_str_array NAME... — print a JSON array of strings, e.g. ["a","b","c"].
# Builds the {role_names:[…]} / {resource_names:[…]} bodies for the by-name
# link endpoints; the server resolves those names to ids.
json_str_array() {
    local out=""
    for n in "$@"; do out="$out,\"$n\""; done
    echo "[${out#,}]"
}

