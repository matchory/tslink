#!/bin/bash
# Guards: G1
# Testbed: t1
# Attacker: A1, a process in another container of a tslink container's stack, on a bridge
#   network the two share but not on tslink, as root inside it, with Docker's default
#   capabilities (CAP_NET_RAW).
# Assumes: E1, E2, E3, E5
# Attack: the neighbour routes 100.64.0.0/10 through the tslink container: it sends raw UDP
#   frames to a tailnet peer (a second container of the same user, which the policy lets the
#   tslink container reach) with the tslink container's MAC address on their shared network as
#   the next hop. It gets through if one of its payloads arrives at the peer's UDP service on
#   its tailnet address. A new namespace may inherit net.ipv4.ip_forward=1 from the host, so the
#   tslink container may forward the frames into its tailscaled; they keep the neighbour's
#   source address, which is none of the node's, so the peer's WireGuard drops them. The probe
#   prints the tslink container's ip_forward as a NOTE.
# Control: in the tslink container's namespace, net.ipv4.ip_forward=1 and MASQUERADE of what
#   leaves through tailscale0, as a stack with NET_ADMIN could set them; the frames then carry
#   the node's own address. That violates E1, so a passing probe reports INFO. Teardown: the
#   rule removed, ip_forward back to the value it had.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

t1_net rt-alice alice || t1_error "could not create rt-alice"
docker network create rt-app >/dev/null || t1_error "could not create rt-app"
docker run -d --name rt-victim --network rt-alice "$T1_ALPINE" sleep 3600 >/dev/null
docker network connect rt-app rt-victim || t1_error "could not connect rt-victim to rt-app"
docker run -d --name rt-neighbour --network rt-app \
	-v "$T1_ROOT/test/integration/rawsend.py:/rawsend.py:ro" "$T1_PYTHON" sleep 3600 >/dev/null
# The peer: a UDP service that logs each payload it receives
docker run -d --name rt-peer --network rt-alice "$T1_PYTHON" python3 -u -c 'import socket
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind(("0.0.0.0", 9999))
while True:
    print(s.recvfrom(2048)[0].decode(errors="replace"), flush=True)' >/dev/null
t1_wait_ip rt-victim
t1_wait_ip rt-peer
victim_pid=$(t1_pid rt-victim)
victim_mac=$(docker inspect -f '{{(index .NetworkSettings.Networks "rt-app").MacAddress}}' rt-victim)
neighbour_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "rt-app").IPAddress}}' rt-neighbour)
target=$(t1_ip rt-peer)
[ -n "$victim_mac" ] && [ -n "$neighbour_ip" ] && [ -n "$target" ] ||
	t1_error "no address: victim MAC '$victim_mac', neighbour '$neighbour_ip', target '$target'"
got() { docker logs rt-peer 2>/dev/null | grep -qF -- "$1"; }
# The first datagrams may go before the WireGuard handshake: each try sends one
self_reaches() {
	docker exec rt-victim sh -c "echo rt-self | nc -u -w 1 $target 9999" >/dev/null 2>&1
	got rt-self
}
t1_retry 30 self_reaches || t1_error "rt-peer's UDP service does not answer rt-victim on $target"
in_victim() { nsenter -t "$victim_pid" -n "$@"; }
forward=$(in_victim cat /proc/sys/net/ipv4/ip_forward)
echo "NOTE: rt-victim's net.ipv4.ip_forward is $forward without sysctls"
NAT=(POSTROUTING -o tailscale0 -m comment --comment rt-neighbour -j MASQUERADE)
nat_on() { in_victim sysctl -qw net.ipv4.ip_forward=1 && in_victim iptables -t nat -I "${NAT[@]}"; }
nat_off() {
	in_victim iptables -t nat -D "${NAT[@]}"
	in_victim sysctl -qw "net.ipv4.ip_forward=$forward"
}

# forwarded PREFIX: sends for 20 seconds until the peer has a payload unique
# to this call. Each call sends from a source port of its own: the attack's
# frames, forwarded without NAT, leave a conntrack entry that would keep the
# control's MASQUERADE from applying to the same flow
forwarded() {
	local p="$1-$SECONDS-$RANDOM"
	local sport=$((20000 + RANDOM % 20000))
	local deadline=$((SECONDS + 20))
	until got "$p"; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		docker exec rt-neighbour python3 /rawsend.py eth0 "$neighbour_ip" "$victim_mac" "$target" 9999 "$p" "$sport"
		sleep 2
	done
}

probe_group "a neighbour's gateway" nat_on nat_off 30
probe G1 "a neighbour reaches a tailnet peer through a tslink container" forwarded rt-neighbour
run_probes
probe_exit "with NAT in a tslink container (NET_ADMIN, E1 violated), its neighbours reach the tailnet through its node"
