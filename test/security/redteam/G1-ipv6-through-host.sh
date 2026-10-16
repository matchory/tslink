#!/bin/bash
# Guards: G1
# Testbed: t1
# Attacker: A1, a process in a container on a tslink network, as root inside it, with Docker's
#   default capabilities (CAP_NET_RAW).
# Assumes: E1, E2, E3, E5
# Attack: the container sends raw IPv6 frames, through its tslink veth and through its Docker
#   gateway (eth0), to a UDP service on the host's tailnet IPv6 address (fd7a:115c:a1e0::/48).
#   The container needs no IPv6 of its own: the frames go to the gateway's MAC address.
# Control: the jumps into ip6tables' TSLINK-ISOLATE-IN and TSLINK-ISOLATE-FWD (mangle) are
#   kept out. Teardown: the watchdog restores them.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

t1_net rt-bob bob || t1_error "could not create rt-bob"
docker run -d --name rt-raw --network rt-bob \
	-v "$T1_ROOT/test/integration/rawsend.py:/rawsend.py:ro" "$T1_PYTHON" sleep 3600 >/dev/null
t1_wait_ip rt-raw
host_ip6=$(t1_host_ts ip -6)
[ -n "$host_ip6" ] || t1_error "the host's tailscaled has no IPv6 address"
# A UDP service of the host's own, on its tailnet IPv6 address only
python3 -u -c 'import socket, sys
s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
s.bind((sys.argv[1], 9999))
while True:
    print(s.recvfrom(2048)[0].decode(errors="replace"), flush=True)' "$host_ip6" >"$T1_WORK/sink6.log" 2>&1 &
T1_PIDS+=("$!")
got() { grep -qF -- "$1" "$T1_WORK/sink6.log"; }
sleep 1
python3 -c 'import socket, sys
socket.socket(socket.AF_INET6, socket.SOCK_DGRAM).sendto(b"rt-self", (sys.argv[1], 9999))' "$host_ip6"
t1_retry 10 got rt-self || t1_error "the host's UDP service does not answer on $host_ip6"

# raw6_send IFACE PAYLOAD: rt-raw sends PAYLOAD to the host's service as a raw
# IPv6 frame to the gateway of IFACE: "veth" for tslink's, eth0 for Docker's
raw6_send() {
	docker exec rt-raw sh -c '
		if [ "$1" = veth ]; then
			ifc=$(ls /sys/class/net | grep "^veth")
			src=$(ip -4 -o addr show dev "$ifc" | awk "\$4 ~ /^10\\.200\\./ {split(\$4, a, \"/\"); print a[1]}")
			gw=${src%.*}.$((${src##*.} - 1))
		else
			ifc=$1
			gw=$(ip route | awk "/^default/ {print \$3}")
		fi
		ping -c 1 -W 1 "$gw" >/dev/null 2>&1
		mac=$(awk -v ip="$gw" "\$1 == ip {print \$4}" /proc/net/arp)
		python3 /rawsend.py "$ifc" fd00:7e57::2 "$mac" "$2" 9999 "$3"' sh "$1" "$host_ip6" "$2"
}
# raw6_arrives IFACE PREFIX: sends until the service has a payload unique to
# this call, for 30 seconds
raw6_arrives() {
	local p="$2-$SECONDS-$RANDOM"
	local deadline=$((SECONDS + 30))
	until got "$p"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		raw6_send "$1" "$p"
		sleep 2
	done
}
# The host takes no IPv6 on an interface with IPv6 disabled, whatever the
# isolation does: a probe through it could not get through in its control run
ipv6_on() { [ "$(cat "/proc/sys/net/ipv6/conf/$1/disable_ipv6" 2>/dev/null)" = 0 ]; }
veth=$(t1_veth rt-raw)
veth_host=$(ip -4 -o addr show | awk -v gw="${veth%.*}.$((${veth##*.} - 1))" \
	'{split($4, a, "/")} a[1] == gw {print $2}')

isolation_off() { t1_hold_off ip6tables mangle INPUT:TSLINK-ISOLATE-IN FORWARD:TSLINK-ISOLATE-FWD; }
isolation_back() { t1_hold_back ip6tables mangle INPUT:TSLINK-ISOLATE-IN FORWARD:TSLINK-ISOLATE-FWD; }
probe_group "IPv6 to the host's tailnet address" isolation_off isolation_back 60
for ifc in veth eth0; do
	dev=$veth_host
	[ "$ifc" = eth0 ] && dev=docker_gwbridge
	if ipv6_on "$dev"; then
		probe G1 "raw IPv6 frames through $ifc reach the host's tailnet address" \
			raw6_arrives "$ifc" "rt-ipv6-$ifc"
	else
		echo "NOTE: IPv6 is disabled on ${dev:-the host end of the veth}: no probe through $ifc"
	fi
done
if [ "${#PROBE_CMDS[@]}" = 0 ]; then
	echo "SKIPPED: IPv6 is disabled on both ways into the host"
	exit 3
fi
run_probes
probe_exit
