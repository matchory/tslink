#!/bin/bash
# No guarantee: availability of the readiness endpoint.
# Testbed: t1
# Attacker: A1, a process in a container on a tslink network with tslink.health, as root inside
#   it, with Docker's default capabilities.
# Assumes: E1, E2, E3
# Attack: the container opens 2000 idle connections to its own readiness endpoint, which the
#   plugin serves, shared by all containers. It gets through if the plugin holds 1000 or more of
#   them open (its file descriptors, counted from the host). Without a control, and so outside
#   lib.sh, during the same flood another container's /ready must answer within 1 s.
# Control: the plugin built without the connection limit (LimitListener, maxHealthConns) and with
#   the 5 s ReadHeaderTimeout from before it. Teardown: the plugin built from the tree again.
#   Each swap removes and recreates the containers and their network.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

ready_answer() {
	docker exec "$1" sh -c "printf 'GET /ready HTTP/1.0\r\n\r\n' | nc -w 5 127.0.0.1 9002" 2>/dev/null
}
answers() { ready_answer "$1" | head -n 1 | grep -q '^HTTP/'; }
bed_up() {
	t1_net rt-alice alice || t1_error "could not create rt-alice"
	docker run -d --name rt-flood --network rt-alice --label tslink.health=9002 \
		--ulimit nofile=8192:8192 "$T1_PYTHON" sleep 3600 >/dev/null
	docker run -d --name rt-other --network rt-alice --label tslink.health=9002 "$T1_ALPINE" sleep 3600 >/dev/null
	t1_wait_ip rt-flood
	t1_wait_ip rt-other
	t1_retry 60 answers rt-flood || t1_error "rt-flood's readiness endpoint does not answer"
}
bed_down() {
	docker rm -f rt-flood rt-other >/dev/null
	docker network rm rt-alice >/dev/null
}
bed_up

# flood: 2000 idle connections from rt-flood to its readiness endpoint, held
# for 10 seconds, in the background
flood() {
	docker exec -d rt-flood python3 -c 'import socket, time
socks = []
for _ in range(2000):
    s = socket.socket()
    s.setblocking(False)
    s.connect_ex(("127.0.0.1", 9002))
    socks.append(s)
time.sleep(10)'
	sleep 2
}
plugin_fds() { find "/proc/$(pgrep -x tslink | head -n 1)/fd" -mindepth 1 | wc -l; }
# held: whether the plugin holds 1000 or more of the flood's connections
held() {
	local before after
	before=$(plugin_fds)
	flood
	after=$(plugin_fds)
	echo "plugin file descriptors: $before before the flood, $after during it"
	sleep 10
	[ $((after - before)) -ge 1000 ]
}

# Availability during the flood, against the defended build
flood
start=$(date +%s%N)
answers rt-other
ok=$?
ms=$((($(date +%s%N) - start) / 1000000))
sleep 10
if [ "$ok" = 0 ] && [ "$ms" -le 1000 ]; then
	echo "PASS    rt-other's /ready answered in $ms ms during the flood"
	other=0
else
	echo "FAIL    rt-other's /ready did not answer within 1 s during the flood ($ms ms)"
	other=1
fi

unlimited() {
	local tree=$T1_WORK/unlimited
	rm -rf "$tree" && cp -a "$T1_ROOT" "$tree"
	sed -i -e 's/^\tln = xnetutil.LimitListener(ln, maxHealthConns)$/\t_ = xnetutil.LimitListener/' \
		-e 's/ReadHeaderTimeout: 2 \* time.Second/ReadHeaderTimeout: 5 * time.Second/' \
		"$tree/pkg/core/health.go"
	if grep -q 'ln = xnetutil.LimitListener' "$tree/pkg/core/health.go"; then
		echo "NOTE: could not remove the connection limit from pkg/core/health.go"
	fi
	bed_down
	t1_build "$T1_WORK/plugin-unlimited" "$tree" || echo "NOTE: could not build the control plugin"
	t1_install "$T1_WORK/plugin-unlimited" || echo "NOTE: could not install the control plugin"
	bed_up
}
limited() {
	bed_down
	t1_install "$T1_WORK/plugin" || t1_error "could not reinstall the plugin"
	bed_up
}

probe_group "a readiness endpoint flooded" unlimited limited 0
probe avail "the plugin holds a container's 2000 idle connections to its readiness endpoint" held
run_probes
[ "$other" = 0 ] || exit 1
probe_exit
