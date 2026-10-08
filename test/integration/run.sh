#!/usr/bin/env bash
# End-to-end test against a headscale control server on this host: builds
# tslink from source, installs it as the plugin "tslink", and checks tailnet
# reachability, ACLs, DNS upstreams, plugin restarts and ephemeral nodes.
# Needs Docker, sudo, curl and jq; no Tailscale account. It replaces the
# host's plugin "tslink" and runs a tailscaled of its own on the host, so run
# it on a disposable machine such as CI, without another tailscaled.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
PLUGIN=tslink
DATA=/var/lib/docker-plugins/tailscale
HEADSCALE_IMAGE=docker.io/headscale/headscale:0.29.4@sha256:8833f828b414c0907b7e5c71da76473216fe17cce0818a166b536ec552c0903f
ALPINE=alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
PYTHON=python:3.13-alpine@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a
MARK=tslink-e2e
# A name only the test's DNS server knows, and its address
PROBE_NAME=probe.tslink.test
PROBE_IP=192.0.2.53
# The containers' tailscaleds reach headscale through tslink's NAT to the
# host, so it must listen on an address other than loopback
HOST_IP=${HOST_IP:-$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')}
URL=http://$HOST_IP:8080
CONTAINERS=(e2e-server e2e-client e2e-outsider e2e-ephemeral e2e-late e2e-dns e2e-resolver e2e-sink e2e-raw e2e-hostsvc)
NETWORKS=(e2e-alice e2e-bob e2e-ephemeral)
WORK=$(mktemp -d)

log() { printf '\n=== %s\n' "$*"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
hs() { docker exec e2e-headscale headscale "$@"; }
# The host's own tailscaled, from the bundled binaries, on an interface of its
# own name: tslink keeps containers from the tailnet through any tailscale*
host_ts() { sudo "$WORK/plugin/rootfs/usr/local/bin/tailscale" --socket="$WORK/host-ts/sock" "$@"; }

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
# state_gone HOSTNAME: whether tslink deleted the hostname's state directory
state_gone() { ! sudo test -e "$DATA/by-hostname/$1"; }

# resolves CONTAINER [SERVER]: whether CONTAINER resolves $PROBE_NAME to $PROBE_IP
resolves() {
	docker exec "$1" nslookup -type=a "$PROBE_NAME" ${2:+"$2"} 2>/dev/null | grep -qF "$PROBE_IP"
}

dump_logs() {
	log "Diagnostics"
	docker ps -a || true
	hs nodes list || true
	echo "--- headscale"
	docker logs --tail 200 e2e-headscale 2>&1 || true
	echo "--- dnsmasq"
	docker logs --tail 50 e2e-dns 2>&1 || true
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
	if [ -n "${HOST_TS_PID:-}" ]; then
		sudo kill "$HOST_TS_PID" 2>/dev/null || true
		while sudo kill -0 "$HOST_TS_PID" 2>/dev/null; do sleep 1; done
	fi
	# The isolation outlives the plugin, so a restart never opens a window
	for ipt in iptables ip6tables; do
		for c in FORWARD:TSLINK-ISOLATE-FWD INPUT:TSLINK-ISOLATE-IN; do
			sudo "$ipt" -t mangle -D "${c%%:*}" -j "${c#*:}" 2>/dev/null || true
			sudo "$ipt" -t mangle -X "${c#*:}" 2>/dev/null || true
		done
	done
	if [ -n "${DNS_IFACE:-}" ]; then
		# shellcheck disable=SC2086 # one argument per domain
		sudo resolvectl domain "$DNS_IFACE" ${DNS_DOMAINS:-""} || true
	fi
	sudo rm -rf "$WORK"
	exit "$status"
}

if docker plugin inspect "$PLUGIN" >/dev/null 2>&1; then
	echo "A plugin named $PLUGIN is installed; remove it first: docker plugin rm -f $PLUGIN" >&2
	exit 1
fi
if pgrep -x tailscaled >/dev/null; then
	echo "A tailscaled is running; stop it first: the test runs one of its own" >&2
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
host=$(hs users create host -o json | jq -r .id)
key() { hs preauthkeys create --user "$1" --reusable --expiration 1h "${@:2}" | tail -n 1; }

log "Starting the host's own tailscaled"
# Like a cluster node's: on the tailnet, and granted access to alice's
# containers, so a container using the host's tailscaled would get through
mkdir -p "$WORK/host-ts"
# shellcheck disable=SC2024 # the log is ours, the daemon root's
sudo "$WORK/plugin/rootfs/usr/local/bin/tailscaled" --state="$WORK/host-ts/state" \
	--socket="$WORK/host-ts/sock" --tun=tailscale-e2e --port=0 >"$WORK/host-ts/log" 2>&1 &
HOST_TS_PID=$!
retry 30 sudo test -S "$WORK/host-ts/sock" || fail "host tailscaled did not start"
key "$host" | host_ts up --login-server="$URL" --auth-key=file:/dev/stdin --hostname=e2e-host
network() {
	# A plain "tslink" does not resolve: the driver is registered under the
	# plugin's full name
	docker network create --driver "$PLUGIN:latest" --opt tslink.loginserver="$URL" --opt tslink.authkey="$2" "${@:3}" "$1" >/dev/null
}
network e2e-alice "$(key "$alice")"
network e2e-bob "$(key "$bob")"
# headscale's keys do not say whether they are ephemeral: the option does
network e2e-ephemeral "$(key "$alice" --ephemeral)" --opt tslink.ephemeral=true

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

log "A persistent network's state is not marked ephemeral"
sudo test -s "$DATA/by-hostname/e2e-server/tailscaled.state" || fail "server has no state"
if sudo test -e "$DATA/by-hostname/e2e-server/ephemeral"; then
	fail "server's state is marked ephemeral"
fi

log "The ACL refuses a container it does not grant"
outsider_ip=$(wait_ip e2e-outsider)
echo "outsider $outsider_ip"
# The host's tailscaled may reach the server, but the outsider's sockets cannot
# use it (checked below): a refusal is the ACL's
if retry 30 reaches e2e-outsider "$server_ip"; then
	fail "outsider reached the server"
fi

log "Stopping a container logs its ephemeral node out and deletes its state"
docker run -d --name e2e-ephemeral --network e2e-ephemeral "$ALPINE" sleep 3600
wait_ip e2e-ephemeral
[ "$(nodes_named e2e-ephemeral)" = 1 ] || fail "ephemeral node not in headscale"
sudo test -e "$DATA/by-hostname/e2e-ephemeral/ephemeral" || fail "ephemeral node's state is not marked"
docker stop -t 1 e2e-ephemeral >/dev/null
# Well under headscale's node.ephemeral.inactivity_timeout (70s): only
# tslink's logout removes the node this soon
retry 20 node_gone e2e-ephemeral || fail "ephemeral node not logged out within 20s"
retry 20 state_gone e2e-ephemeral || fail "ephemeral node's state not deleted"

log "Tailscale's resolver forwards to the container's other DNS servers"
# On the host's network, like headscale, so tailscaled reaches it through
# tslink's NAT to the host. Docker asks the container's first DNS server,
# 100.100.100.100, and accepts its answer, so the name resolves only if the
# container's tailscaled forwards to the second.
docker run -d --name e2e-dns --network host "$ALPINE" sh -c \
	"apk add --no-cache dnsmasq >/dev/null && exec dnsmasq --keep-in-foreground --log-facility=- --log-queries \
	--no-resolv --no-hosts --bind-interfaces --listen-address=$HOST_IP --address=/$PROBE_NAME/$PROBE_IP"
retry 60 resolves e2e-dns "$HOST_IP" || fail "dnsmasq does not answer"
# A change of the host's DNS: systemd-resolved then replaces the file the
# plugin's resolv.conf is a bind mount of, and tailscaled's must still go over it
if systemctl is-active --quiet systemd-resolved; then
	DNS_IFACE=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "dev") print $(i + 1)}')
	DNS_DOMAINS=$(resolvectl domain "$DNS_IFACE" | sed 's/^[^:]*:[[:space:]]*//')
	# shellcheck disable=SC2086 # one argument per domain
	sudo resolvectl domain "$DNS_IFACE" $DNS_DOMAINS e2e.invalid
fi
docker run -d --name e2e-resolver --network e2e-alice --dns 100.100.100.100 --dns "$HOST_IP" "$ALPINE" sleep 3600
wait_ip e2e-resolver
# Asking 100.100.100.100 itself rules out Docker trying the second server
# because the first did not answer
retry 60 resolves e2e-resolver 100.100.100.100 || fail "Tailscale's resolver does not forward to the container's"
resolves e2e-resolver || fail "e2e-resolver cannot resolve $PROBE_NAME"

log "tailscaled does not reach the host's system bus"
# Through it, tailscaled would configure the host's systemd-resolved with the
# control server's DNS settings. It pings resolved as it starts.
if sudo grep -l "resolved-ping=yes" "$DATA"/by-hostname/*/tailscaled.log; then
	fail "tailscaled reached the host's systemd-resolved"
fi

log "A plugin restart keeps the containers' identities"
docker plugin disable -f "$PLUGIN"
docker plugin enable "$PLUGIN"
retry 180 reaches e2e-client "$server_ip" || fail "client cannot reach the server after the restart"
[ "$(tailnet_ip e2e-server)" = "$server_ip" ] || fail "server IP changed: $(tailnet_ip e2e-server)"
[ "$(tailnet_ip e2e-client)" = "$client_ip" ] || fail "client IP changed: $(tailnet_ip e2e-client)"
[ "$(nodes_named e2e-server)" = 1 ] || fail "server registered again"
[ "$(nodes_named e2e-client)" = 1 ] || fail "client registered again"

log "A network without containers during the restart takes new ones"
docker run -d --name e2e-late --network e2e-ephemeral "$ALPINE" sleep 3600
wait_ip e2e-late
retry 120 reaches e2e-late "$server_ip" || fail "late container cannot reach the server"

log "Containers do not reach the tailnet through the host's tailscaled"
# A UDP sink on the tailnet, which the host may reach
docker run -d --name e2e-sink --network e2e-alice "$PYTHON" python -u -c $'import socket\ns = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)\ns.bind(("", 9999))\nwhile True:\n    d, a = s.recvfrom(2048)\n    print(a[0], d.decode(errors="replace"), flush=True)'
sink_ip=$(wait_ip e2e-sink)
# A tslink container with Docker's default capabilities, CAP_NET_RAW included
docker run -d --name e2e-raw --network e2e-bob -v "$HERE/rawsend.py:/rawsend.py:ro" "$PYTHON" sleep 3600
wait_ip e2e-raw >/dev/null

# A service of the host's own, on its tailnet address only
host_ts_ip=$(host_ts ip -4)
docker run -d --name e2e-hostsvc --network host "$ALPINE" sh -c \
	"printf '#!/bin/sh\necho $MARK\n' >/reply && chmod +x /reply && exec nc -lk -s $host_ts_ip -p 8081 -e /reply"

# plain_reaches IP PORT: whether a container on Docker's default bridge, not
# on tslink, gets the answer of the service at IP:PORT on the tailnet
plain_reaches() {
	[ "$(docker run --rm "$ALPINE" sh -c "timeout 15 nc -w 5 $1 $2 </dev/null" 2>/dev/null)" = "$MARK" ]
}
# raw_send IFACE PAYLOAD: e2e-raw sends PAYLOAD to the sink as a raw frame to
# the gateway of IFACE: "veth" for tslink's, eth0 for docker_gwbridge
raw_send() {
	docker exec e2e-raw sh -c '
		if [ "$1" = veth ]; then
			ifc=$(ls /sys/class/net | grep "^veth")
			src=$(ip -4 -o addr show dev "$ifc" | awk "{split(\$4, a, \"/\"); print a[1]}")
			gw=${src%.*}.$((${src##*.} - 1))
		else
			ifc=$1
			src=$(ip -4 -o addr show dev "$ifc" | awk "{split(\$4, a, \"/\"); print a[1]}")
			gw=$(ip route | awk "/^default/ {print \$3}")
		fi
		ping -c 1 -W 1 "$gw" >/dev/null 2>&1
		mac=$(awk -v ip="$gw" "\$1 == ip {print \$4}" /proc/net/arp)
		python3 /rawsend.py "$ifc" "$src" "$mac" "$2" 9999 "$3"' sh "$1" "$sink_ip" "$2"
}
sink_got() { docker logs e2e-sink 2>&1 | grep -qF "$1"; }
# raw_arrives IFACE PAYLOAD: sends until the sink has PAYLOAD, for 30 seconds
raw_arrives() {
	local deadline=$((SECONDS + 30))
	until sink_got "$2"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		raw_send "$1" "$2"
		sleep 2
	done
}
set_isolation() {
	docker plugin disable -f "$PLUGIN"
	docker plugin set "$PLUGIN" TSLINK_ISOLATE_HOST_TAILNET="$1"
	docker plugin enable "$PLUGIN"
	retry 180 has_ip e2e-sink || fail "sink lost its tailnet IP after the restart"
}

docker run --rm --network host "$ALPINE" sh -c "timeout 15 nc -w 5 $server_ip 8080 </dev/null" |
	grep -qx "$MARK" || fail "precondition: the host cannot reach the server"
if plain_reaches "$server_ip" 8080; then
	fail "a container on Docker's bridge reached the tailnet through the host"
fi
if plain_reaches "$host_ts_ip" 8081; then
	fail "a container on Docker's bridge reached the host's tailnet address"
fi
for ifc in veth eth0; do
	if raw_arrives "$ifc" "isolated-$ifc"; then
		fail "raw frames through $ifc reached the tailnet: $(docker logs e2e-sink 2>&1 | tail -n 1)"
	fi
done

log "Without the isolation, the same probes get through"
# Shows that the probes above would catch a leak
set_isolation false
retry 60 plain_reaches "$server_ip" 8080 || fail "control: a container on Docker's bridge cannot reach the server"
plain_reaches "$host_ts_ip" 8081 || fail "control: a container on Docker's bridge cannot reach the host's tailnet address"
for ifc in veth eth0; do
	raw_arrives "$ifc" "open-$ifc" || fail "control: raw frames through $ifc did not arrive"
done
set_isolation true
if plain_reaches "$server_ip" 8080; then
	fail "the isolation did not come back with the setting"
fi

log "A container cannot store Taildrop files on the host"
# The sink's node is untagged, so its own container may send it files. They
# would land in its state directory on the host: tailscaled must refuse them.
api=$(sudo grep -o "peerapi: serving on http://$sink_ip:[0-9]*" "$DATA/by-hostname/e2e-sink/tailscaled.log" |
	tail -n 1 | sed 's/.*on //')
[ -n "$api" ] || fail "no peerapi address for the sink in its tailscaled.log"
# peer_status METHOD PATH: the HTTP status of the sink's own peerapi
peer_status() {
	docker exec e2e-sink python3 -c 'import sys, urllib.request, urllib.error
req = urllib.request.Request(sys.argv[1] + sys.argv[3], data=b"tslink-e2e-taildrop" if sys.argv[2] == "PUT" else None, method=sys.argv[2])
try:
    print(urllib.request.urlopen(req, timeout=10).status)
except urllib.error.HTTPError as e:
    print(e.code)' "$api" "$1" "$2"
}
# The peerapi answers the container, so a refusal below is Taildrop's
[ "$(peer_status GET /)" = 200 ] || fail "precondition: the sink's container cannot reach its peerapi"
put=$(peer_status PUT /v0/put/e2e-taildrop.txt)
case $put in 2*) fail "Taildrop accepted a file from the container ($put)" ;; esac
if sudo grep -rqs tslink-e2e-taildrop "$DATA"; then
	fail "a Taildrop file reached the host: $(sudo grep -rls tslink-e2e-taildrop "$DATA")"
fi

log "All end-to-end tests passed"
