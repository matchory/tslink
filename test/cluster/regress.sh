#!/bin/bash
# regress.sh [--upgrade [ref]] [--cases R1,R2,...] [--list]: cluster regression checks for tslink
# on the test swarm, the cases R1 to R8 in this order (--list prints them). --upgrade also runs R6:
# it installs tslink from ref (default WT, the working tree) with install-plugin.sh and checks that
# every identity survives. The cases that reinstall the plugin install ref too. --cases runs only
# the cases named. Exits non-zero if any check fails. Needs the callee, caller and nogrant stacks;
# the cases deploy and remove the others they use.
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
	"R6 a plugin upgrade keeps every identity (--upgrade only)"
	"R7 several named Services per task: both VIPs answer, HTTPS has the Service's certificate, both replicas approved; with a shared directory, one certificate, advertised only once it exists"
	"R8 removing a Service's label drains it from the old tasks before their logout, without failing the other Service"
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

# R6. A plugin upgrade keeps every identity
r6() {
	local before
	if [ -z "$UPGRADE" ]; then
		echo "SKIP  plugin upgrade: needs --upgrade"
		return
	fi
	before=$(test_devices)
	"$B/install-plugin.sh" "$REF" >/dev/null 2>&1
	sleep 30
	check "plugin upgrade keeps identities" "$([ "$before" = "$(test_devices)" ] && echo ok || echo changed)"
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
