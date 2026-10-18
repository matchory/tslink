#!/bin/bash
# Guards: G4
# Testbed: tn
# Attacker: A3, the author of a stack file that passed review.
# Assumes: E1, E2, E3, E4, E8
# Attack: stack B (rt-intruder) attaches to stack A's network as an external network: the
#   callee stack's callee_tailnet, with callee's credential and tag, as test/cluster's intruder
#   stack does. It gets through if B's task gets a running endpoint.
# Control: B attaches to a network without the stack label: rt_open, created outside any stack
#   with the callee credential and tag. Teardown: rt_open is removed and B points at
#   callee_tailnet again.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
B=rt-intruder
NET=callee_tailnet
tn_stack_exists callee || tn_error "the callee stack is not deployed"
tn_stack_exists "$B" && tn_error "a stack $B is deployed: remove it first"
tn_on_exit "stack_rm $B"
tn_on_exit "tn_s $M 'docker network rm rt_open >/dev/null 2>&1; true'"

settled() { tn_states "$B" | grep -qE '^(running|failed)'; }
# attaches: deploys B on $NET, and whether its task's endpoint runs; removes it
attaches() {
	local st
	tn_deploy "$B" - >/dev/null <<EOF || return 1
services:
  sneak:
    image: alpine:3.23
    command: ["sh","-c","while :; do sleep 3600; done"]
    networks: [target]
networks:
  target:
    external: true
    name: $NET
EOF
	wait_for 180 settled
	st=$(tn_states "$B" | head -n 1)
	echo "stack $B on $NET: ${st:-no endpoint}"
	stack_rm "$B"
	[[ $st == running:* ]]
}
open_network() {
	tn_network rt_open callee "$T-callee" || echo "NOTE: could not create rt_open"
	NET=rt_open
}
close_network() {
	NET=callee_tailnet
	tn_s "$M" "docker network rm rt_open >/dev/null"
}

probe_group "another stack's network" open_network close_network 0
probe G4 "stack $B joins stack callee's network" attaches
run_probes
probe_exit
