#!/bin/bash
# shellcheck disable=SC2329 # attacks and controls run through lib.sh's eval
# lib_test.sh: checks lib.sh's outcomes with attacks whose results are known
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$HERE/lib.sh"
FAILS=0
DEFENCE=on
defence_off() { DEFENCE=off; }
defence_on() { DEFENCE=on; }
stays_off() { :; }
blocked_by_defence() { [ "$DEFENCE" = off ]; }
always_through() { true; }
never_through() { false; }

# expect NAME WANT_STATUS WANT_OUTPUT: runs run_probes and compares
expect() {
	local out status
	out=$(run_probes)
	status=$?
	[ "$status" = 0 ] || status=1
	if [ "$status" != "$2" ] || [ "$out" != "$3" ]; then
		printf 'FAIL %s\n  want status %s, output:\n%s\n  got status %s, output:\n%s\n' \
			"$1" "$2" "$3" "$status" "$out"
		FAILS=$((FAILS + 1))
	else
		echo "ok   $1"
	fi
}

probe_group defended defence_off defence_on 0
probe GT "blocked" blocked_by_defence
expect "a defended attack passes" 0 $'\n--- probes: defended\nPASS    GT blocked'

probe_group leaky defence_off defence_on 0
probe GT "leaks" always_through
expect "an attack that gets through fails" 1 $'\n--- probes: leaky\nFAIL    GT leaks'

probe_group broken defence_off defence_on 0
probe GT "dead" never_through
expect "an attack that never gets through is broken" 1 \
	$'\n--- probes: broken\nBROKEN  GT dead: the control run did not get through'

probe_group sticky defence_off stays_off 0
probe GT "blocked" blocked_by_defence
expect "a defence that does not come back fails" 1 \
	$'\n--- probes: sticky\nPASS    GT blocked\nFAIL    GT blocked: the defence did not come back after the control run'

probe_group mixed defence_off defence_on 0
probe GT "blocked" blocked_by_defence
probe GT "dead" never_through
expect "one broken probe fails the group" 1 \
	$'\n--- probes: mixed\nPASS    GT blocked\nBROKEN  GT dead: the control run did not get through'

probe_group args defence_off defence_on 0
probe GT "with arguments" test "a b" = "a b"
expect "arguments with spaces reach the attack" 1 $'\n--- probes: args\nFAIL    GT with arguments'

echo "$FAILS failed"
exit "$FAILS"
