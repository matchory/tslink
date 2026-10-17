#!/bin/bash
# Guards: G1
# Testbed: t1
# Attacker: A1, a process in a container on a tslink network, as root inside it, with Docker's
#   default capabilities.
# Assumes: E1, E2, E3, E5
# Attack: the container pings the host's tailnet address with ordinary sockets, five times a
#   second, while the plugin restarts and recovers it: while its tailscaled is gone, tailscale0
#   and table 52 with it, the unreachable routes for the tailnet ranges in its namespace are all
#   that keeps such traffic from leaving through Docker's gateway. It gets through if a packet to
#   100.64.0.0/10 leaves the container other than through tailscale0, counted by a rule in its
#   namespace set from the host. The host's tailnet isolation, which would still stop it, is
#   probed by test/integration/run.sh.
# Control: the container's unreachable routes are flushed before the restart. Teardown: recovery
#   restores them; the probe checks that it did.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

t1_net rt-bob bob || t1_error "could not create rt-bob"
docker run -d --name rt-rec --network rt-bob "$T1_ALPINE" sleep 3600 >/dev/null
t1_wait_ip rt-rec
target=$(t1_host_ts ip -4)
pid=$(t1_pid rt-rec)
in_rec() { nsenter -t "$pid" -n "$@"; }
in_rec iptables -t mangle -I POSTROUTING ! -o tailscale0 -d 100.64.0.0/10 -m comment --comment rt-leak ||
	t1_error "could not add the counting rule"
unreachable() { in_rec ip route show type unreachable | grep -q '^unreachable 100\.64\.0\.0/10'; }
unreachable || t1_error "rt-rec has no unreachable route for 100.64.0.0/10"

counted() { in_rec iptables -t mangle -L POSTROUTING -vnx | awk '/rt-leak/ {print $1}'; }
# leaks_while_recovering: restarts the plugin under pings, until rt-rec has
# its tailnet address again
leaks_while_recovering() {
	local n
	in_rec iptables -t mangle -Z POSTROUTING
	docker exec -d rt-rec sh -c "while :; do ping -c 1 -W 1 $target >/dev/null 2>&1; sleep 0.2; done"
	sleep 2
	docker plugin disable -f "$T1_PLUGIN" >/dev/null
	sleep 5
	docker plugin enable "$T1_PLUGIN" >/dev/null
	sleep 5
	t1_retry 180 t1_has_ip rt-rec || echo "NOTE: rt-rec has no tailnet IP 180 s after the restart"
	docker exec rt-rec pkill -f "ping -c 1" 2>/dev/null
	docker exec rt-rec pkill -f "while :" 2>/dev/null
	n=$(counted)
	echo "packets to the tailnet left rt-rec other than through tailscale0: $n"
	[ "$n" -gt 0 ]
}
flush_unreachable() {
	in_rec ip route flush type unreachable
	in_rec ip -6 route flush type unreachable 2>/dev/null
}
routes_back() { t1_retry 30 unreachable || echo "NOTE: recovery did not restore rt-rec's unreachable routes"; }

probe_group "the restart window" flush_unreachable routes_back 30
probe G1 "a container's packets to the tailnet leave other than through tailscale0 while the plugin recovers" \
	leaks_while_recovering
run_probes
probe_exit
