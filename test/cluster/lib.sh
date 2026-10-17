# shellcheck shell=bash
# lib.sh: helpers of regress.sh and test/security/tn.sh, sourced after config.sh. $M is the
# manager they run stack commands on.
CL=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
# wait_for <seconds> <command...>: run the command every 5 s until it succeeds, at most that long
wait_for() {
	local end=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -ge "$end" ] && return 1
		sleep 5
	done
}
# eps: endpoints.sh on every node, each line led by the node: node, endpoint, stack, hostname,
# state, advertised Services, error, tab-separated
eps() {
	local n
	for n in $("$CL/nodes"); do
		"$CL/s" "$n" sh -s <"$CL/endpoints.sh" | awk -v n="$n" '{ print n "\t" $0 }'
	done
}
# gone <stack>: the stack has no endpoints left
gone() { [ -z "$(eps | awk -F'\t' -v s="$1" '$3 == s')" ]; }
stack_rm() {
	"$CL/s" "$M" "docker stack rm $1 >/dev/null 2>&1 || true"
	wait_for 120 gone "$1" || echo "NOTE  stack $1 still has endpoints after 120 s"
}
replicas() { "$CL/s" "$M" "docker service inspect $1 --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null"; }
scale() { [ -z "$2" ] || "$CL/s" "$M" "docker service scale -d $1=$2 >/dev/null"; }
