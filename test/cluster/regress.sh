#!/bin/bash
# regress.sh [--upgrade [ref]] [--cases R1,R2,...] [--list]: cluster regression checks for tslink
# on the test swarm, the cases R1 to R17 in this order (--list prints them). --upgrade also runs
# R6, which installs tslink from ref (default WT, the working tree) with install-plugin.sh and
# checks that every identity survives, and R11, the upgrade from v0.3.2. The cases that reinstall
# the plugin install ref too. --cases runs only the cases named. Exits non-zero if any check
# fails. Needs the callee, caller and nogrant stacks and, for R11 and R12, the cluster credential
# on every node; the cases deploy and remove the others they use. R14 restarts Docker on one
# worker, R15 the plugin on another, and R17 flushes that one's filter table, then restores it.
# shellcheck disable=SC2329 # the cases are called by name, as r<n>
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
T=$TSLINK_TEST_TAG
M=${MGR:-mgr}
TN=$TSLINK_TEST_TAILNET
SVC_C=$TSLINK_TEST_SVC-callee
SVC_O=$TSLINK_TEST_SVC-other
L=/var/lib/docker-plugins/tailscale/plugin.log
FAILS=0
UPGRADE=
REF=WT
ONLY=
CASES=(
	"R1 every task tailscaled answers and is Running"
	"R2 callers reach the callees and the VIP, nogrant tasks are refused, also through their host"
	"R3 DNS from overlay and tslink tasks using quad-100"
	"R4 a rolling update fails at most 3 VIP requests per caller"
	"R5 draining and reactivating a worker leaves no orphaned devices"
	"R6 a plugin upgrade keeps every identity and advertised Service, fails at most 3 VIP requests per caller and /ready only goes unanswered (--upgrade only)"
	"R7 several named Services per task: both VIPs answer, HTTPS has the Service's certificate, both replicas approved; with a shared directory, one certificate, advertised only once it exists"
	"R8 removing a Service's label drains it from the old tasks before their logout, without failing the other Service"
	"R9 refused label combinations fail their endpoint, naming the labels, without a device; legacy labels cleared next to a named one serve it"
	"R10 /ready reports each Service, keeps ready while one loses its approval, answers 200 at once after a plugin restart and 503 drained while stopping"
	"R11 an upgrade from v0.3.2 passes R6's checks with legacy HTTPS tasks, named labels work after it, and a downgrade advertises none of them (--upgrade only)"
	"R12 each task device has exactly its network's tag and is ephemeral, no test key outlives its expiry, the cluster credential refuses a network without tags and a tag outside the stack's scope"
	"R13 a non-ephemeral task whose device was deleted comes back after a restart as a new device the caller reaches"
	"R14 a task without dns: resolves through daemon.json's resolver; one with dns: [100.100.100.100, <resolver>] resolves tailnet and site names"
	"R15 at TSLINK_LOG_LEVEL=warn on one node, the restarted plugin logs nothing at info"
	"R16 tslink diag --preflight on every node finds nothing violated but E6, which a plugin built from a directory violates"
	"R17 callers on different nodes ping each other directly, over the node's address and its mapped port, not DERP, also after iptables -F on one node and one watchdog run"
)
while [ $# -gt 0 ]; do
	case $1 in
	--upgrade)
		UPGRADE=1
		if [ $# -gt 1 ] && [ "${2#--}" = "$2" ]; then
			REF=$2
			shift
		fi
		;;
	--cases)
		ONLY=,${2:?--cases needs a list},
		shift
		;;
	--list)
		printf '%s\n' "${CASES[@]}"
		exit 0
		;;
	*)
		echo "usage: $0 [--upgrade [ref]] [--cases R1,R2,...] [--list]" >&2
		exit 2
		;;
	esac
	shift
done

check() {
	if [ "$2" = ok ]; then
		echo "PASS  $1"
	else
		echo "FAIL  $1: $2"
		FAILS=$((FAILS + 1))
	fi
}
devices() { "$B/tsapi" GET "/tailnet/$TSLINK_TEST_TAILNET/devices"; }
test_devices() {
	devices | jq -r --arg t "$T" '[.devices[]|select(any(.tags[]?; startswith($t+"-c") or .==$t+"-nogrant") and .connectedToControl)|"\(.hostname) \(.addresses[0])"]|sort|join(",")'
}
orphans() {
	devices | jq --arg since "$1" --arg t "$T" '[.devices[]|select(any(.tags[]?; startswith($t+"-c") or .==$t+"-nogrant") and (.connectedToControl|not) and .isEphemeral and (.lastSeen > $since))]|length'
}
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
# wait_for <seconds> <command...>: run the command every 5 s until it succeeds, at most that long
wait_for() {
	local end=$((SECONDS + $1))
	shift
	until "$@"; do
		[ "$SECONDS" -ge "$end" ] && return 1
		sleep 5
	done
}
# eps: endpoints.sh on every node, each line led by the node: node, endpoint, stack, hostname,
# state, advertised Services, error, tab-separated
eps() {
	local n
	for n in $("$B/nodes"); do
		"$B/s" "$n" sh -s <"$B/endpoints.sh" | awk -v n="$n" '{ print n "\t" $0 }'
	done
}
# running <stack>: "node endpoint hostname advertised" of the stack's running endpoints
running() { eps | awk -F'\t' -v s="$1" '$3 == s && $5 == "running" { print $1, $2, $4, $6 }'; }
gone() { [ -z "$(eps | awk -F'\t' -v s="$1" '$3 == s')" ]; }
stack_rm() {
	"$B/s" "$M" "docker stack rm $1 >/dev/null 2>&1 || true"
	wait_for 120 gone "$1" || echo "NOTE  stack $1 still has endpoints after 120 s"
}
replicas() { "$B/s" "$M" "docker service inspect $1 --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null"; }
scale() { [ -z "$2" ] || "$B/s" "$M" "docker service scale -d $1=$2 >/dev/null"; }
# probe_fails <node> [log]: failed requests in the probe log of the node's caller
probe_fails() {
	"$B/cx" "$1" caller_client "awk \"\\\$3!=200\" ${2:-/tmp/probe.log} | wc -l" | tr -d ' '
}
# in_caller <cmd>: run a command in the caller task on $M
in_caller() { "$B/cx" "$M" caller_client "$1"; }
# multi_ready: both multi tasks run and advertise both Services
multi_ready() { [ "$(running multi | grep -c " $SVC_C,$SVC_O\$")" = 2 ]; }
up_multi() {
	"$B/deploy" multi callee >/dev/null
	wait_for 300 multi_ready && return
	check "multi tasks advertise both Services" "not within 300 s: $(running multi | tr '\n' ';')"
	return 1
}
approved() { "$B/tsapi" GET "/tailnet/$TN/services/$1/device/$2/approved" | jq -r '.approved'; }
node_ids() {
	devices | jq -r --arg h "$1" '.devices[]|select(.connectedToControl and (.hostname|startswith($h)))|.nodeId'
}
node_id_of() { devices | jq -r --arg h "$1" '.devices[]|select(.connectedToControl and .hostname == $h)|.nodeId'; }
# readylogs start|stop|show [swarm-service]: ready.sh logging to /tmp/ready.log in each running
# container with tslink.health on every node, or each of the service; show prints "node container
# epoch code seconds state services" from each container that has a log, running or exited
readylogs() {
	local n f=label=tslink.health
	[ -n "${2:-}" ] && f=label=com.docker.swarm.service.name=$2
	for n in $("$B/nodes"); do
		case $1 in
		start) "$B/s" "$n" "cat >/tmp/ready.sh; for c in \$(docker ps -q -f $f); do p=\$(docker inspect -f '{{index .Config.Labels \"tslink.health\"}}' \$c); docker exec \$c rm -f /tmp/ready.log; docker cp /tmp/ready.sh \$c:/tmp/ready.sh && docker exec -d \$c sh /tmp/ready.sh \$p /tmp/ready.log; done" <"$B/ready.sh" ;;
		stop) "$B/s" "$n" "for c in \$(docker ps -q -f $f); do docker exec \$c pkill -f /tmp/ready.log; done" </dev/null ;;
		show) "$B/s" "$n" "for c in \$(docker ps -aq -f $f); do docker cp \$c:/tmp/ready.log - 2>/dev/null | tar -xO | sed \"s/^/$n \$c /\"; done" </dev/null ;;
		esac
	done
}
# svc_state <ready.sh line> <service>: the Service's state in the line's services
svc_state() { awk '{ print $5 }' <<<"$1" | jq -r --arg s "$2" '.[$s] // .[$s | ltrimstr("svc:")] // "-"'; }
# advertised: "hostname advertised" of every running endpoint, sorted
advertised() { eps | awk -F'\t' '$5 == "running" { print $4, $6 }' | sort; }
# same <want> <command...>: the command prints want
same() { [ "$1" = "$("${@:2}")" ]; }
# since_log <since> <ERE>: "node line" of the plugin.log lines on every node matching the
# expression, logged at or after since
since_log() {
	local n
	for n in $("$B/nodes"); do
		"$B/s" "$n" "grep -a -E '$2' $L" </dev/null | awk -v n="$n" -v since="time=$1" '$1 >= since { print n, $0 }'
	done
}
# upgrade <name>: install $REF under VIP probes and /ready logs; check that every identity and
# each device's advertised Services survive, that at most 3 VIP requests per caller fail (as in
# R4) and that /ready answers 200 or, while the plugin is down, nothing
upgrade() {
	local devs adv n f v
	devs=$(test_devices)
	adv=$(advertised)
	"$B/probes" stop >/dev/null 2>&1
	"$B/probes" start "$VIP" 2>/dev/null
	readylogs start
	sleep 5
	"$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	wait_for 120 same "$devs" test_devices
	check "$1 keeps identities" "$(same "$devs" test_devices && echo ok || echo changed)"
	wait_for 60 same "$adv" advertised
	v=$(diff <(echo "$adv") <(advertised) | grep '^[<>]' | tr '\n' ';')
	check "$1 keeps each device's advertised Services" "${v:-ok}"
	sleep 10
	"$B/probes" stop >/dev/null 2>&1
	readylogs stop
	for n in $("$B/nodes"); do
		f=$(probe_fails "$n")
		check "$1: caller@$n failed VIP requests ($f)" "$([ "$f" -le 3 ] && echo ok || echo "$f failed")"
	done
	v=$(readylogs show)
	[ -n "$v" ] || echo "NOTE  $1: no task with tslink.health to watch /ready on"
	echo "INFO  $1: /ready unanswered $(awk '$4 == "000"' <<<"$v" | grep -c .) times (5 a second)"
	v=$(awk '{ k = $1 " " $2; last[k] = $0 } $4 != "200" && $4 != "000" { bad = bad $0 ";" }
		END { for (k in last) { split(last[k], f, " "); if (f[4] != "200") bad = bad "last: " last[k] ";" }; printf "%s", bad }' <<<"$v")
	check "$1: /ready answered 200 or nothing, and 200 at the end" "${v:-ok}"
}
node_up() { [ -n "$(node_id_of "$1")" ]; }
# new_node <hostname> <node ID>: a device of that hostname with another node ID is connected
new_node() {
	local id
	id=$(node_id_of "$1")
	[ -n "$id" ] && [ "$id" != "$2" ]
}
# running_on <node>: the number of running endpoints on a node
running_on() { "$B/s" "$1" sh -s <"$B/endpoints.sh" | awk -F'\t' '$4 == "running"' | grep -c .; }
# localapi <node> <endpoint> <path> [curl args]: a LocalAPI request to an endpoint's tailscaled
localapi() {
	"$B/s" "$1" "curl -s -m10 ${*:4} --unix-socket /var/lib/docker-plugins/tailscale/sock/$2.sock 'http://local-tailscaled.sock/localapi/v0/$3'" </dev/null
}

# R1. Every task tailscaled answers and is Running
r1() {
	local bad
	bad=$("$B/health" | grep -v 'bad: none' || true)
	check "task tailscaleds healthy" "$([ -z "$bad" ] && echo ok || echo "$bad")"
}

# R2. Isolation. Each host's tailscaled (node tag) may reach the callees, so traffic leaking out
#     of a nogrant task through its host would succeed instead of being refused.
r2() {
	local n c g h
	for n in $("$B/nodes"); do
		c=$("$B/cxs" "$n" caller_client "$B/reach.sh" "${CALLEES[@]}" "$VIP")
		g=$("$B/cxs" "$n" nogrant_client "$B/reach.sh" "${CALLEES[@]}" "$VIP")
		h=$("$B/s" "$n" "for t in ${CALLEES[*]}; do curl -s -m4 -o /dev/null -w '%{http_code} ' http://\$t/; done")
		check "caller@$n reaches callees and VIP" "$(echo "$c" | grep -q refused && echo "$c" || echo ok)"
		check "nogrant@$n refused (no leak)" "$(echo "$g" | grep -q ok && echo "$g" || echo ok)"
		check "leak path live on host $n" "$(echo "$h" | grep -qv 200 && echo "$h" || echo ok)"
	done
}

# R3. DNS from overlay + tslink tasks using quad-100
r3() {
	local d n r
	d=$(devices | jq -r '[.devices[]|select(.hostname|startswith("callee-whoami-"))|.hostname][0]')
	for n in $("$B/nodes"); do
		r=$("$B/cxs" "$n" caller_client "$B/dns.sh" "$d" "${TSLINK_TEST_SVC#svc:}-callee.$SUFFIX" example.com echo tasks.echo)
		check "DNS on $n" "$(echo "$r" | grep -q FAIL && echo "$r" || echo ok)"
	done
}

# R4. Rolling update (start-first) under VIP probes: at most 3 failed requests per caller
r4() {
	local n f
	"$B/probes" stop >/dev/null 2>&1
	"$B/probes" start "$VIP" 2>/dev/null
	sleep 5
	"$B/s" "$M" 'docker service update -d -q --update-order start-first --force callee_whoami' >/dev/null
	"$B/await" callee_whoami >/dev/null
	sleep 12
	for n in $("$B/nodes"); do
		# shellcheck disable=SC2016 # awk program, expanded in the container
		f=$("$B/cx" "$n" caller_client 'awk "\$3!=200" /tmp/probe.log | wc -l' | tr -d ' ')
		check "rolling update: caller@$n failed requests ($f)" "$([ "$f" -le 3 ] && echo ok || echo "$f failed")"
	done
	"$B/probes" stop >/dev/null 2>&1
}

# R5. Drain a worker (the first node other than $M) and reactivate it: no orphaned devices
r5() {
	local w o
	w=$("$B/s" "$("$B/nodes" | grep -vx "$M" | head -1)" hostname)
	"$B/s" "$M" "docker node update --availability drain $w >/dev/null"
	sleep 30
	"$B/s" "$M" "docker node update --availability active $w >/dev/null"
	sleep 30
	o=$(orphans "$START")
	check "drain and reactivate leave no orphans" "$([ "$o" = 0 ] && echo ok || echo "$o orphans")"
}

# R6. A plugin upgrade keeps every identity, under probes (upgrade)
r6() {
	if [ -z "$UPGRADE" ]; then
		echo "SKIP  plugin upgrade: needs --upgrade"
		return
	fi
	upgrade "R6 plugin upgrade"
}

# R7. Several named Services per task (multi.yml), then again with a shared certificate directory
r7() {
	local r ro v code dom ids id svc
	# The callee stack's backends serve no HTTPS: let only the multi tasks host the Services
	r=$(replicas callee_whoami)
	ro=$(replicas callee_whoami-other)
	scale callee_whoami 0
	scale callee_whoami-other 0
	if up_multi; then
		for v in "$VIP" "$VIP_O"; do
			code=$(in_caller "curl -s -m5 -o /dev/null -w %{http_code} http://$v/")
			check "R7 VIP $v answers over HTTP ($code)" "$([ "$code" = 200 ] && echo ok || echo "$code")"
		done
		# curl checks the certificate against the name: a success means its SAN is the Service's
		dom=${SVC_C#svc:}.$SUFFIX
		code=$(in_caller "curl -sS -m10 -o /dev/null -w %{http_code} https://$dom/ 2>&1")
		check "R7 https://$dom presents a valid certificate for its name" "$([ "$code" = 200 ] && echo ok || echo "$code")"
		ids=$(node_ids multi-web-)
		check "R7 two multi devices" "$([ "$(echo "$ids" | grep -c .)" = 2 ] && echo ok || echo "$ids")"
		for id in $ids; do
			for svc in "$SVC_C" "$SVC_O"; do
				v=$(approved "$svc" "$id")
				check "R7 $id approved for $svc" "$([ "$v" = true ] && echo ok || echo "$v")"
			done
		done
	fi
	scale callee_whoami "$r"
	scale callee_whoami-other "$ro"
	r7_shared
}

# R7, shared variant: shared.source on an NFS directory every node mounts. One replica issues the
# certificate; the other configures its Service only once the certificate exists.
r7_shared() {
	local since dom n crt logs issuer it late configured
	stack_rm multi
	"$B/shared-dir.sh" on >/dev/null
	PLUGIN_SETTINGS='shared.source=/mnt/tslink' "$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	since=$(now)
	if up_multi; then
		dom=${SVC_C#svc:}.$SUFFIX
		crt=$("$B/s" "$M" "find /mnt/tslink -name '$dom.crt' | wc -l" | tr -d ' ')
		check "R7 shared: one $dom.crt in the shared directory ($crt)" "$([ "$crt" = 1 ] && echo ok || echo "$crt")"
		logs=$(for n in $("$B/nodes"); do
			"$B/s" "$n" "grep -a -E 'Certificate for $dom issued|with the certificate for $dom' $L" </dev/null |
				awk -v n="$n" -v since="time=$since" '$1 >= since { print n, $0 }'
		done)
		issuer=$(echo "$logs" | grep 'issued: advertising' | awk '{ print $1 }')
		it=$(echo "$logs" | grep 'issued: advertising' | awk '{ print $2 }')
		check "R7 shared: one replica issued the certificate (${issuer:-none})" "$([ "$(echo "$issuer" | grep -c .)" = 1 ] && echo ok || echo "issuers: $issuer")"
		configured=$(echo "$logs" | grep 'with the certificate for' | awk -v n="$issuer" '$1 != n' | grep -c .)
		late=$(echo "$logs" | grep 'with the certificate for' | awk -v n="$issuer" -v it="$it" '$1 != n && $2 < it')
		check "R7 shared: the other replica advertised only once the certificate existed" "$([ "$configured" -ge 1 ] && [ -z "$late" ] && echo ok || echo "configured=$configured before the issue: $late")"
	fi
	stack_rm multi
	"$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	"$B/shared-dir.sh" off >/dev/null
}

# R8. Removing the label of one Service under probes on both VIPs: the new tasks do not advertise
#     it, each old task's node is logged out only after control fetched its drain (tslink logs
#     tailscaled's "c2n: GET /vip-services received" as "control fetched the drained": the
#     tailscaled.log goes with the ephemeral node's state), and the other Service fails no request
r8() {
	local old n id lines a f v
	up_multi || return
	old=$(running multi | awk '{ print $1, $2 }')
	"$B/probes" stop >/dev/null 2>&1
	"$B/probes" start "$VIP" 2>/dev/null
	PROBE_LOG=/tmp/probe-other.log "$B/probes" start "$VIP_O" 2>/dev/null
	sleep 5
	"$B/s" "$M" "docker service update -d -q --container-label-rm tslink.service.${SVC_O#svc:}.serve.80 multi_web" >/dev/null
	"$B/await" multi_web >/dev/null
	sleep 20
	"$B/probes" stop >/dev/null 2>&1
	a=$(running multi)
	v=ok
	if [ "$(echo "$a" | grep -c .)" != 2 ] || echo "$a" | grep -q "$SVC_O"; then v=$(echo "$a" | tr '\n' ';'); fi
	check "R8 the new tasks do not advertise $SVC_O" "$v"
	while read -r n id; do
		lines=$("$B/s" "$n" "grep -a -E 'Endpoint $id: (control fetched the drained|logging out ephemeral node)' $L" </dev/null | sed 's/.*msg="Endpoint [0-9a-f]*: //')
		v=ok
		if ! echo "$lines" | head -1 | grep -q '^control fetched' || ! echo "$lines" | tail -1 | grep -q '^logging out'; then
			v="log: $(echo "$lines" | tr '\n' ';')"
		fi
		check "R8 old task $id@$n: control fetched the drain before the logout" "$v"
	done <<<"$old"
	for n in $("$B/nodes"); do
		f=$(probe_fails "$n")
		check "R8 $SVC_C: caller@$n failed requests ($f)" "$([ "$f" = 0 ] && echo ok || echo "$f failed")"
		echo "INFO  R8 $SVC_O: caller@$n failed requests ($(probe_fails "$n" /tmp/probe-other.log))"
	done
	stack_rm multi
}

# R9. Refused label combinations (mixed.yml): legacy and named labels together, or a named label
#     with tslink.direct=true, fail the endpoint with an error naming them, and no device comes
#     up; legacy labels cleared to "" next to a named label serve the named Service
mixed_settled() {
	local e
	e=$(eps | awk -F'\t' '$3 == "mixed" { print $4, $5 }')
	grep -q '^mixed-combo-.* failed' <<<"$e" && grep -q '^mixed-direct-.* failed' <<<"$e" &&
		grep -q '^mixed-cleared-.* running' <<<"$e"
}
r9() {
	local since e svc want v d
	since=$(now)
	"$B/deploy" mixed callee >/dev/null
	wait_for 300 mixed_settled || echo "NOTE  mixed not settled after 300 s"
	e=$(eps | awk -F'\t' '$3 == "mixed"')
	for svc in combo direct; do
		want='tslink.service.<name>.serve.<port> cannot be combined with tslink.service or tslink.serve.<port>'
		[ "$svc" = direct ] && want='tslink.direct=true cannot be combined with tslink.service.<name>.serve.<port>'
		v=$(awk -F'\t' -v h="mixed-$svc-" 'index($4, h) == 1 { print $5 ": " $7 }' <<<"$e")
		check "R9 $svc refused ($v)" "$(grep -qF "failed: invalid Service labels: $want" <<<"$v" && echo ok || echo "want failed: $want")"
		d=$(devices | jq -r --arg h "mixed-$svc-" --arg since "$since" '[.devices[]|select((.hostname|startswith($h)) and .lastSeen > $since)|.hostname]|join(",")')
		check "R9 $svc has no device" "$([ -z "$d" ] && echo ok || echo "$d")"
	done
	v=$(awk -F'\t' 'index($4, "mixed-cleared-") == 1 { print $5, $6 }' <<<"$e")
	check "R9 cleared serves $SVC_O ($v)" "$([ "$v" = "running $SVC_O" ] && echo ok || echo "want running $SVC_O")"
	stack_rm mixed
}

# R10. /ready per Service and the latch (multi.yml): 200 with both Services ready; with one
#      Service's approval revoked, services shows it while state stays ready; after a plugin
#      restart, each task's first answer is 200 within 1 s; a stopping task answers 503 drained
r10() {
	local n host id line v c code t
	up_multi || return
	read -r n _ host _ <<<"$(running multi | head -1)"
	line=$("$B/cxs" "$n" multi_web "$B/ready.sh" 9002)
	v=ok
	if [ "$(awk '{ print $2, $4 }' <<<"$line")" != "200 ready" ] || [ "$(svc_state "$line" "$SVC_C")" != ready ] ||
		[ "$(svc_state "$line" "$SVC_O")" != ready ]; then v=$line; fi
	check "R10 /ready on $host: 200 with both Services ready" "$v"

	id=$(node_id_of "$host")
	"$B/tsapi" POST "/tailnet/$TN/services/$SVC_O/device/$id/approved" '{"approved":false}' >/dev/null
	sleep 5
	# a latched endpoint answers from its cache, refreshed in the background: the second answer is current
	"$B/cxs" "$n" multi_web "$B/ready.sh" 9002 >/dev/null
	sleep 1
	line=$("$B/cxs" "$n" multi_web "$B/ready.sh" 9002)
	v=ok
	if [ "$(awk '{ print $2, $4 }' <<<"$line")" != "200 ready" ] || [ "$(svc_state "$line" "$SVC_O")" = ready ] ||
		[ "$(svc_state "$line" "$SVC_C")" != ready ]; then v=$line; fi
	check "R10 $SVC_O unapproved on $host: services shows it, state stays ready" "$v"
	"$B/tsapi" POST "/tailnet/$TN/services/$SVC_O/device/$id/approved" '{"approved":true}' >/dev/null

	readylogs start multi_web
	sleep 2
	"$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	sleep 10
	readylogs stop multi_web
	# per container, the first answer after the plugin was gone (no answer, code 000)
	line=$(readylogs show multi_web)
	[ -n "$line" ] || check "R10 /ready logged across the plugin restart" "no log"
	v=$(awk '{ k = $1 " " $2; seen[k] = 1 } $4 == "000" { gap[k] = 1; next }
		gap[k] && !done[k] { done[k] = 1; print k, $4, $5 }
		END { for (k in seen) if (gap[k] && !done[k]) print k, "000", "-" }' <<<"$line")
	[ -z "$line" ] || [ -n "$v" ] || echo "NOTE  R10 /ready answered throughout the plugin restart"
	while read -r n c code t; do
		check "R10 first /ready on $n after the plugin restart: $code in ${t}s" "$([ "$code" = 200 ] && awk -v t="$t" 'BEGIN { exit !(t < 1) }' && echo ok || echo "want 200 within 1 s")"
	done < <([ -z "$v" ] || echo "$v")

	readylogs start multi_web
	scale multi_web 0
	sleep 30
	v=$(readylogs show multi_web | awk '$4 == "503" && $6 == "drained" { print $1 }' | sort -u | grep -c .)
	check "R10 both stopping tasks answered 503 drained ($v)" "$([ "$v" = 2 ] && echo ok || echo "$v")"
	stack_rm multi
}

# R11. Upgrade from v0.3.2, with the legacy callee stacks, tslink-test-callee's HTTPS included,
#      under R6's checks; then named labels on $REF; then the downgrade, which is unsupported:
#      v0.3.2 must warn of the named labels and advertise none of their Services
legacy_up() { [ "$(running tslink-test-callee | grep -c " $SVC_C\$")" -ge 3 ]; }
r11() {
	local since v
	if [ -z "$UPGRADE" ]; then
		echo "SKIP  R11: needs --upgrade"
		return
	fi
	"$B/install-plugin.sh" v0.3.2 >/dev/null 2>&1
	"$B/deploy" tslink-test-callee - >/dev/null
	wait_for 300 legacy_up || echo "NOTE  R11 tslink-test-callee not up on v0.3.2 after 300 s"
	upgrade "R11 upgrade from v0.3.2"
	stack_rm tslink-test-callee
	if up_multi; then
		check "R11 named labels after the upgrade: both Services advertised" ok
		since=$(now)
		"$B/install-plugin.sh" v0.3.2 >/dev/null 2>&1
		sleep 60
		v=$(eps | awk -F'\t' '$3 == "multi" { print $1, $4, $5, $6 }')
		echo "INFO  R11 downgrade, multi endpoints on v0.3.2: $(tr '\n' ';' <<<"$v")"
		check "R11 downgrade: no named-label Service advertised" "$(awk '$4 != "-"' <<<"$v" | grep -q . && tr '\n' ';' <<<"$v" || echo ok)"
		v=$(since_log "$since" 'ignoring unknown label tslink\.service\.' | grep -c .)
		check "R11 downgrade: v0.3.2 warned of the named labels ($v lines)" "$([ "$v" -ge 1 ] && echo ok || echo none)"
		stack_rm multi
		"$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	fi
}

# R12. OAuth and tags: each task device has exactly its network's tag and is ephemeral; no key
#      the tests described outlives its expiry; on the cluster credential, a network without
#      tslink.tags fails and a tag outside the stack's scope never starts tailscaled
scope_settled() { eps | awk -F'\t' '$3 == "tslink-test-scope" { print $5 }' | grep -q .; }
r12() {
	local v n out e id since
	since=$(now)
	n=$(devices | jq --arg t "$T" '[.devices[]|select(.connectedToControl and (.hostname|test("^(callee|caller|nogrant)-")))]|length')
	v=$(devices | jq -r --arg t "$T" '.devices[]|select(.connectedToControl)
		| (.hostname|capture("^(?<s>callee|caller|nogrant)-").s) as $s
		| select(.tags != [$t + "-" + $s] or (.isEphemeral|not))
		| "\(.hostname) tags=\(.tags // []|join(",")) ephemeral=\(.isEphemeral)"' | tr '\n' ';')
	check "R12 $n task devices: exactly their network's tag, ephemeral" "$([ -z "$v" ] && [ "$n" -gt 0 ] && echo ok || echo "${v:-no devices}")"
	v=$(for id in $("$B/tsapi" GET "/tailnet/$TN/keys" | jq -r --arg d "${T#tag:}" '.keys[]|select((.description // "")|startswith($d))|.id'); do
		"$B/tsapi" GET "/tailnet/$TN/keys/$id"
	done | jq -r --arg now "$(now)" 'select(.expires != null and .expires < $now)|"\(.id) expired \(.expires)"' | tr '\n' ';')
	check "R12 no test key outlives its expiry" "${v:-ok}"

	out=$("$B/deploy" tslink-test-notags - 2>&1)
	sleep 30
	e=$("$B/s" "$M" "docker service ps --no-trunc --format '{{.Error}}' tslink-test-notags_client 2>/dev/null")
	v=ok
	grep -q 'tslink.tags is required' <<<"$out $e" || v=$(echo "deploy: $out; tasks: $e" | tr '\n' ' ')
	check "R12 tslink-test-notags: the network is refused" "$v"
	stack_rm tslink-test-notags

	"$B/deploy" tslink-test-scope - >/dev/null
	wait_for 120 scope_settled
	read -r n id e <<<"$(eps | awk -F'\t' '$3 == "tslink-test-scope" { print $1, $2, $5 ": " $7 }' | head -1)"
	check "R12 tslink-test-scope: refused ($e)" "$(grep -q '^failed: .*outside stack' <<<"$e" && echo ok || echo "want failed: ... outside stack ...")"
	v=$("$B/s" "${n:-$M}" "test -e /var/lib/docker-plugins/tailscale/sock/$id.sock && echo socket")
	v+=$(devices | jq -r --arg since "$since" '.devices[]|select((.hostname|startswith("tslink-test-scope-")) and .lastSeen > $since)|" device \(.hostname)"')
	check "R12 tslink-test-scope: no tailscaled" "${v:-ok}"
	stack_rm tslink-test-scope
}

# R13. Stale state as a new device: a non-ephemeral task (stale.yml) whose device was deleted
#      logs in again as a new device after a restart, and the caller reaches it
r13() {
	local old new ip v
	"$B/deploy" stale callee >/dev/null
	if ! wait_for 300 node_up stale-web; then
		check "R13 stale-web comes up" "not within 300 s"
		stack_rm stale
		return
	fi
	old=$(node_id_of stale-web)
	"$B/tsapi" DELETE "/device/$old" >/dev/null
	"$B/s" "$M" "docker service update -d -q --force stale_web" >/dev/null
	"$B/await" stale_web >/dev/null
	wait_for 300 new_node stale-web "$old"
	new=$(node_id_of stale-web)
	check "R13 a new device after the deleted $old (${new:-none})" "$([ -n "$new" ] && [ "$new" != "$old" ] && echo ok || echo "no new device")"
	ip=$(devices | jq -r --arg id "$new" '.devices[]|select(.nodeId == $id)|.addresses[0]')
	v=$("$B/cxs" "$M" caller_client "$B/reach.sh" "$ip")
	check "R13 the caller reaches the new device $ip ($v)" "$([ "$v" = "ok " ] && echo ok || echo refused)"
	stack_rm stale
	# not ephemeral: the device stays until deleted
	[ -n "$new" ] && "$B/tsapi" DELETE "/device/$new" >/dev/null
}

# R14. DNS (T8): on one worker, daemon.json's dns names a resolver (dnsmasq on docker0's address)
#      that alone knows a site name; a task without dns: resolves it through Docker, and a task
#      with dns: [100.100.100.100, <resolver>] resolves it and a tailnet name
r14() {
	local n h r k site=site.tslink-test.internal
	n=$("$B/nodes" | grep -vx "$M" | tail -1)
	h=$("$B/s" "$n" hostname)
	k=$(running_on "$n")
	r=$("$B/s" "$n" "ip -4 -o addr show docker0" | awk '{ sub("/.*", "", $4); print $4 }')
	"$B/s" "$n" "R=$r SITE=$site sh -s" <<'EOF_NODE'
set -e
docker rm -f tslink-test-dns >/dev/null 2>&1 || true
docker run -d -q --name tslink-test-dns --restart unless-stopped --network host alpine:3.23 sh -c \
	"apk add -q dnsmasq && exec dnsmasq -k --listen-address=$R --bind-dynamic --no-hosts --no-resolv --server=1.1.1.1 --address=/$SITE/192.0.2.53" >/dev/null
f=/etc/docker/daemon.json b=/root/daemon.json.tslink-test
rm -f "$b"
[ ! -f "$f" ] || cp "$f" "$b"
{ cat "$b" 2>/dev/null || echo '{}'; } | jq --arg r "$R" '.dns = [$r]' >"$f.new"
mv "$f.new" "$f"
systemctl restart docker
EOF_NODE
	wait_for 300 same "$k" running_on "$n" || echo "NOTE  R14 $n: $(running_on "$n") of $k endpoints running after the Docker restart"
	TSLINK_TEST_PIN=$h TSLINK_TEST_DNS=$r "$B/deploy" dnsmix caller >/dev/null
	wait_for 300 same "$((k + 2))" running_on "$n"
	r=$("$B/cxs" "$n" dnsmix_plain "$B/dns.sh" "$site")
	check "R14 without dns:, $site through daemon.json's resolver ($r)" "$([ "$r" = "ok " ] && echo ok || echo "$r")"
	r=$("$B/cxs" "$n" dnsmix_mixed "$B/dns.sh" "$site" "${SVC_C#svc:}.$SUFFIX")
	check "R14 with dns: [100.100.100.100, resolver], the site and tailnet names ($r)" "$([ "$r" = "ok ok " ] && echo ok || echo "$r")"
	stack_rm dnsmix
	"$B/s" "$n" sh -s <<'EOF_NODE'
f=/etc/docker/daemon.json b=/root/daemon.json.tslink-test
if [ -f "$b" ]; then mv "$b" "$f"; else rm -f "$f"; fi
docker rm -f tslink-test-dns >/dev/null
systemctl restart docker
EOF_NODE
	wait_for 300 same "$k" running_on "$n" || echo "NOTE  R14 $n: $(running_on "$n") of $k endpoints running after the Docker restart"
}

# R15. TSLINK_LOG_LEVEL=warn on one worker: the plugin, restarted with it, logs no info line,
#      past one watchdog run; then back to info
r15() {
	local n k v
	n=$("$B/nodes" | grep -vx "$M" | head -1)
	"$B/s" "$n" "docker plugin disable -f tslink:latest >/dev/null && docker plugin set tslink:latest TSLINK_LOG_LEVEL=warn"
	k=$("$B/s" "$n" "wc -l <$L")
	"$B/s" "$n" "docker plugin enable tslink:latest >/dev/null"
	sleep 90
	v=$("$B/s" "$n" "tail -n +$((k + 1)) $L | grep -ci 'level=info'")
	check "R15 no info line at warn on $n ($v)" "$([ "$v" = 0 ] && echo ok || echo "$v info lines")"
	"$B/s" "$n" "docker plugin disable -f tslink:latest >/dev/null && docker plugin set tslink:latest TSLINK_LOG_LEVEL=info && docker plugin enable tslink:latest >/dev/null"
}

# R16. The preflight as run.sh runs it, from the installed plugin's rootfs, on every node:
#      nothing violated or failing but E6, which a plugin created from a directory violates
r16() {
	local n out v
	for n in $("$B/nodes"); do
		out=$("$B/s" "$n" "tar -C /root/plugin/rootfs -c . | docker import -q - tslink:preflight >/dev/null &&
			docker run --rm --network host --cap-add NET_ADMIN -v /var/run/docker.sock:/var/run/docker.sock \
			-v /var/lib/docker-plugins/tailscale:/data --entrypoint /tslink tslink:preflight diag --preflight 2>&1
			echo exit=\$?; docker image rm -f tslink:preflight >/dev/null")
		echo "INFO  R16 $n: $(grep -E '^E6 |^exit=' <<<"$out" | tr '\n' ';')"
		v=$(awk '$2 == "violated" || $2 == "error"' <<<"$out" | grep -v '^E6 ' | tr '\n' ';')
		grep -q '^E1 ' <<<"$out" || v="no preflight output: $(tr '\n' ' ' <<<"$out")"
		check "R16 preflight on $n" "${v:-ok}"
	done
}

# R17. Direct paths across hosts: a LocalAPI disco ping from each node's caller to the next node's
#      goes to that node's address with a mapped WireGuard port (6xxxx), not through DERP; again
#      after iptables -F on one worker, past one watchdog run (which restores tslink's rules)
direct_ring() {
	local ns ids=() ips=() addrs=() e i j out ep try
	read -ra ns <<<"$("$B/nodes" | tr '\n' ' ')"
	e=$(running caller)
	for i in "${!ns[@]}"; do
		ids[i]=$(awk -v n="${ns[i]}" '$1 == n { print $2; exit }' <<<"$e")
		ips[i]=$(localapi "${ns[i]}" "${ids[i]}" 'status?peers=false' | jq -r '.Self.TailscaleIPs[0]')
		addrs[i]=$("$B/s" "${ns[i]}" "docker info --format '{{.Swarm.NodeAddr}}'")
	done
	for i in "${!ns[@]}"; do
		j=$(((i + 1) % ${#ns[@]}))
		for try in 1 2 3 4 5 6 7 8 9 10; do
			out=$(localapi "${ns[i]}" "${ids[i]}" "ping?ip=${ips[j]}&type=disco" -X POST)
			ep=$(jq -r '.Endpoint // empty' <<<"$out" 2>/dev/null)
			[[ $ep == "${addrs[j]}":6[0-9][0-9][0-9][0-9] ]] && break
			sleep 3
		done
		check "$1: ${ns[i]} to ${ns[j]} direct after $try pings (${ep:-$(tr '\n' ' ' <<<"$out")})" "$([[ $ep == "${addrs[j]}":6[0-9][0-9][0-9][0-9] ]] && echo ok || echo "want ${addrs[j]}:6xxxx")"
	done
}
r17() {
	local w
	direct_ring R17
	w=$("$B/nodes" | grep -vx "$M" | head -1)
	"$B/s" "$w" "iptables-save -t filter >/root/filter.tslink-test && iptables -F"
	sleep 75
	direct_ring "R17 after iptables -F on $w"
	"$B/s" "$w" "iptables-restore </root/filter.tslink-test && rm /root/filter.tslink-test"
}

CALLEES=()
while read -r a; do CALLEES+=("$a"); done < <(devices | jq -r --arg t "$T" '.devices[]|select(any(.tags[]?; .==$t+"-callee") and .connectedToControl and (.hostname|startswith("callee-whoami-")))|.addresses[0]')
VIP=$("$B/tsapi" GET "/tailnet/$TSLINK_TEST_TAILNET/services/$TSLINK_TEST_SVC-callee" | jq -r '.addrs[0]')
VIP_O=$("$B/tsapi" GET "/tailnet/$TN/services/$SVC_O" | jq -r '.addrs[0]')
SUFFIX=$("$B/s" "$M" 'tailscale status --json | jq -r .MagicDNSSuffix')
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)

for c in "${CASES[@]}"; do
	id=${c%% *}
	case $ONLY in "" | *",$id,"*) ;; *) continue ;; esac
	echo "== $c"
	"r${id#R}"
done

o=$(orphans "$START")
check "no orphaned devices since start" "$([ "$o" = 0 ] && echo ok || echo "$o orphans")"
echo "$FAILS check(s) failed"
exit "$FAILS"
