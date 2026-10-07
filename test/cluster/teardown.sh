#!/bin/bash
# teardown.sh -- Hetzner Cloud provisioner: cleanup.sh's tailnet cleanup, then delete all Hetzner
# resources labelled purpose=tslink-test and the ssh_config provision.sh wrote. The Hetzner
# project is left for a human to remove.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
"$B/cleanup.sh"
HCLOUD_TOKEN=$(cat "$TSLINK_TEST_DIR/hcloud.token")
export HCLOUD_TOKEN
for n in $(hcloud server list -l purpose=tslink-test -o noheader -o columns=name); do hcloud server delete "$n"; done
hcloud firewall delete tslink-test
hcloud network delete tslink-test
hcloud ssh-key delete tslink-test
hcloud server list
rm -f "$TSLINK_TEST_SSH_CONFIG"
