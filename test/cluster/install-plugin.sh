#!/bin/bash
# install-plugin.sh [ref|WT] -- build tslink from a git ref (default main) or the working tree (WT)
# on $MGR (default mgr), and replace the plugin tslink (tslink:latest, the name the documented
# install --alias tslink gives it) with it on every node. PLUGIN_SETTINGS, if set, is passed to
# docker plugin set before the plugin is enabled (e.g. "shared.source=/mnt/x KEY=value"). With
# ONE_BY_ONE=1, nodes are replaced one at a time, as the upgrade guide does: the next only once every
# endpoint of the last runs again and ONE_BY_ONE_SETTLE seconds (default 15) passed, so the other
# replicas of a Service keep serving. Exits non-zero if the build or any node's install failed
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
REF=${1:-main}
S="$B/s"
M=${MGR:-mgr}
cd "$(git -C "$B" rev-parse --show-toplevel)"

# The version the binary reports, found here: the source on $M has no history. Checked to need no
# quoting in the command for $M
if [ "$REF" = WT ]; then V=$(scripts/version.sh); else V=$(scripts/version.sh "$REF"); fi
case $V in *[!A-Za-z0-9._+-]* | "") echo "unexpected version: $V" >&2 && exit 1 ;; esac

source_tar() {
	if [ "$REF" = WT ]; then
		git ls-files -co --exclude-standard | COPYFILE_DISABLE=1 tar --no-xattrs -cf - -T -
	else
		git archive --format=tar "$REF"
	fi
}
source_tar | "$S" "$M" 'rm -rf /root/src && mkdir -p /root/src && tar -x -C /root/src'

# shellcheck disable=SC2016 # runs on $M
"$S" "$M" 'set -e; cd /root/src
  docker build -q --build-arg TSLINK_VERSION='"$V"' -t tslink:rootfs -f docker/Dockerfile . >/dev/null
  rm -rf /root/plugin && mkdir -p /root/plugin/rootfs && cp docker/config.json /root/plugin/
  id=$(docker create tslink:rootfs); docker export "$id" | tar -x -C /root/plugin/rootfs; docker rm "$id" >/dev/null
  tar -C /root -czf /root/plugin.tgz plugin'
for h in $("$B/nodes" | grep -vx "$M"); do
	"$S" "$M" 'cat /root/plugin.tgz' | "$S" "$h" 'rm -rf /root/plugin && tar -xz -C /root'
done

# docker plugin install cannot read a local directory, so create the plugin under the alias's name
P=tslink:latest
rc=0
# settled <node>: every endpoint on the node runs
settled() { ! "$S" "$1" sh -s <"$B/endpoints.sh" | awk -F'\t' '$4 != "running"' | grep -q .; }
pids=()
for h in $("$B/nodes"); do
	"$S" "$h" "set -e
    if docker plugin inspect $P >/dev/null 2>&1; then docker plugin disable -f $P >/dev/null || true; docker plugin rm -f $P >/dev/null; fi
    docker plugin create $P /root/plugin >/dev/null
    ${PLUGIN_SETTINGS:+docker plugin set $P $PLUGIN_SETTINGS}
    docker plugin enable $P >/dev/null
    echo \"\$(hostname): \$(docker plugin ls --format '{{.Name}} {{.Enabled}}')\"" &
	pids+=("$!")
	if [ -n "${ONE_BY_ONE:-}" ]; then
		wait "$!" || rc=1
		pids=()
		sleep 5 # recovery replaces the status files the old plugin left
		t=0
		until settled "$h" || [ "$t" -ge 180 ]; do
			sleep 5
			t=$((t + 5))
		done
		settled "$h" || echo "$h: endpoints not running 180 s after the install" >&2
		sleep "${ONE_BY_ONE_SETTLE:-15}"
	fi
done
# Each node's install, so a node that failed fails the script
for p in ${pids[@]+"${pids[@]}"}; do wait "$p" || rc=1; done
exit "$rc"
