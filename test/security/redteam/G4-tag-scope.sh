#!/bin/bash
# Guards: G4
# Testbed: tn
# Attacker: A3, the author of a stack file that passed review.
# Assumes: E1, E2, E3, E4, E8
# Attack: stack "a" asks the cluster credential for "tag:a-b", stack "a-b"'s base tag: on the
#   bed, stack tslink-test (the tag prefix of config.sh) asks for tag:tslink-test-callee, the
#   one tag the cluster credential may mint, with test/cluster's tslink-test-scope stack file.
#   It gets through if the task's endpoint runs.
# Control: the plugin setting TSLINK_TAG_SCOPE=prefix on every node. Teardown: exact, the
#   default, on every node.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
STACK=$TN_BASE
tn_stack_exists "$STACK" && tn_error "a stack $STACK is deployed: remove it first"
tn_on_exit "stack_rm $STACK"

settled() { tn_states "$STACK" | grep -qE '^(running|failed)'; }
# claims_tag: deploys the stack, and whether its endpoint runs; removes it
claims_tag() {
	local st
	tn_deploy "$STACK" - <"$C/stacks/tslink-test-scope.yml" >/dev/null || return 1
	wait_for 180 settled
	st=$(tn_states "$STACK" | head -n 1)
	echo "stack $STACK: ${st:-no endpoint}"
	stack_rm "$STACK"
	[[ $st == running:* ]]
}

SCOPE=exact
scope() {
	SCOPE=$1
	tn_plugin_set TSLINK_TAG_SCOPE="$1"
}
restore_scope() { [ "$SCOPE" = exact ] || scope exact; }
tn_on_exit restore_scope

probe_group "the cluster credential's tag scope" "scope prefix" "scope exact" 0
probe G4 "stack $STACK gets $T-callee from the cluster credential" claims_tag
run_probes
probe_exit
