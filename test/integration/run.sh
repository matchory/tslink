#!/usr/bin/env bash
# End-to-end test against a headscale control server on this host: builds
# tslink from source, installs it as the plugin "tslink", and checks tailnet
# reachability, ACLs, plugin restarts and ephemeral nodes.
# Needs Docker, sudo, curl and jq; no Tailscale account. It replaces the
# host's plugin "tslink", so run it on a disposable machine such as CI.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
PLUGIN=tslink
DATA=/var/lib/docker-plugins/tailscale
HEADSCALE_IMAGE=docker.io/headscale/headscale:0.29.4@sha256:8833f828b414c0907b7e5c71da76473216fe17cce0818a166b536ec552c0903f
ALPINE=alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
MARK=tslink-e2e
# The containers' tailscaleds reach headscale through tslink's NAT to the
# host, so it must listen on an address other than loopback
HOST_IP=${HOST_IP:-$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')}
URL=http://$HOST_IP:8080
CONTAINERS=(e2e-server e2e-client e2e-outsider e2e-ephemeral)
NETWORKS=(e2e-alice e2e-bob e2e-ephemeral)
WORK=$(mktemp -d)

log() { printf '\n=== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
hs() { docker exec e2e-headscale headscale "$@"; }

# retry SECONDS CMD...: runs CMD every 2 seconds until it succeeds
retry() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 2
	done
}

# tailnet_ip CONTAINER: the container's Tailscale IPv4 address, if it has one
tailnet_ip() {
	docker exec "$1" ip -4 -o addr show dev tailscale0 2>/dev/null |
		awk '{split($4, a, "/"); print a[1]}'
}
has_ip() { [ -n "$(tailnet_ip "$1")" ]; }
wait_ip() {
	retry 180 has_ip "$1" || fail "$1 got no tailnet IP"
	tailnet_ip "$1"
}

# reaches CONTAINER IP: whether CONTAINER gets the server's answer from IP:8080
reaches() {
	local out
	out=$(docker exec "$1" sh -c "timeout 15 nc -w 5 $2 8080 </dev/null" 2>/dev/null || true)
	[ "$out" = "$MARK" ]
}

nodes_named() { hs nodes list -o json | jq --arg n "$1" '[(. // [])[] | select(.given_name == $n)] | length'; }
node_gone() { [ "$(nodes_named "$1")" = 0 ]; }

dump_logs() {
	log "Diagnostics"
	docker ps -a || true
	hs nodes list || true
	echo "--- headscale"
	docker logs --tail 200 e2e-headscale 2>&1 || true
	echo "--- $DATA/plugin.log"
	sudo tail -n 300 "$DATA/plugin.log" || true
	sudo sh -c "for f in $DATA/by-hostname/*/tailscaled.log; do echo \"--- \$f\"; tail -n 60 \"\$f\"; done; cat $DATA/status/*.json" || true
}

cleanup() {
	local status=$?
	[ "$status" -eq 0 ] || dump_logs
	docker rm -f "${CONTAINERS[@]}" e2e-headscale >/dev/null 2>&1 || true
	docker network rm "${NETWORKS[@]}" >/dev/null 2>&1 || true
	docker plugin disable -f "$PLUGIN" >/dev/null 2>&1 || true
	docker plugin rm -f "$PLUGIN" >/dev/null 2>&1 || true
	sudo rm -rf "$WORK"
	exit "$status"
}

if docker plugin inspect "$PLUGIN" >/dev/null 2>&1; then
	echo "A plugin named $PLUGIN is installed; remove it first: docker plugin rm -f $PLUGIN" >&2
	exit 1
fi
trap cleanup EXIT

log "Building the plugin"
docker build -t tslink:rootfs -f "$ROOT/docker/Dockerfile" "$ROOT"
mkdir -p "$WORK/plugin/rootfs"
cp "$ROOT/docker/config.json" "$WORK/plugin/"
id=$(docker create tslink:rootfs)
docker export "$id" | sudo tar -x -C "$WORK/plugin/rootfs"
docker rm "$id" >/dev/null
sudo mkdir -p "$DATA"
# The CLI packs the rootfs, which is root's like in the image
sudo docker plugin create "$PLUGIN" "$WORK/plugin"
# Docker creates /var/run/docker/netns with the first container
docker run --rm "$ALPINE" true
docker plugin enable "$PLUGIN"

log "Starting headscale at $URL"
docker run -d --name e2e-headscale --network host \
	--tmpfs /var/lib/headscale --tmpfs /var/run/headscale \
	-v "$HERE/headscale:/etc/headscale:ro" \
	-e HEADSCALE_SERVER_URL="$URL" \
	"$HEADSCALE_IMAGE" serve
retry 60 curl -fsS -o /dev/null "$URL/health" || fail "headscale did not come up"

alice=$(hs users create alice -o json | jq -r .id)
bob=$(hs users create bob -o json | jq -r .id)
key() { hs preauthkeys create --user "$1" --reusable --expiration 1h "${@:2}" | tail -n 1; }
network() {
	# A plain "tslink" does not resolve: the driver is registered under the
	# plugin's full name
	docker network create --driver "$PLUGIN:latest" --opt tslink.loginserver="$URL" --opt tslink.authkey="$2" "$1" >/dev/null
}
network e2e-alice "$(key "$alice")"
network e2e-bob "$(key "$bob")"
network e2e-ephemeral "$(key "$alice" --ephemeral)"

# The server answers every connection on port 8080 with $MARK
docker run -d --name e2e-server --network e2e-alice "$ALPINE" sh -c \
	"printf '#!/bin/sh\necho $MARK\n' >/reply && chmod +x /reply && exec nc -lk -p 8080 -e /reply"
docker run -d --name e2e-client --network e2e-alice "$ALPINE" sleep 3600
docker run -d --name e2e-outsider --network e2e-bob "$ALPINE" sleep 3600

log "Two containers reach each other over the tailnet"
server_ip=$(wait_ip e2e-server)
client_ip=$(wait_ip e2e-client)
echo "server $server_ip, client $client_ip"
retry 120 reaches e2e-client "$server_ip" || fail "client cannot reach the server"

log "The ACL refuses a container it does not grant"
outsider_ip=$(wait_ip e2e-outsider)
echo "outsider $outsider_ip"
# The host has no Tailscale, so only the outsider's own tailscaled could
# carry the connection: a refusal is the ACL's
if retry 30 reaches e2e-outsider "$server_ip"; then
	fail "outsider reached the server"
fi

log "A plugin restart keeps the containers' identities"
docker plugin disable -f "$PLUGIN"
docker plugin enable "$PLUGIN"
retry 180 reaches e2e-client "$server_ip" || fail "client cannot reach the server after the restart"
[ "$(tailnet_ip e2e-server)" = "$server_ip" ] || fail "server IP changed: $(tailnet_ip e2e-server)"
[ "$(tailnet_ip e2e-client)" = "$client_ip" ] || fail "client IP changed: $(tailnet_ip e2e-client)"
[ "$(nodes_named e2e-server)" = 1 ] || fail "server registered again"
[ "$(nodes_named e2e-client)" = 1 ] || fail "client registered again"

log "Stopping a container removes its ephemeral node"
docker run -d --name e2e-ephemeral --network e2e-ephemeral "$ALPINE" sleep 3600
wait_ip e2e-ephemeral
[ "$(nodes_named e2e-ephemeral)" = 1 ] || fail "ephemeral node not in headscale"
docker stop -t 1 e2e-ephemeral >/dev/null
# headscale deletes it after node.ephemeral.inactivity_timeout (70s)
retry 240 node_gone e2e-ephemeral || fail "ephemeral node still in headscale"

log "All end-to-end tests passed"
