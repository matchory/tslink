#!/bin/bash
# regress.sh [--upgrade]: cluster regression checks for tslink on the test swarm.
# Exits non-zero if any check fails. Needs the callee, caller and nogrant stacks.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
T=$TSLINK_TEST_TAG
M=${MGR:-mgr}
FAILS=0

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
CALLEES=()
while read -r a; do CALLEES+=("$a"); done < <(devices | jq -r --arg t "$T" '.devices[]|select(any(.tags[]?; .==$t+"-callee") and .connectedToControl and (.hostname|startswith("callee-whoami-")))|.addresses[0]')
VIP=$("$B/tsapi" GET "/tailnet/$TSLINK_TEST_TAILNET/services/$TSLINK_TEST_SVC-callee" | jq -r '.addrs[0]')
SUFFIX=$("$B/s" "$M" 'tailscale status --json | jq -r .MagicDNSSuffix')
START=$(date -u +%Y-%m-%dT%H:%M:%SZ)

# 1. Every task tailscaled answers and is Running
bad=$("$B/health" | grep -v 'bad: none' || true)
check "task tailscaleds healthy" "$([ -z "$bad" ] && echo ok || echo "$bad")"

# 2. Isolation. Each host's tailscaled (node tag) may reach the callees, so traffic leaking out
#    of a nogrant task through its host would succeed instead of being refused.
for n in $("$B/nodes"); do
	c=$("$B/cxs" "$n" caller_client "$B/reach.sh" "${CALLEES[@]}" "$VIP")
	g=$("$B/cxs" "$n" nogrant_client "$B/reach.sh" "${CALLEES[@]}" "$VIP")
	h=$("$B/s" "$n" "for t in ${CALLEES[*]}; do curl -s -m4 -o /dev/null -w '%{http_code} ' http://\$t/; done")
	check "caller@$n reaches callees and VIP" "$(echo "$c" | grep -q refused && echo "$c" || echo ok)"
	check "nogrant@$n refused (no leak)" "$(echo "$g" | grep -q ok && echo "$g" || echo ok)"
	check "leak path live on host $n" "$(echo "$h" | grep -qv 200 && echo "$h" || echo ok)"
done

# 3. DNS from overlay + tslink tasks using quad-100
D=$(devices | jq -r '[.devices[]|select(.hostname|startswith("callee-whoami-"))|.hostname][0]')
for n in $("$B/nodes"); do
	r=$("$B/cxs" "$n" caller_client "$B/dns.sh" "$D" "${TSLINK_TEST_SVC#svc:}-callee.$SUFFIX" example.com echo tasks.echo)
	check "DNS on $n" "$(echo "$r" | grep -q FAIL && echo "$r" || echo ok)"
done

# 4. Rolling update (start-first) under VIP probes: at most 3 failed requests per caller
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

# 5. Drain a worker (the first node other than $M) and reactivate it: no orphaned devices
w=$("$B/s" "$("$B/nodes" | grep -vx "$M" | head -1)" hostname)
"$B/s" "$M" "docker node update --availability drain $w >/dev/null"
sleep 30
"$B/s" "$M" "docker node update --availability active $w >/dev/null"
sleep 30
o=$(orphans "$START")
check "drain and reactivate leave no orphans" "$([ "$o" = 0 ] && echo ok || echo "$o orphans")"

# 6. A plugin upgrade keeps every identity
if [ "${1:-}" = --upgrade ]; then
	before=$(test_devices)
	"$B/install-plugin.sh" WT >/dev/null 2>&1
	sleep 30
	check "plugin upgrade keeps identities" "$([ "$before" = "$(test_devices)" ] && echo ok || echo changed)"
fi

o=$(orphans "$START")
check "no orphaned devices since start" "$([ "$o" = 0 ] && echo ok || echo "$o orphans")"
echo "$FAILS check(s) failed"
exit "$FAILS"
