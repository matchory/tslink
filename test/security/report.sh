#!/bin/bash
# report.sh: the security evidence for releasing the current commit, as
# docs/releasing.md lists it. Writes $TSLINK_TEST_DIR/reports/release-<commit>.md
# and exits non-zero if any evidence is missing.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$B/../.." && pwd)
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
cd "$ROOT" || exit 2
sha=$(git rev-parse HEAD)
short=$(git rev-parse --short HEAD)
OUT=$TSLINK_TEST_DIR/reports/release-$short.md
mkdir -p "$TSLINK_TEST_DIR/reports"
miss() { echo "MISSING $*"; }

{
	echo "# Release evidence for $short"
	echo
	echo "Commit: $sha ($(git describe --tags --always))"
	echo "Tailscale: $(grep -oE 'tailscale/tailscale:v[0-9.]+' docker/Dockerfile | head -n 1)"
	echo
	echo "## CI on this commit"
	ci=$(gh run list --commit "$sha" --json name,conclusion --jq '.[] | "\(.conclusion) \(.name)"')
	# shellcheck disable=SC2001 # indenting every line of $ci, not a single substitution
	if [ -z "$ci" ]; then miss "no CI run for $short"; else echo "$ci" | sed 's/^/    /'; fi
	if [ -n "$ci" ] && echo "$ci" | grep -qvE '^success '; then
		miss "a CI run on $short did not succeed"
	fi
	echo
	echo "## Fuzzing"
	fuzz=$(gh run list --workflow fuzz.yml --branch main --limit 1 --json conclusion,createdAt \
		--jq '.[0] | "\(.conclusion) \(.createdAt)"')
	echo "    last run: ${fuzz:-none}"
	week_ago=$(date -u -v-7d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ)
	case $fuzz in
	success\ *) [[ ${fuzz#success } > $week_ago ]] || miss "no successful fuzz run in the last 7 days" ;;
	*) miss "the last fuzz run did not succeed" ;;
	esac
	echo
	echo "## Guarantees and their tests"
	go test -count=1 -v -run TestRepository ./internal/secmodel/ 2>&1 |
		grep -oE 'G[0-9]+: .*' | sed 's/^/    /' || miss "the coverage check failed"
	echo
	echo "## Red team"
	# shellcheck disable=SC2012 # redteam.sh's own directory names, no need for find
	latest=$(ls -d "$TSLINK_TEST_DIR"/reports/redteam-* 2>/dev/null | sort | tail -n 1)
	if [ -z "$latest" ]; then
		miss "no red-team report"
	else
		sed 's/^/    /' "$latest/summary.md"
		case $latest in *-"$short") ;; *) miss "the newest red-team report is not for $short" ;; esac
		if grep -qE '^ *(FAIL|BROKEN|ERROR)' "$latest/summary.md"; then
			miss "the red-team run has failures"
		fi
	fi
} | tee "$OUT"
echo "Report: $OUT"
! grep -q '^MISSING' "$OUT"
