#!/usr/bin/env bash
source /etc/tmasqued/scripts/helper.sh
source /etc/tmasqued/tmasqued.conf
function log {
    level=$(echo $1 | tr '[a-z]' '[A-Z]')
    msg=$2
    echo -e "$(date --rfc-3339 ns) bootstrap [$level]: $msg"
}

log "info" "Initializing masque server"
log "info" "Environment variables ---->>\n$(printenv)"
log "info" "Creating server CA, server cert, and client CA"

chmod -R +x $SCRIPT_DIR/*

ln -s $SCRIPT_DIR/gen_server_CA.sh /usr/sbin/genServerCA
ln -s $SCRIPT_DIR/gen_client_CA.sh /usr/sbin/genClientCA
ln -s $SCRIPT_DIR/gen_server_cert.sh /usr/sbin/genServerCert
ln -s $SCRIPT_DIR/gen_client_cert.sh /usr/sbin/genClientCert
ln -s $SCRIPT_DIR/tmasquectl.sh /usr/sbin/tmasquectl
ln -s $SCRIPT_DIR/gen_client.sh /usr/sbin/genClient
ln -s $SCRIPT_DIR/gen_role.sh /usr/sbin/genRole
ln -s $SCRIPT_DIR/gen_resource.sh /usr/sbin/genResource
ln -s $SCRIPT_DIR/assign_role.sh /usr/sbin/assignRole
ln -s $SCRIPT_DIR/unassign_role.sh /usr/sbin/unassignRole
ln -s $SCRIPT_DIR/assign_resource.sh /usr/sbin/assignResource
ln -s $SCRIPT_DIR/unassign_resource.sh /usr/sbin/unassignResource

genServerCA
genServerCert -f --dns-list ${SAN_DNS_LIST} --ip-list ${SAN_IP_LIST}
genClientCA

log "info" "Running masque daemon"
tmasqued
