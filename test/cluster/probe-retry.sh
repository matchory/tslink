#!/bin/sh
# probe-retry.sh <target-ip> <logfile> <fallback-ip>: like probe.sh, but a failed request is retried
# once through the target, then once against the fallback (another backend's own address). Logs
# "epoch uptime code backend retry-code fallback-code"; the retries are "-" when not needed.
t=$1
log=$2
alt=$3
: >"$log"
get() { curl -s --connect-timeout 1 -m2 -w '\n%{http_code}' "http://$1/" 2>/dev/null; }
while :; do
	up=$(cut -d' ' -f1 /proc/uptime)
	out=$(get "$t")
	code=$(echo "$out" | tail -1)
	be=$(echo "$out" | grep -m1 '^Hostname' | cut -d' ' -f2)
	r=- f=-
	if [ "$code" != 200 ]; then
		r=$(get "$t" | tail -1)
		[ "$r" = 200 ] || f=$(get "$alt" | tail -1)
	fi
	echo "$(date +%s) $up $code ${be:--} $r $f" >>"$log"
	sleep 0.2
done
