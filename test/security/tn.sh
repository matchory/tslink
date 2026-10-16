# shellcheck shell=bash disable=SC2034 # its variables are the probes'
# tn.sh: the Swarm bed of test/cluster, in a Tailscale tailnet, for the probes
# of redteam/ that run against it. They source it as ../tn.sh from their own
# directory and run on this machine, reaching the nodes over SSH with
# test/cluster's helpers: run one as test/security/redteam/<probe>.sh.
# redteam.sh copies each probe to the single-host testbed, where it finds no
# tn.sh and exits SKIPPED; so does one whose SSH config names no nodes.
#
# The bed is the one test/cluster/README.md describes, with the callee, caller
# and nogrant stacks deployed. Credentials stay in $TSLINK_TEST_DIR and reach
# the nodes on stdin only.
C=$(cd "$(dirname "${BASH_SOURCE[0]}")/../cluster" && pwd)
# shellcheck source=../cluster/config.sh
. "$C/config.sh"
if [ ! -f "$TSLINK_TEST_SSH_CONFIG" ] || [ -z "$("$C/nodes")" ]; then
	echo "SKIPPED: no Swarm bed in $TSLINK_TEST_SSH_CONFIG"
	exit 3
fi
M=${MGR:-mgr}
T=$TSLINK_TEST_TAG
TN=$TSLINK_TEST_TAILNET
# The stack whose tag the cluster credential may mint, and that tag
TN_BASE=${T#tag:}
TN_DATA=/var/lib/docker-plugins/tailscale
TN_WORK=$(
	umask 077
	mktemp -d
)
TN_EXIT=()

tn_s() { "$C/s" "$@"; }
tn_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# tn_error MESSAGE: a failure of the setup, not of an attack: exits ERROR
tn_error() {
	echo "ERROR: $*" >&2
	exit 5
}

# tn_on_exit CMD: runs CMD, a shell command, when the probe exits, after the
# ones registered before it
tn_on_exit() { TN_EXIT=("$1" "${TN_EXIT[@]}"); }
tn_down() {
	local status=$? c
	for c in "${TN_EXIT[@]}"; do eval "$c"; done
	rm -rf "$TN_WORK"
	exit "$status"
}
trap tn_down EXIT

# tn_wait_for SECONDS CMD...: runs CMD every 5 s until it succeeds
tn_wait_for() {
	local end=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$end" ] || return 1
		sleep 5
	done
}

# tn_eps: endpoints.sh on every node, each line led by the node: node,
# endpoint, stack, hostname, state, advertised Services, error, tab-separated
tn_eps() {
	local n
	for n in $("$C/nodes"); do
		tn_s "$n" sh -s <"$C/endpoints.sh" | awk -v n="$n" '{ print n "\t" $0 }'
	done
}
# tn_states STACK: "state: error" of each of the stack's endpoints
tn_states() { tn_eps | awk -F'\t' -v s="$1" '$3 == s { print $5 ": " $7 }'; }
tn_stack_exists() { tn_s "$M" "docker stack ls --format '{{.Name}}'" | grep -qx "$1"; }
tn_stack_gone() { [ -z "$(tn_states "$1")" ]; }
tn_stack_rm() {
	tn_s "$M" "docker stack rm $1 >/dev/null 2>&1; true"
	tn_wait_for 120 tn_stack_gone "$1" || echo "NOTE: stack $1 still has endpoints after 120 s"
}

# tn_deploy STACK SECRET: docker stack deploy of the stack file on stdin, on
# $M, with config.sh's settings and, unless SECRET is "-",
# $TSLINK_TEST_DIR/SECRET.secret as TSLINK_OAUTH_SECRET, as test/cluster's
# deploy does it
tn_deploy() {
	cat >"$TN_WORK/$1.yml"
	scp -q -F "$TSLINK_TEST_SSH_CONFIG" "$TN_WORK/$1.yml" "$M:/root/$1.yml" || return 1
	{
		printf 'TSLINK_TEST_TAG=%q\nTSLINK_TEST_SVC=%q\nTSLINK_TEST_MTU=%q\n' \
			"$TSLINK_TEST_TAG" "$TSLINK_TEST_SVC" "$TSLINK_TEST_MTU"
		if [ "$2" != - ]; then
			printf 'TSLINK_OAUTH_SECRET='
			cat "$TSLINK_TEST_DIR/$2.secret"
		fi
	} | tn_s "$M" "set -a; . /dev/stdin; set +a; docker stack deploy -d -c /root/$1.yml $1"
}

# tn_plugin_set SETTING=VALUE...: sets the plugin settings on every node,
# restarting the plugin, and waits until the tasks' tailscaleds run again
tn_plugin_set() {
	local n
	for n in $("$C/nodes"); do
		tn_s "$n" "docker plugin disable -f tslink:latest >/dev/null &&
			docker plugin set tslink:latest $* && docker plugin enable tslink:latest >/dev/null" ||
			echo "NOTE: could not set $* on $n"
	done
	tn_wait_for 300 tn_healthy || echo "NOTE: not every task's tailscaled runs 300 s after setting $*"
}
tn_healthy() { ! "$C/health" | grep -qv 'bad: none$'; }

# tn_network NAME SECRET TAG: a swarm-scoped tslink network outside any stack,
# created on $M through the Docker API, its credential an OAuth client of
# $TSLINK_TEST_DIR, which travels on stdin
tn_network() {
	jq -n --arg name "$1" --rawfile s "$TSLINK_TEST_DIR/$2.secret" --arg tag "$3" \
		--arg mtu "$TSLINK_TEST_MTU" '{Name: $name, Driver: "tslink:latest", Scope: "swarm",
		Attachable: true, Options: {"tslink.authkey": (($s | rtrimstr("\n")) +
		"?ephemeral=true&preauthorized=true"), "tslink.tags": $tag,
		"com.docker.network.driver.mtu": $mtu}}' |
		tn_s "$M" "curl -fsS --unix-socket /var/run/docker.sock -H 'Content-Type: application/json' \
			-d @- http://docker/networks/create >/dev/null"
}
