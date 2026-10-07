#!/bin/bash
# install-plugin.sh [ref|WT] -- build tslink from a git ref (default main) or the working tree (WT)
# on mgr, and replace ghcr.io/matchory/tslink:latest with it on every node
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
REF=${1:-main}
S="$B/s"
cd "$(git -C "$B" rev-parse --show-toplevel)"

source_tar() {
	if [ "$REF" = WT ]; then
		git ls-files -co --exclude-standard | COPYFILE_DISABLE=1 tar --no-xattrs -cf - -T -
	else
		git archive --format=tar "$REF"
	fi
}
source_tar | "$S" mgr 'rm -rf /root/src && mkdir -p /root/src && tar -x -C /root/src'

# shellcheck disable=SC2016 # runs on mgr
"$S" mgr 'set -e; cd /root/src; docker build -q -t tslink:rootfs -f docker/Dockerfile . >/dev/null
  rm -rf /root/plugin && mkdir -p /root/plugin/rootfs && cp docker/config.json /root/plugin/
  id=$(docker create tslink:rootfs); docker export "$id" | tar -x -C /root/plugin/rootfs; docker rm "$id" >/dev/null
  tar -C /root -czf /root/plugin.tgz plugin'
for h in w1 w2; do
	"$S" mgr 'cat /root/plugin.tgz' | "$S" "$h" 'rm -rf /root/plugin && tar -xz -C /root'
done

P=ghcr.io/matchory/tslink:latest
for h in $("$B/nodes"); do
	"$S" "$h" "set -e; mkdir -p /var/lib/docker-plugins/tailscale
    if docker plugin inspect $P >/dev/null 2>&1; then docker plugin disable -f $P >/dev/null || true; docker plugin rm -f $P >/dev/null; fi
    docker plugin create $P /root/plugin >/dev/null
    docker plugin enable $P >/dev/null
    echo \"\$(hostname): \$(docker plugin ls --format '{{.Name}} {{.Enabled}}')\"" &
done
wait
