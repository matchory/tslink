#!/bin/bash
# regress.sh [--upgrade [ref]] [--cases R1,R2,...] [--list]: cluster regression checks for tslink
# on the test swarm, the cases R1 to R6 in this order (--list prints them). --upgrade also runs R6:
# it installs tslink from ref (default WT, the working tree) with install-plugin.sh and checks that
# every identity survives. --cases runs only the cases named. Exits non-zero if any check fails.
# Needs the callee, caller and nogrant stacks.
# shellcheck disable=SC2329 # the cases are called by name, as r<n>
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
T=$TSLINK_TEST_TAG
M=${MGR:-mgr}
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

CALLEES=()
while read -r a; do CALLEES+=("$a"); done < <(devices | jq -r --arg t "$T" '.devices[]|select(any(.tags[]?; .==$t+"-callee") and .connectedToControl and (.hostname|startswith("callee-whoami-")))|.addresses[0]')
VIP=$("$B/tsapi" GET "/tailnet/$TSLINK_TEST_TAILNET/services/$TSLINK_TEST_SVC-callee" | jq -r '.addrs[0]')
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
