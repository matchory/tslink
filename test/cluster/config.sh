# shellcheck shell=bash
# config.sh: settings of the cluster tests, sourced by the helpers. Set any of them in the
# environment to override it; the defaults suit a Swarm built by provision.sh.
_here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# The nodes: an SSH config whose Host entries (mgr, w1, ...) are the Swarm nodes
: "${TSLINK_TEST_SSH_CONFIG:=$_here/ssh_config}"
# Credentials (*.secret, node.authkey, hcloud.token), the API token cache and SSH state
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
# Tag prefix: tasks are tagged <prefix>-caller, -callee or -nogrant, host tailscaleds <prefix>-node
: "${TSLINK_TEST_TAG:=tag:tslink-test}"
# Service prefix: the callee and multi stacks host <prefix>-callee and <prefix>-other; mixed asks
# for <prefix>-mixed, which tslink must refuse; manual hosts <prefix>-manual, which no autoApprovers
# approve
: "${TSLINK_TEST_SVC:=svc:tslink-test}"
# The Services the tests use, which cleanup.sh deletes
: "${TSLINK_TEST_SERVICES:=$TSLINK_TEST_SVC-callee $TSLINK_TEST_SVC-other $TSLINK_TEST_SVC-mixed $TSLINK_TEST_SVC-manual}"
# Tailscale API base URL and tailnet ("-" is the tailnet of the API client)
: "${TSLINK_TEST_API:=https://api.tailscale.com/api/v2}"
: "${TSLINK_TEST_TAILNET:=-}"
# MTU of the stacks' tslink networks: the nodes' network MTU minus overlay overhead
# (1450 suits the Hetzner provisioner's private network)
: "${TSLINK_TEST_MTU:=1450}"
# Power actions for disrupt's reset and poweroff: <script> reset|poweroff|poweron <node>
: "${TSLINK_TEST_POWER:=$_here/hetzner-power}"
unset _here
