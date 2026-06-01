#!/usr/bin/env bash
# tmasquectl — one wrapper over the tmasqued management API, so you don't have to
# memorize endpoints. Everything is addressed by NAME (names are unique). Run with
# no args (or `help`) for the command list.
set -o pipefail
source /etc/tmasqued/scripts/helper.sh
PROG=$(basename "$0")

usage() {
    cat <<EOF
$PROG — manage the tmasqued VPN (clients, roles, resources, routes) by name.

CLIENTS
  $PROG client list                       list all clients
  $PROG client show     <name>            show one client
  $PROG client create   <name>            create client + its default role + cert bundle
  $PROG client rename   <name> <new>      rename (also renames the default role)
  $PROG client delete   <name>            delete client (and its default role)
  $PROG client roles    <name>            roles linked to the client
  $PROG client resources <name>           the client's effective routes
  $PROG client assign   <name> <role>...  link role(s) to the client
  $PROG client unassign <name> <role>...  unlink role(s)

ROLES
  $PROG role list
  $PROG role show     <name>
  $PROG role create   <name>
  $PROG role rename   <name> <new>
  $PROG role delete   <name>
  $PROG role assign   <name> <resource>...  grant resource(s) to the role
  $PROG role unassign <name> <resource>...  revoke resource(s)

RESOURCES (a named CIDR/route)
  $PROG resource list
  $PROG resource show   <name>
  $PROG resource create <name> <cidr>     e.g. $PROG resource create corp 10.0.0.0/8
  $PROG resource rename <name> <new>
  $PROG resource delete <name>

DHCP POOL
  $PROG dhcp show
  $PROG dhcp reset <first-ip-int> <last-ip-int>

Names are unique; create rejects duplicates.
EOF
}

die() { echo "$PROG: $1" >&2; exit 1; }

# api METHOD PATH [BODY] — call the management API; pretty-print any JSON body on
# success, or die (non-zero exit) if the server rejected the request. Output is
# captured first, so curl --fail's exit code is honored (not masked by a jq pipe).
api() {
    local out
    out=$(mgmt "$@") || die "request rejected by the server ($1 $2) — likely a duplicate name, a missing entity, or invalid input"
    [ -n "$out" ] && printf '%s\n' "$out" | jq .
    return 0
}

# id_of COLLECTION NAME — print the id for a unique name, or return 1 (no output).
# Returns rather than dies, so callers can react in the parent shell (not a subshell).
id_of() {
    local id
    id=$(curl -s --unix-socket "$MANAGEMENT_SOCKET_PATH" "$MGMT_URL/$1" \
        | jq -r --arg n "$2" 'map(select(.name==$n)) | .[0].id // empty') || return 1
    [ -n "$id" ] || return 1
    printf '%s\n' "$id"
}

# show_named COLLECTION NAME — print the single row whose name matches.
show_named() {
    local out
    out=$(mgmt GET "/$1") || die "cannot list $1"
    printf '%s\n' "$out" | jq --arg n "$2" '.[] | select(.name==$n)'
}

noun=${1-}; verb=${2-}
if [ $# -ge 2 ]; then shift 2; else set --; fi

case "$noun" in
  client)
    case "$verb" in
      list)       api GET /clients ;;
      show)       [ -z "${1-}" ] && die "usage: client show <name>"; show_named clients "$1" ;;
      create)     [ -z "${1-}" ] && die "usage: client create <name>"; exec bash "$SCRIPT_DIR/gen_client.sh" "$1" ;;
      rename)     [ -z "${2-}" ] && die "usage: client rename <name> <new>"; id=$(id_of clients "$1") || die "no client named '$1'"; api PATCH "/clients/$id" "{\"name\":\"$2\"}"; echo "renamed client '$1' -> '$2'" ;;
      delete)     [ -z "${1-}" ] && die "usage: client delete <name>"; id=$(id_of clients "$1") || die "no client named '$1'"; api DELETE "/clients/$id" ;;
      roles)      [ -z "${1-}" ] && die "usage: client roles <name>"; id=$(id_of clients "$1") || die "no client named '$1'"; api GET "/clients/$id/roles" ;;
      resources)  [ -z "${1-}" ] && die "usage: client resources <name>"; id=$(id_of clients "$1") || die "no client named '$1'"; api GET "/clients/$id/resources" ;;
      assign)     c=${1-}; [ $# -ge 1 ] && shift; { [ -z "$c" ] || [ $# -eq 0 ]; } && die "usage: client assign <name> <role>..."; api POST "/clients/by-name/$c/roles" "{\"role_names\":$(json_str_array "$@")}" ;;
      unassign)   c=${1-}; [ $# -ge 1 ] && shift; { [ -z "$c" ] || [ $# -eq 0 ]; } && die "usage: client unassign <name> <role>..."; api DELETE "/clients/by-name/$c/roles" "{\"role_names\":$(json_str_array "$@")}" ;;
      *)          die "unknown: client $verb (try '$PROG help')" ;;
    esac ;;
  role)
    case "$verb" in
      list)       api GET /roles ;;
      show)       [ -z "${1-}" ] && die "usage: role show <name>"; show_named roles "$1" ;;
      create)     [ -z "${1-}" ] && die "usage: role create <name>"; api POST /roles "{\"names\":[\"$1\"]}" ;;
      rename)     [ -z "${2-}" ] && die "usage: role rename <name> <new>"; id=$(id_of roles "$1") || die "no role named '$1'"; api PATCH "/roles/$id" "{\"name\":\"$2\"}"; echo "renamed role '$1' -> '$2'" ;;
      delete)     [ -z "${1-}" ] && die "usage: role delete <name>"; id=$(id_of roles "$1") || die "no role named '$1'"; api DELETE "/roles/$id" ;;
      assign)     r=${1-}; [ $# -ge 1 ] && shift; { [ -z "$r" ] || [ $# -eq 0 ]; } && die "usage: role assign <name> <resource>..."; api POST "/roles/by-name/$r/resources" "{\"resource_names\":$(json_str_array "$@")}" ;;
      unassign)   r=${1-}; [ $# -ge 1 ] && shift; { [ -z "$r" ] || [ $# -eq 0 ]; } && die "usage: role unassign <name> <resource>..."; api DELETE "/roles/by-name/$r/resources" "{\"resource_names\":$(json_str_array "$@")}" ;;
      *)          die "unknown: role $verb (try '$PROG help')" ;;
    esac ;;
  resource)
    case "$verb" in
      list)       api GET /resources ;;
      show)       [ -z "${1-}" ] && die "usage: resource show <name>"; show_named resources "$1" ;;
      create)     [ -z "${2-}" ] && die "usage: resource create <name> <cidr>"; api POST /resources "{\"resources\":[{\"name\":\"$1\",\"value\":\"$2\"}]}" ;;
      rename)     [ -z "${2-}" ] && die "usage: resource rename <name> <new>"; id=$(id_of resources "$1") || die "no resource named '$1'"; api PATCH "/resources/$id" "{\"name\":\"$2\"}"; echo "renamed resource '$1' -> '$2'" ;;
      delete)     [ -z "${1-}" ] && die "usage: resource delete <name>"; id=$(id_of resources "$1") || die "no resource named '$1'"; api DELETE "/resources/$id" ;;
      *)          die "unknown: resource $verb (try '$PROG help')" ;;
    esac ;;
  dhcp)
    case "$verb" in
      show)       api GET /dhcp ;;
      reset)      [ -z "${2-}" ] && die "usage: dhcp reset <first-ip-int> <last-ip-int>"; api PUT /dhcp "{\"first_ip\":$1,\"last_ip\":$2}"; echo "dhcp pool reset" ;;
      *)          die "unknown: dhcp $verb (try '$PROG help')" ;;
    esac ;;
  ""|help|-h|--help) usage ;;
  *) usage; exit 1 ;;
esac
