#!/bin/sh
# endpoints.sh: on a node, one tab-separated line per tslink endpoint: its ID (12 characters),
# stack, hostname, status state, the Services its tailscaled advertises (from the LocalAPI's prefs,
# comma-separated) and its error; "-" for an empty field
D=/var/lib/docker-plugins/tailscale
for f in "$D"/status/*.json; do
	[ -f "$f" ] || continue
	id=$(basename "$f" .json)
	adv=$(curl -s -m5 --unix-socket "$D/sock/$id.sock" http://local-tailscaled.sock/localapi/v0/prefs |
		jq -r '(.AdvertiseServices // []) | sort | join(",")' 2>/dev/null)
	jq -r --arg id "$id" --arg adv "${adv:--}" \
		'[$id, (.stack // "-"), .hostname, .state, $adv, (.error // "-")] | @tsv' "$f"
done
