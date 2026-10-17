#!/bin/bash
# redteam-tn.sh: run the probes of test/security/redteam/ whose header says
# "Testbed: tn" against the Swarm bed of test/cluster, from this machine, as
# tn.sh describes; install the commit under test on the bed first
# (test/cluster/install-plugin.sh). Writes a report to
# $TSLINK_TEST_DIR/reports/tn-<time>-<commit>/, in redteam.sh's shape, which
# report.sh reads, and exits non-zero if a probe failed, broke or errored.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=lib.sh
. "$B/lib.sh"
C=$(cd "$B/../cluster" && pwd)
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
[ $# = 0 ] || {
	echo "usage: redteam-tn.sh" >&2
	exit 2
}
commit=$(git -C "$B" rev-parse --short HEAD)
dirty=$(git -C "$B" status --porcelain | head -c1)
OUT=$TSLINK_TEST_DIR/reports/tn-$(date -u +%Y%m%dT%H%M%SZ)-$commit
mkdir -p "$OUT"

results=()
for p in "$B"/redteam/*.sh; do
	grep -q '^# Testbed: tn$' "$p" || continue
	name=$(basename "$p" .sh)
	bash "$p" >"$OUT/$name.log" 2>&1
	results+=("$(probe_label $?) $name")
done

M=${MGR:-mgr}
{
	echo "# Red team on the Swarm bed, $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo
	echo "Commit: $commit${dirty:+ (with uncommitted changes)}"
	echo "Bed: $("$C/nodes" 2>/dev/null | tr '\n' ' ')Docker $("$C/s" "$M" docker version \
		--format '{{.Server.Version}}' 2>/dev/null)"
	echo
	printf '    %s\n' "${results[@]}"
} >"$OUT/summary.md"
cat "$OUT/summary.md"
echo "Report: $OUT"
! printf '%s\n' "${results[@]}" | grep -qE '^(FAIL|BROKEN|ERROR)'
