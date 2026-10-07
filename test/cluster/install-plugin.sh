#!/bin/bash
# install-plugin.sh [ref|WT] -- build tslink from a git ref (default main) or the working tree (WT)
# on $MGR (default mgr), and replace the plugin tslink (tslink:latest, the name the documented
# install --alias tslink gives it) with it on every node. PLUGIN_SETTINGS, if set, is passed to
# docker plugin set before the plugin is enabled (e.g. "shared.source=/mnt/x KEY=value")
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
REF=${1:-main}
S="$B/s"
M=${MGR:-mgr}
cd "$(git -C "$B" rev-parse --show-toplevel)"

source_tar() {
	if [ "$REF" = WT ]; then
		git ls-files -co --exclude-standard | COPYFILE_DISABLE=1 tar --no-xattrs -cf - -T -
	else
		git archive --format=tar "$REF"
	fi
}
source_tar | "$S" "$M" 'rm -rf /root/src && mkdir -p /root/src && tar -x -C /root/src'

# shellcheck disable=SC2016 # runs on $M
"$S" "$M" 'set -e; cd /root/src; docker build -q -t tslink:rootfs -f docker/Dockerfile . >/dev/null
  rm -rf /root/plugin && mkdir -p /root/plugin/rootfs && cp docker/config.json /root/plugin/
  id=$(docker create tslink:rootfs); docker export "$id" | tar -x -C /root/plugin/rootfs; docker rm "$id" >/dev/null
  tar -C /root -czf /root/plugin.tgz plugin'
for h in $("$B/nodes" | grep -vx "$M"); do
	"$S" "$M" 'cat /root/plugin.tgz' | "$S" "$h" 'rm -rf /root/plugin && tar -xz -C /root'
done

# docker plugin install cannot read a local directory, so create the plugin under the alias's name
P=tslink:latest
for h in $("$B/nodes"); do
	"$S" "$h" "set -e; mkdir -p /var/lib/docker-plugins/tailscale
    if docker plugin inspect $P >/dev/null 2>&1; then docker plugin disable -f $P >/dev/null || true; docker plugin rm -f $P >/dev/null; fi
    docker plugin create $P /root/plugin >/dev/null
    ${PLUGIN_SETTINGS:+docker plugin set $P $PLUGIN_SETTINGS}
    docker plugin enable $P >/dev/null
    echo \"\$(hostname): \$(docker plugin ls --format '{{.Name}} {{.Enabled}}')\"" &
done
wait
