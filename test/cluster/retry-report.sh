#!/bin/sh
# retry-report.sh <epoch>: summarise probe-retry.sh's log relative to an event at <epoch>
awk -v T="$1" '$3 != 200 {
	f++; if (!fs) fs = $1; fe = $1
	if ($5 != 200) { r++; if (!rs) rs = $1; re = $1; if ($6 != 200) a++ }
} END {
	printf "first attempt failed %d [%+d..%+d s]; after a VIP retry %d [%+d..%+d s]; after the fallback %d\n", f, fs - T, fe - T, r, rs - T, re - T, a
}' /tmp/probe.log
