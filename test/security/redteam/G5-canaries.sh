#!/bin/bash
# Guards: G5
# Testbed: tn
# Attacker: A1, a process in a container on a tslink network; A2, a tailnet peer; A5, the
#   control server: whoever reads where a credential leaked to.
# Assumes: E2, E3
# Attack: at TSLINK_LOG_LEVEL=debug on every node, the bed's credentials are used: the caller
#   stack's tasks are replaced (keys minted from its OAuth client), and a stack joins a network
#   on an auth key the probe mints with the description tslink-test-redteam-g5 (its canary).
#   Then every node is searched for any Tailscale key or OAuth client secret (tskey-...), which
#   takes in the cluster credential, the stacks' OAuth clients, the canary key and every key
#   tslink minted, in: the plugin's data directory (plugin.log, every tailscaled.log and
#   tailscaled.state, status/*), the base64-decoded values of every tailscaled.state (a JSON
#   map of base64 values, which a plain search never matches), the shared certificate directory, the output of tslink diag,
#   /proc/<pid>/environ and cmdline of the plugin and of every tailscaled, and every container's
#   filesystem, PID 1's environment and docker inspect output. Allowed: oauth-client.secret,
#   TS_AUTHKEY in the plugin's own environment (not in a tailscaled's), and Docker's network
#   options, which are not searched. A hit names the place, never the key. Without a control,
#   and so outside lib.sh: the peerapi's /v0/env of each task node, as a debug-capable peer, the
#   manager's own tailscaled (tag:tslink-test-node).
# Needs: for the peerapi check, a tailnet policy that makes the node tag a debug-capable peer
#   of the task nodes: a grant from tag:tslink-test-node to tag:tslink-test-callee (the tag
#   prefix of config.sh) with "ip": ["tcp:*"], the peerapi's port being any, and "app":
#   {"https://tailscale.com/cap/debug-peer": [{}]}, and nodeAttrs giving
#   tag:tslink-test-callee the attribute "https://tailscale.com/cap/debug", without which a
#   node serves no debug endpoint. Grants to the caller and nogrant tags are checked as well if
#   present. Without such a grant, no peer answers: the probe then reports INFO, saying that
#   /v0/env was not checked, never PASS.
# Control: a fake key planted on the manager in each place: a file in the data directory, a
#   tailscaled.state with it base64-encoded as a value, a status file (which tslink diag shows), a process named tailscaled with it in its
#   environment, and a container with it in a label and a file. Teardown: all removed.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
tn_stack_exists caller || tn_error "the caller stack is not deployed"
tn_stack_exists rt-canary && tn_error "a stack rt-canary is deployed: remove it first"
RE='tskey-[a-z]+-[A-Za-z0-9]{6,}-[A-Za-z0-9]{6,}'

# search MODE: the places of MODE on a node that hold a key, one per line.
# A script for the node's bash; RE comes as its first argument.
# shellcheck disable=SC2016 # runs on the node
SEARCH='RE=$1 D=/var/lib/docker-plugins/tailscale
hit() { grep -aqE "$RE"; }
case $2 in
data)
	for d in "$D" /mnt/tslink; do
		[ -d "$d" ] && grep -rlaE "$RE" --exclude=oauth-client.secret "$d"
	done
	;;
state)
	# tailscaled.state maps each key to its value in base64
	for d in "$D" /mnt/tslink; do
		[ -d "$d" ] || continue
		find "$d" -name tailscaled.state -type f -print0 | while IFS= read -r -d "" f; do
			jq -r ".[] | @base64d" "$f" 2>/dev/null | hit && echo "$f, decoded"
		done
	done
	;;
processes)
	# The plugin setting TS_AUTHKEY is allowed in the plugin environment only:
	# tslink keeps it out of every tailscaled environment
	for p in $(pgrep -x tslink); do
		tr "\0" "\n" <"/proc/$p/environ" | grep -v "^TS_AUTHKEY=" | hit && echo "environ of the plugin ($p)"
		tr "\0" "\n" <"/proc/$p/cmdline" | hit && echo "cmdline of the plugin ($p)"
	done
	for p in $(pgrep -x tailscaled); do
		tr "\0" "\n" <"/proc/$p/environ" | hit && echo "environ of tailscaled $p"
		tr "\0" "\n" <"/proc/$p/cmdline" | hit && echo "cmdline of tailscaled $p"
	done
	;;
diag)
	tar -C /root/plugin/rootfs -c . | docker import -q - tslink:redteam >/dev/null &&
		docker run --rm -v "$D:/data:ro" --entrypoint /tslink tslink:redteam diag 2>&1 | hit && echo "tslink diag"
	docker image rm -f tslink:redteam >/dev/null 2>&1
	;;
containers)
	for c in $(docker ps -q); do
		n=$(docker inspect -f "{{.Name}}" "$c")
		docker inspect "$c" | hit && echo "docker inspect $n"
		p=$(docker inspect -f "{{.State.Pid}}" "$c")
		tr "\0" "\n" <"/proc/$p/environ" | hit && echo "environment of $n"
		find "/proc/$p/root/" -xdev -type f -size -4M -print0 2>/dev/null |
			xargs -0 -r grep -laE "$RE" 2>/dev/null | sed "s|^/proc/$p/root|$n:|"
	done
	;;
esac
true'
# found MODE: whether a node holds a key in a place of MODE; prints them
found() {
	local n out any=1
	for n in $("$C/nodes"); do
		out=$(tn_s "$n" bash -s -- "'$RE'" "$1" <<<"$SEARCH")
		[ -n "$out" ] || continue
		# shellcheck disable=SC2001 # prefixing every line
		sed "s/^/$n: /" <<<"$out"
		any=0
	done
	return "$any"
}

# Activity at debug level
tn_on_exit 'tn_plugin_set TSLINK_LOG_LEVEL=info'
tn_plugin_set TSLINK_LOG_LEVEL=debug
"$C/tsapi" POST "/tailnet/$TN/keys" "$(jq -nc --arg t "$T-caller" '{capabilities: {devices: {create:
	{reusable: false, ephemeral: true, preauthorized: true, tags: [$t]}}}, expirySeconds: 3600,
	description: "tslink-test-redteam-g5"}')" | jq -r '.key // empty' >"$TN_WORK/canary.key"
[ -s "$TN_WORK/canary.key" ] || tn_error "could not mint the canary key"
revoke_canary() {
	local id
	for id in $("$C/tsapi" GET "/tailnet/$TN/keys?all=true" |
		jq -r '.keys[] | select(.description == "tslink-test-redteam-g5") | .id'); do
		"$C/tsapi" DELETE "/tailnet/$TN/keys/$id" >/dev/null
	done
}
tn_on_exit revoke_canary
jq -n --rawfile k "$TN_WORK/canary.key" --arg tag "$T-caller" --arg mtu "$TSLINK_TEST_MTU" \
	'{Name: "rt_canary", Driver: "tslink:latest", Scope: "swarm", Attachable: true,
	Options: {"tslink.authkey": ($k | rtrimstr("\n")), "tslink.tags": $tag,
	"com.docker.network.driver.mtu": $mtu}}' |
	tn_s "$M" "curl -fsS --unix-socket /var/run/docker.sock -H 'Content-Type: application/json' \
		-d @- http://docker/networks/create >/dev/null" || tn_error "could not create rt_canary"
tn_on_exit "tn_s $M 'docker network rm rt_canary >/dev/null 2>&1; true'"
tn_on_exit "stack_rm rt-canary"
tn_deploy rt-canary - >/dev/null <<EOF || tn_error "could not deploy rt-canary"
services:
  client:
    image: alpine:3.23
    command: ["sh","-c","while :; do sleep 3600; done"]
    networks: [tailnet]
networks:
  tailnet:
    external: true
    name: rt_canary
EOF
tn_s "$M" "docker service update -d -q --force caller_client >/dev/null"
"$C/await" caller_client
canary_up() { tn_states rt-canary | grep -q '^running'; }
wait_for 180 canary_up || echo "NOTE: rt-canary's task does not run: $(tn_states rt-canary)"
wait_for 300 tn_healthy || echo "NOTE: not every task's tailscaled runs"

# The control's fake key, which matches RE
fake=tskey-auth-kRT$RANDOM${RANDOM}CNTRL-redteam$RANDOM$RANDOM
plant() {
	tn_s "$M" bash -s -- "$fake" <<'EOF'
D=/var/lib/docker-plugins/tailscale
echo "$1" >"$D/rt-canary"
mkdir -p "$D/rt-canary-state"
printf '{"rt-canary":"%s"}' "$(printf %s "$1" | base64 -w0)" >"$D/rt-canary-state/tailscaled.state"
printf '{"endpoint":"rtcanary0000","hostname":"rt-canary","state":"failed","attempts":1,%s}' \
	"\"updated\":\"2026-01-01T00:00:00Z\",\"error\":\"$1\"" >"$D/status/rtcanary0000.json"
mkdir -p /root/rt-canary && cp "$(command -v sleep)" /root/rt-canary/tailscaled
RT_CANARY=$1 nohup /root/rt-canary/tailscaled 3600 >/dev/null 2>&1 </dev/null &
docker run -d --name rt-canary-ctr --label "rt.canary=$1" alpine:3.23 \
	sh -c 'echo "$0" >/tmp/rt-canary; exec sleep 3600' "$1" >/dev/null
EOF
}
unplant() {
	# shellcheck disable=SC2016 # runs on the node
	tn_s "$M" 'D=/var/lib/docker-plugins/tailscale; rm -rf "$D/rt-canary" "$D/rt-canary-state" "$D/status/rtcanary0000.json"
		pkill -f /root/rt-canary/tailscaled; rm -rf /root/rt-canary; docker rm -f rt-canary-ctr >/dev/null 2>&1; true'
}
tn_on_exit unplant

probe_group "credentials" plant unplant 30
probe G5 "a key in the plugin's data directory or the shared certificate directory" found data
probe G5 "a key in a base64-decoded value of a tailscaled.state" found state
probe G5 "a key in the command line or environment of the plugin or a tailscaled" found processes
probe G5 "a key in tslink diag output" found diag
probe G5 "a key in a container or its docker inspect output" found containers
run_probes

# The peerapi's /v0/env, as the manager's tailscaled, a debug-capable peer
# if the policy makes it one (see Needs)
peers=$(tn_s "$M" 'tailscale status --json' | jq -r --arg t "$T" '.Peer[]
	| select(any(.Tags[]?; startswith($t + "-c") or . == $t + "-nogrant") and .Online)
	| .PeerAPIURL[0] // empty')
answered=0 refused=0 unreachable=0 leaked=0
for u in $peers; do
	code=$(tn_s "$M" "curl -s -m5 -o /tmp/rt-env -w '%{http_code}' $u/v0/env")
	case $code in
	200)
		answered=$((answered + 1))
		if tn_s "$M" "grep -aqE '$RE' /tmp/rt-env"; then
			echo "FAIL    G5 a key in the peerapi's /v0/env of $u"
			leaked=$((leaked + 1))
		fi
		;;
	403) refused=$((refused + 1)) ;;
	*) unreachable=$((unreachable + 1)) ;;
	esac
done
tn_s "$M" 'rm -f /tmp/rt-env'
echo "peerapi /v0/env of $(wc -w <<<"$peers" | tr -d ' ') task nodes: $answered answered, $refused refused" \
	"(not debug-capable), $unreachable unreachable, $leaked leaked a key"
[ "$leaked" = 0 ] || exit 1
if [ "$answered" = 0 ]; then
	echo "NOTE: no task node answered /v0/env to $M: the policy lacks the grant and attribute of Needs"
	[ "$PROBE_RESULT" = PASS ] &&
		probe_exit "the search found no key, but the peerapi's /v0/env was not checked: no peer could query it"
fi
probe_exit
