#!/bin/bash
# Guards: G1
# Testbed: t1
# Attacker: A1, a process in another container of a tslink container's stack, on a bridge
#   network the two share but not on tslink, as root inside it, with Docker's default
#   capabilities (CAP_NET_RAW).
# Assumes: E1, E2, E3, E5
# Attack: the neighbour routes 100.64.0.0/10 through the tslink container: it sends raw UDP
#   frames to the host's tailnet address with the tslink container's MAC address on their shared
#   network as the next hop. It gets through if one of its packets leaves the tslink container
#   through tailscale0, into that container's tailscaled, which sends it as its own node. A
#   counting rule in the tslink container's namespace, set from the host, counts them; the source
#   address is the neighbour's, so peers may still drop them: the probe errs towards FAIL.
# Control: net.ipv4.ip_forward=1 in the tslink container's namespace, as the compose file's
#   sysctls would set it; that violates E1, so a passing probe reports INFO. Teardown: the value
#   it had.
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
t1_wait_ip rt-victim
victim_pid=$(t1_pid rt-victim)
victim_mac=$(docker inspect -f '{{(index .NetworkSettings.Networks "rt-app").MacAddress}}' rt-victim)
neighbour_ip=$(docker inspect -f '{{(index .NetworkSettings.Networks "rt-app").IPAddress}}' rt-neighbour)
target=$(t1_host_ts ip -4)
[ -n "$victim_mac" ] && [ -n "$neighbour_ip" ] && [ -n "$target" ] ||
	t1_error "no address: victim MAC '$victim_mac', neighbour '$neighbour_ip', target '$target'"
in_victim() { nsenter -t "$victim_pid" -n "$@"; }
forward=$(in_victim cat /proc/sys/net/ipv4/ip_forward)
echo "rt-victim's net.ipv4.ip_forward is $forward without sysctls"
COUNT=(POSTROUTING -o tailscale0 -s "$neighbour_ip" -m comment --comment rt-neighbour)
in_victim iptables -t mangle -I "${COUNT[@]}" || t1_error "could not add the counting rule"

counted() { in_victim iptables -t mangle -L POSTROUTING -vnx | awk '/rt-neighbour/ {print $1}'; }
# forwarded PREFIX: sends for 20 seconds until the victim forwards a frame
forwarded() {
	local deadline=$((SECONDS + 20))
	in_victim iptables -t mangle -Z POSTROUTING
	until [ "$(counted)" -gt 0 ]; do
		[ "$SECONDS" -lt "$deadline" ] || return 1
		docker exec rt-neighbour python3 /rawsend.py eth0 "$neighbour_ip" "$victim_mac" "$target" 9999 \
			"$1-$SECONDS"
		sleep 2
	done
}

probe_group "a neighbour's gateway" \
	"in_victim sysctl -qw net.ipv4.ip_forward=1" "in_victim sysctl -qw net.ipv4.ip_forward=$forward" 30
probe G1 "a neighbour reaches the tailnet through a tslink container" forwarded rt-neighbour
run_probes
probe_exit "with forwarding on in a tslink container (E1 violated), its neighbours reach the tailnet through its node"
