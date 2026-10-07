#!/bin/sh
# reach.sh <ip>...: "ok" per target that answers HTTP, "refused" otherwise
for t in "$@"; do
	if wget -q -T4 -O /dev/null "http://$t/" 2>/dev/null; then printf 'ok '; else printf 'refused '; fi
done
echo
