# shellcheck shell=bash disable=SC2034 # its variables are the probes'
# t1.sh: the single-host testbed for the probes of redteam/, which source it
# as test/security/t1.sh: redteam.sh runs them as root with the working tree
# at /root/tslink as the current directory. t1_up builds the plugin "tslink"
# from the tree and installs it, and starts headscale with the end-to-end
# test's users (alice, bob, host) and policy, and the host's own tailscaled,
# logged in as host, which may reach alice's nodes. It sets t1_down as the
# EXIT trap, which runs the probe's t1_on_exit commands, removes all of it,
# the containers and networks named rt-* and tslink's iptables chains, and
# keeps the probe's exit status. A probe sets no EXIT trap of its own.
#
# Elsewhere than on the testbed, sourcing it exits SKIPPED (3).
if [ "$(hostname)" != tslink-security ] || [ ! -f test/integration/run.sh ]; then
	echo "SKIPPED: not on the security testbed, in its working tree"
	exit 3
fi
T1_ROOT=$PWD
T1_WORK=$(mktemp -d)
T1_PLUGIN=tslink
T1_DATA=/var/lib/docker-plugins/tailscale
T1_PIDS=()
T1_EXIT=()
# t1_pin NAME: the image run.sh pins as NAME, so the probes use the same ones
t1_pin() { sed -n "s/^$1=//p" "$T1_ROOT/test/integration/run.sh"; }
T1_ALPINE=$(t1_pin ALPINE)
T1_PYTHON=$(t1_pin PYTHON)
T1_HEADSCALE=$(t1_pin HEADSCALE_IMAGE)
# Not loopback: the containers' tailscaleds reach headscale through tslink's NAT
T1_HOST_IP=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
T1_URL=http://$T1_HOST_IP:8080

# t1_error MESSAGE: a failure of the setup, not of an attack: exits ERROR
t1_error() {
	echo "ERROR: $*" >&2
	exit 5
}

# t1_retry SECONDS CMD...: runs CMD every 2 seconds until it succeeds
t1_retry() {
	local deadline=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		sleep 2
	done
}

t1_hs() { docker exec rt-headscale headscale "$@"; }
t1_host_ts() { "$T1_WORK/plugin/rootfs/usr/local/bin/tailscale" --socket="$T1_WORK/host-ts/sock" "$@"; }

# t1_build DIR [TREE]: the plugin, built from TREE (default the working tree),
# in DIR
t1_build() {
	local id
	docker build -q -t tslink:redteam -f "${2:-$T1_ROOT}/docker/Dockerfile" "${2:-$T1_ROOT}" >/dev/null ||
		return 1
	rm -rf "$1" && mkdir -p "$1/rootfs" && cp "${2:-$T1_ROOT}/docker/config.json" "$1/" || return 1
	id=$(docker create tslink:redteam) || return 1
	docker export "$id" | tar -x -C "$1/rootfs"
	docker rm "$id" >/dev/null
}

# t1_install DIR: replaces the plugin with the one built in DIR. A plugin
# that has the setting TSLINK_LOGIN_SERVER gets headscale's URL there; t1_net
# passes it as a network option otherwise.
t1_install() {
	if docker plugin inspect "$T1_PLUGIN" >/dev/null 2>&1; then
		docker plugin disable -f "$T1_PLUGIN" >/dev/null 2>&1
		docker plugin rm -f "$T1_PLUGIN" >/dev/null || return 1
	fi
	docker plugin create "$T1_PLUGIN" "$1" >/dev/null || return 1
	if grep -q '"TSLINK_LOGIN_SERVER"' "$1/config.json"; then
		docker plugin set "$T1_PLUGIN" TSLINK_LOGIN_SERVER="$T1_URL" || return 1
	fi
	docker plugin enable "$T1_PLUGIN" >/dev/null
}

t1_up() {
	[ "$(id -u)" = 0 ] || t1_error "t1 probes run as root"
	if docker plugin inspect "$T1_PLUGIN" >/dev/null 2>&1 || pgrep -x tailscaled >/dev/null; then
		t1_error "a plugin $T1_PLUGIN or a tailscaled is running: redteam.sh's reset removes them"
	fi
	trap t1_down EXIT
	mkdir -p "$T1_DATA"
	t1_build "$T1_WORK/plugin" || t1_error "could not build the plugin"
	# Docker creates /var/run/docker/netns with the first container
	docker run --rm "$T1_ALPINE" true
	t1_install "$T1_WORK/plugin" || t1_error "could not install the plugin"
	docker run -d --name rt-headscale --network host \
		--tmpfs /var/lib/headscale --tmpfs /var/run/headscale \
		-v "$T1_ROOT/test/integration/headscale:/etc/headscale:ro" \
		-e HEADSCALE_SERVER_URL="$T1_URL" "$T1_HEADSCALE" serve >/dev/null
	t1_retry 60 curl -fsS -o /dev/null "$T1_URL/health" || t1_error "headscale did not come up"
	local u
	for u in alice bob host; do
		t1_hs users create "$u" >/dev/null || t1_error "could not create the user $u"
	done
	mkdir -p "$T1_WORK/host-ts"
	"$T1_WORK/plugin/rootfs/usr/local/bin/tailscaled" --state="$T1_WORK/host-ts/state" \
		--socket="$T1_WORK/host-ts/sock" --tun=tailscale-rt --port=0 >"$T1_WORK/host-ts/log" 2>&1 &
	T1_PIDS+=("$!")
	t1_retry 30 test -S "$T1_WORK/host-ts/sock" || t1_error "the host's tailscaled did not start"
	t1_key host | t1_host_ts up --login-server="$T1_URL" --auth-key=file:/dev/stdin --hostname=rt-host ||
		t1_error "the host's tailscaled did not log in"
}

# t1_key USER [FLAGS...]: a new pre-auth key of USER, also recorded in
# $T1_WORK/keys, which only root reads
t1_key() {
	local id
	id=$(t1_hs users list -o json | jq -r --arg n "$1" '.[] | select(.name == $n) | .id')
	(
		umask 077
		t1_hs preauthkeys create --user "$id" --reusable --expiration 1h "${@:2}" | tail -n 1 |
			tee -a "$T1_WORK/keys"
	)
}

# t1_net NAME USER [OPTION=VALUE...]: a tslink network on a new key of USER,
# created through the Docker API, so the key is on no command line
t1_net() {
	local key opts
	key=$(t1_key "$2") || return 1
	opts=$(printf '%s\n' "${@:3}" | jq -R 'select(. != "") | split("=") | {(.[0]): (.[1:] | join("="))}' |
		jq -s 'add // {}')
	grep -q '"TSLINK_LOGIN_SERVER"' "$T1_WORK/plugin/config.json" ||
		opts=$(jq --arg u "$T1_URL" '. + {"tslink.loginserver": $u}' <<<"$opts")
	jq -n --arg name "$1" --arg driver "$T1_PLUGIN:latest" --arg key "$key" --argjson opts "$opts" \
		'{Name: $name, Driver: $driver, Options: ({"tslink.authkey": $key} + $opts)}' |
		curl -fsS --unix-socket /var/run/docker.sock -H 'Content-Type: application/json' \
			-d @- http://docker/networks/create >/dev/null
}

# t1_ip CONTAINER [-6]: the container's Tailscale address, if it has one
t1_ip() {
	docker exec "$1" ip "${2:--4}" -o addr show dev tailscale0 scope global 2>/dev/null |
		awk '{split($4, a, "/"); print a[1]}' | head -n 1
}
t1_has_ip() { [ -n "$(t1_ip "$1")" ]; }
t1_wait_ip() { t1_retry 180 t1_has_ip "$1" || t1_error "$1 got no tailnet IP"; }
t1_pid() { docker inspect -f '{{.State.Pid}}' "$1"; }
# t1_veth CONTAINER: the container's address on its tslink veth
t1_veth() {
	nsenter -t "$(t1_pid "$1")" -n ip -4 -o addr show |
		awk '$2 ~ /^veth/ && $4 ~ /^10\.200\./ {split($4, a, "/"); print a[1]}'
}

# t1_hold_off IPTABLES TABLE CHAIN:JUMP...: keeps each CHAIN's jump to JUMP
# out until t1_hold_back: the watchdog restores them every minute, which
# would end a control run in its middle
T1_HOLD=""
t1_hold_off() {
	# shellcheck disable=SC2016 # a script for sh, with its own arguments
	sh -c 'ipt=$1 t=$2; shift 2; while :; do
		for c in "$@"; do "$ipt" -t "$t" -D "${c%%:*}" -j "${c#*:}" 2>/dev/null; done
		sleep 0.5
	done' sh "$@" &
	T1_HOLD="$!"
	T1_PIDS+=("$T1_HOLD")
}
# t1_hold_back IPTABLES TABLE CHAIN:JUMP...: the watchdog puts the jumps back
t1_hold_back() {
	local ipt=$1 t=$2 c
	shift 2
	[ -z "$T1_HOLD" ] || kill "$T1_HOLD" 2>/dev/null
	T1_HOLD=""
	for c in "$@"; do
		t1_retry 150 "$ipt" -t "$t" -C "${c%%:*}" -j "${c#*:}" ||
			echo "NOTE: the watchdog did not restore $ipt $c within 150 s"
	done
}

# t1_on_exit CMD: runs CMD, a shell command, when the probe exits, before
# the ones registered earlier and before t1_down removes the testbed
t1_on_exit() { T1_EXIT=("$1" "${T1_EXIT[@]}"); }

t1_down() {
	local status=$? p ipt c
	for c in "${T1_EXIT[@]}"; do eval "$c"; done
	for p in "${T1_PIDS[@]}"; do
		kill "$p" 2>/dev/null && while kill -0 "$p" 2>/dev/null; do sleep 1; done
	done
	docker ps -aq --filter name=^rt- | xargs -r docker rm -f >/dev/null 2>&1
	docker network ls -q --filter name=^rt- | xargs -r docker network rm >/dev/null 2>&1
	docker plugin disable -f "$T1_PLUGIN" >/dev/null 2>&1
	docker plugin rm -f "$T1_PLUGIN" >/dev/null 2>&1
	for ipt in iptables ip6tables; do
		for c in FORWARD:TSLINK-ISOLATE-FWD INPUT:TSLINK-ISOLATE-IN FORWARD:TSLINK-VETH-FWD INPUT:TSLINK-WG-IN; do
			"$ipt" -t mangle -D "${c%%:*}" -j "${c#*:}" 2>/dev/null
			"$ipt" -t mangle -F "${c#*:}" 2>/dev/null
			"$ipt" -t mangle -X "${c#*:}" 2>/dev/null
		done
	done
	iptables -t nat -D POSTROUTING -j TSLINK-WG-SNAT 2>/dev/null
	iptables -t nat -F TSLINK-WG-SNAT 2>/dev/null
	iptables -t nat -X TSLINK-WG-SNAT 2>/dev/null
	iptables -t filter -D FORWARD -j TSLINK-FORWARD 2>/dev/null
	iptables -t filter -F TSLINK-FORWARD 2>/dev/null
	iptables -t filter -X TSLINK-FORWARD 2>/dev/null
	iptables -t nat -D POSTROUTING -s 10.200.0.0/16 -j MASQUERADE 2>/dev/null
	rm -rf "$T1_DATA" "$T1_WORK"
	exit "$status"
}
