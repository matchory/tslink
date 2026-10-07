#!/bin/bash
# install-drain.sh [--remove]: install deploy/systemd's tslink-drain and its docker.service drop-in
# on every node, or remove them
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
D="$B/../../deploy/systemd"
for n in $("$B/nodes"); do
	if [ "${1:-}" = --remove ]; then
		"$B/s" "$n" 'rm -f /usr/local/sbin/tslink-drain /etc/systemd/system/docker.service.d/tslink-drain.conf && systemctl daemon-reload'
	else
		"$B/s" "$n" 'cat >/usr/local/sbin/tslink-drain && chmod 755 /usr/local/sbin/tslink-drain' <"$D/tslink-drain"
		"$B/s" "$n" 'mkdir -p /etc/systemd/system/docker.service.d && cat >/etc/systemd/system/docker.service.d/tslink-drain.conf && systemctl daemon-reload' <"$D/tslink-drain.conf"
	fi
	echo "$n: $("$B/s" "$n" 'systemctl show docker -p ExecStop | grep -o "path=[^ ]*" || echo "no ExecStop"')"
done
