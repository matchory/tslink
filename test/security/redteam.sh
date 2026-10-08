#!/bin/bash
# redteam.sh [--tree DIR] [--probes DIR]: run the security probes on the
# testbed (test/security/testbed): the full end-to-end test, then each probe
# script of test/security/redteam/ and of DIR, which holds probes kept out of
# the repository. Writes a report to $TSLINK_TEST_DIR/reports/ and exits
# non-zero if a probe failed, broke or errored.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
S=$B/testbed/s
TREE=$(cd "$B/../.." && pwd)
PROBES=""
usage() {
	echo "usage: redteam.sh [--tree DIR] [--probes DIR]" >&2
	exit 2
}
while [ $# -gt 0 ]; do
	case $1 in
	--tree)
		[ $# -ge 2 ] || usage
		TREE=$(cd "$2" 2>/dev/null && pwd) || {
			echo "--tree $2: no such directory" >&2
			exit 2
		}
		shift 2
		;;
	--probes)
		[ $# -ge 2 ] || usage
		PROBES=$(cd "$2" 2>/dev/null && pwd) || {
			echo "--probes $2: no such directory" >&2
			exit 2
		}
		shift 2
		;;
	*) usage ;;
	esac
done

host=$("$S" hostname) || {
	echo "the testbed does not answer" >&2
	exit 2
}
if [ "$host" != tslink-security ]; then
	echo "refusing to run on $host: the testbed is tslink-security" >&2
	exit 2
fi
commit=$(git -C "$TREE" rev-parse --short HEAD)
dirty=$(git -C "$TREE" status --porcelain | head -c1)
OUT=$TSLINK_TEST_DIR/reports/redteam-$(date -u +%Y%m%dT%H%M%SZ)-$commit
mkdir -p "$OUT"
# A failed sync would leave the previous tree on the testbed, and the report
# would credit its results to this commit
if ! "$B/testbed/sync.sh" "$TREE"; then
	echo "could not copy $TREE to the testbed" >&2
	exit 2
fi

# reset: what an interrupted run may have left on the testbed. Single-quoted:
# this is a script for the remote shell, and must not expand locally.
# shellcheck disable=SC2016
"$S" 'pkill -x tailscaled; sleep 2
	docker rm -f $(docker ps -aq) >/dev/null 2>&1
	docker network prune -f >/dev/null
	docker plugin disable -f tslink >/dev/null 2>&1; docker plugin rm -f tslink >/dev/null 2>&1
	for ipt in iptables ip6tables; do
		for c in FORWARD:TSLINK-ISOLATE-FWD INPUT:TSLINK-ISOLATE-IN FORWARD:TSLINK-VETH-FWD; do
			$ipt -t mangle -D "${c%%:*}" -j "${c#*:}" 2>/dev/null
			$ipt -t mangle -F "${c#*:}" 2>/dev/null; $ipt -t mangle -X "${c#*:}" 2>/dev/null
		done
	done
	rm -rf /var/lib/docker-plugins/tailscale /root/probes; true'

results=()
"$S" 'cd /root/tslink && test/integration/run.sh' >"$OUT/run.log" 2>&1
case $? in 0) results+=("PASS    end-to-end test") ;; *) results+=("FAIL    end-to-end test (run.log)") ;; esac

probes=("$B"/redteam/*.sh)
[ -n "$PROBES" ] && probes+=("$PROBES"/*.sh)
for p in "${probes[@]}"; do
	[ -f "$p" ] || continue
	name=$(basename "$p" .sh)
	if ! "$S" 'mkdir -p /root/probes && cat >/root/probes/p.sh && chmod +x /root/probes/p.sh' <"$p"; then
		results+=("ERROR   $name (upload failed)")
		continue
	fi
	"$S" 'cd /root/tslink && /root/probes/p.sh' >"$OUT/$name.log" 2>&1
	case $? in
	0) results+=("PASS    $name") ;;
	1) results+=("FAIL    $name") ;;
	2) results+=("BROKEN  $name") ;;
	3) results+=("SKIPPED $name") ;;
	*) results+=("ERROR   $name") ;;
	esac
done

{
	echo "# Red team, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo
	echo "Commit: $commit${dirty:+ (with uncommitted changes)}"
	echo "Testbed: $host, Docker $("$S" docker version --format '{{.Server.Version}}'), $("$S" uname -r)"
	echo "Private probes: ${PROBES:-none}"
	echo
	printf '    %s\n' "${results[@]}"
} >"$OUT/summary.md"
cat "$OUT/summary.md"
echo "Report: $OUT"
! printf '%s\n' "${results[@]}" | grep -qE '^(FAIL|BROKEN|ERROR)'
