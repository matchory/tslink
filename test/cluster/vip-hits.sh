#!/bin/sh
# vip-hits.sh <url> <count>: in a container, <count> requests to the URL, five a second; prints
# "partial=<n> other=<n> failed=<n>", partial counting the answers of R18's partial stack
p=0 o=0 f=0 i=0
while [ "$i" -lt "$2" ]; do
	b=$(curl -s -m3 "$1") || b=
	case $b in
	partial*) p=$((p + 1)) ;;
	"") f=$((f + 1)) ;;
	*) o=$((o + 1)) ;;
	esac
	i=$((i + 1))
	sleep 0.2
done
echo "partial=$p other=$o failed=$f"
