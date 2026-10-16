#!/bin/bash
# Guards: G2
# Testbed: t1
# Attacker: a host on the LAN that routes tslink's veth range, 10.200.0.0/16, through the Docker
#   host. A network namespace on the testbed stands in for it, behind a veth pair whose host end
#   (rtlan-h) has none of the names of Docker's or tslink's interfaces: to the host, its traffic
#   arrives as a LAN host's does.
# Assumes: E1, E2, E3, E5 (mangle table, plugin privileges)
# Attack: the LAN host sends TCP (8080) and UDP (9999, not 41641) to a tslink container's veth
#   address. It gets through if a packet arrives on the container's veth, counted by rules in the
#   container's namespace set from the host; a TCP handshake cannot complete, since the container
#   answers through its Docker gateway.
# Control: the jump into TSLINK-VETH-FWD (mangle FORWARD) is kept out. Teardown: the watchdog
#   restores it.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

t1_net rt-alice alice || t1_error "could not create rt-alice"
docker run -d --name rt-target --network rt-alice "$T1_ALPINE" sleep 3600 >/dev/null
t1_wait_ip rt-target
veth=$(t1_veth rt-target)
[ -n "$veth" ] || t1_error "rt-target has no veth address"
pid=$(t1_pid rt-target)
in_target() { nsenter -t "$pid" -n "$@"; }

LAN=198.51.100.2
lan_down() {
	ip link del rtlan-h 2>/dev/null
	ip netns del rt-lan 2>/dev/null
}
t1_on_exit lan_down
ip netns add rt-lan || t1_error "could not create the LAN namespace"
ip link add rtlan-h type veth peer name rtlan-p netns rt-lan
ip addr add 198.51.100.1/30 dev rtlan-h
ip link set rtlan-h up
ip netns exec rt-lan ip addr add "$LAN/30" dev rtlan-p
ip netns exec rt-lan ip link set rtlan-p up
ip netns exec rt-lan ip link set lo up
ip netns exec rt-lan ip route add 10.200.0.0/16 via 198.51.100.1
ip netns exec rt-lan ping -c 1 -W 2 198.51.100.1 >/dev/null || t1_error "the LAN namespace does not reach the host"

for proto in tcp:8080 udp:9999; do
	in_target iptables -t mangle -I PREROUTING -s "$LAN" -d "$veth" -p "${proto%%:*}" \
		--dport "${proto#*:}" -m comment --comment "rt-lan-${proto%%:*}" ||
		t1_error "could not add the counting rule for $proto"
done
counted() { in_target iptables -t mangle -L PREROUTING -vnx | awk -v c="rt-lan-$1" '$0 ~ c {print $1}'; }
# arrives PROTO PORT: the LAN host sends for 20 seconds until a packet of
# PROTO arrives on the container's veth
arrives() {
	local deadline=$((SECONDS + 20))
	in_target iptables -t mangle -Z PREROUTING
	until [ "$(counted "$1")" -gt 0 ]; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		ip netns exec rt-lan timeout 3 bash -c "echo rt-lan >/dev/$1/$veth/$2" 2>/dev/null
		sleep 2
	done
}

probe_group "a LAN host routing the veth range" \
	"t1_hold_off iptables mangle FORWARD:TSLINK-VETH-FWD" "t1_hold_back iptables mangle FORWARD:TSLINK-VETH-FWD" 60
probe G2 "a LAN host reaches a container's veth address over TCP" arrives tcp 8080
probe G2 "a LAN host reaches a container's veth address over UDP" arrives udp 9999
run_probes
probe_exit
