#!/bin/sh
# probe.sh <target-ip> <logfile>: one HTTP request every 200 ms, logging "epoch uptime code backend"
t=$1
log=$2
: >"$log"
while :; do
	up=$(cut -d' ' -f1 /proc/uptime)
	out=$(curl -s -m2 -w '\n%{http_code}' "http://$t/" 2>/dev/null)
	code=$(echo "$out" | tail -1)
	be=$(echo "$out" | grep -m1 '^Hostname' | cut -d' ' -f2)
	echo "$(date +%s) $up $code ${be:--}" >>"$log"
	sleep 0.2
done
