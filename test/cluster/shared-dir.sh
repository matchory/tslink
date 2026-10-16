#!/bin/bash
# shared-dir.sh on|off: a certificate directory shared by every node, for the plugin setting
# shared.source=/mnt/tslink: "on" exports an empty /srv/tslink-shared from $MGR (default mgr) over
# NFS to the swarm addresses of the nodes and mounts it at /mnt/tslink on each, $MGR included;
# "off" unmounts it, removes the export and deletes the directory with its certificates and
# ACME account key. Needs apt (Debian, Ubuntu) on the nodes.
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
M=${MGR:-mgr}
S="$B/s"
addrs=$("$S" "$M" "docker node ls -q | xargs docker node inspect --format '{{.Status.Addr}}'")
self=$("$S" "$M" "docker node inspect self --format '{{.Status.Addr}}'")
case ${1:-} in
on)
	opts=$(for a in $addrs; do printf '%s(rw,sync,no_root_squash,no_subtree_check) ' "$a"; done)
	"$S" "$M" "set -e; DEBIAN_FRONTEND=noninteractive apt-get install -qq -y nfs-kernel-server >/dev/null
	  rm -rf /srv/tslink-shared; mkdir -p /srv/tslink-shared /etc/exports.d; chmod 700 /srv/tslink-shared
	  echo '/srv/tslink-shared $opts' >/etc/exports.d/tslink-test.exports; exportfs -ra"
	for n in $("$B/nodes"); do
		"$S" "$n" "set -e; DEBIAN_FRONTEND=noninteractive apt-get install -qq -y nfs-common >/dev/null
		  mkdir -p /mnt/tslink; mountpoint -q /mnt/tslink || mount -t nfs $self:/srv/tslink-shared /mnt/tslink"
	done
	;;
off)
	for n in $("$B/nodes"); do "$S" "$n" 'if mountpoint -q /mnt/tslink; then umount /mnt/tslink; fi; rmdir /mnt/tslink 2>/dev/null || true'; done
	"$S" "$M" 'rm -f /etc/exports.d/tslink-test.exports; exportfs -ra; rm -rf /srv/tslink-shared'
	;;
*)
	echo "usage: $0 on|off" >&2
	exit 2
	;;
esac
