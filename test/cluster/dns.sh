#!/bin/sh
# dns.sh <name>...: "ok" per name that resolves, "FAIL(name)" otherwise
fail=0
for q in "$@"; do
	if getent hosts "$q" >/dev/null 2>&1 || nslookup "$q" >/dev/null 2>&1; then
		printf 'ok '
	else
		printf 'FAIL(%s) ' "$q"
		fail=1
	fi
done
echo
exit "$fail"
