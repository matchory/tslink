#!/bin/sh
# version.sh [ref]: tslink's version, for the builds to stamp into the
# binary: the nearest release tag, with the commits since and the commit if
# not on it (v1.2.0-3-gabcdef0), or the commit alone before the first release.
# Of the working tree by default, marked -dirty if it has uncommitted changes;
# of ref if given.
set -eu
cd "$(dirname "$0")"
match='v[0-9]*.[0-9]*.[0-9]*'
if [ $# -eq 0 ]; then
	exec git describe --tags --always --dirty --match "$match"
fi
exec git describe --tags --always --match "$match" "$1"
