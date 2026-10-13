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
# Service prefix: the callee stack hosts <prefix>-callee and <prefix>-other
: "${TSLINK_TEST_SVC:=svc:tslink-test}"
# Tailscale API base URL and tailnet ("-" is the tailnet of the API client)
: "${TSLINK_TEST_API:=https://api.tailscale.com/api/v2}"
: "${TSLINK_TEST_TAILNET:=-}"
# MTU of the stacks' tslink networks: the nodes' network MTU minus overlay overhead
# (1450 suits the Hetzner provisioner's private network)
: "${TSLINK_TEST_MTU:=1450}"
# Power actions for disrupt's reset and poweroff: <script> reset|poweroff|poweron <node>
: "${TSLINK_TEST_POWER:=$_here/hetzner-power}"
unset _here
