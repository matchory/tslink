#!/bin/bash
# teardown.sh: delete the security testbed's server, firewall and SSH key, and
# its SSH config.
set -uo pipefail
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
: "${TSLINK_SEC_DIR:=$TSLINK_TEST_DIR/security}"
NAME=tslink-security
HCLOUD_TOKEN=$(cat "$TSLINK_TEST_DIR/hcloud.token")
export HCLOUD_TOKEN
hcloud server delete $NAME
hcloud firewall delete $NAME
hcloud ssh-key delete $NAME
rm -f "$TSLINK_SEC_DIR/ssh_config" "$TSLINK_SEC_DIR/known_hosts" "$TSLINK_SEC_DIR/firewall.json"
