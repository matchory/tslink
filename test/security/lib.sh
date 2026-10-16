#!/bin/bash
# lib.sh: security probes with control runs. Source it, then:
#
#   probe_group NAME CONTROL_SETUP CONTROL_TEARDOWN [SETTLE_SECONDS]
#   probe G<n> DESCRIPTION COMMAND [ARGS...]
#   ...
#   run_probes || fail "NAME"
#
# COMMAND is an attack: it succeeds if it got through, and must bound its own
# run time. run_probes runs every attack of the group against the defended
# system, where it must fail; then CONTROL_SETUP, which disables the defence,
# and every attack again, each retried for up to SETTLE_SECONDS (default 60),
# where it must succeed; then CONTROL_TEARDOWN, and the first attack once
# more, which must fail again. A probe whose control run does not get through
# is BROKEN: it could not catch a regression. CONTROL_SETUP and
# CONTROL_TEARDOWN are shell commands, evaluated.
#
# run_probes prints PASS, FAIL or BROKEN per probe, with its guarantee and
# description, and returns non-zero unless every probe passed. It sets
# PROBE_RESULT to FAIL if a probe failed, else to BROKEN if one was broken,
# else to PASS; a probe script of test/security/redteam ends with probe_exit.

PROBE_GROUP=""
PROBE_SETUP=""
PROBE_TEARDOWN=""
PROBE_SETTLE=60
PROBE_IDS=()
PROBE_DESCS=()
PROBE_CMDS=()
PROBE_RESULT=""

probe_group() {
	PROBE_GROUP=$1
	PROBE_SETUP=$2
	PROBE_TEARDOWN=$3
	PROBE_SETTLE=${4:-60}
	PROBE_IDS=()
	PROBE_DESCS=()
	PROBE_CMDS=()
}

probe() {
	local id=$1 desc=$2
	shift 2
	PROBE_IDS+=("$id")
	PROBE_DESCS+=("$desc")
	PROBE_CMDS+=("$(printf '%q ' "$@")")
}

# probe_gets_through I: whether probe I's attack gets through
probe_gets_through() { eval "${PROBE_CMDS[$1]}"; }

run_probes() {
	local i deadline n=${#PROBE_CMDS[@]} failed=0 broken=0
	local -a blocked=() controlled=()
	printf '\n--- probes: %s\n' "$PROBE_GROUP"
	for ((i = 0; i < n; i++)); do
		blocked[i]=1
		if probe_gets_through "$i"; then blocked[i]=0; fi
	done
	eval "$PROBE_SETUP"
	for ((i = 0; i < n; i++)); do
		controlled[i]=0
		deadline=$((SECONDS + PROBE_SETTLE))
		until probe_gets_through "$i"; do
			[ "$SECONDS" -lt "$deadline" ] || continue 2
			sleep 2
		done
		controlled[i]=1
	done
	eval "$PROBE_TEARDOWN"
	for ((i = 0; i < n; i++)); do
		if [ "${blocked[i]}" = 0 ]; then
			echo "FAIL    ${PROBE_IDS[i]} ${PROBE_DESCS[i]}"
			failed=$((failed + 1))
		elif [ "${controlled[i]}" = 0 ]; then
			echo "BROKEN  ${PROBE_IDS[i]} ${PROBE_DESCS[i]}: the control run did not get through"
			broken=$((broken + 1))
		else
			echo "PASS    ${PROBE_IDS[i]} ${PROBE_DESCS[i]}"
		fi
	done
	if [ "$n" -gt 0 ] && [ "${blocked[0]}" = 1 ] && probe_gets_through 0; then
		echo "FAIL    ${PROBE_IDS[0]} ${PROBE_DESCS[0]}: the defence did not come back after the control run"
		failed=$((failed + 1))
	fi
	PROBE_RESULT=PASS
	[ "$broken" = 0 ] || PROBE_RESULT=BROKEN
	[ "$failed" = 0 ] || PROBE_RESULT=FAIL
	[ "$PROBE_RESULT" = PASS ]
}

# probe_exit [INFO]: exits with the code redteam.sh reads for PROBE_RESULT: 1
# for FAIL, 2 for BROKEN, else 0, or with INFO, a sentence, 4 for INFO: the
# probes passed, and their control run violated an environment property, so
# INFO says what an operator gives up who violates it.
probe_exit() {
	case $PROBE_RESULT in
	PASS) ;;
	FAIL) exit 1 ;;
	BROKEN) exit 2 ;;
	*) exit 5 ;;
	esac
	[ $# = 0 ] && exit 0
	echo "INFO    $1"
	exit 4
}
