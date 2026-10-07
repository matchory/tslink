#!/bin/sh
# perf.sh <ip>: latency, and throughput with 1 and 4 streams, to an iperf3 server
ping -c 30 -i 0.2 -q "$1" | tail -1
for p in 1 4; do
	iperf3 -c "$1" -t 8 -P "$p" -f m 2>&1 | grep sender | tail -1 | sed "s/^/  P$p /"
done
