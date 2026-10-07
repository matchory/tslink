#!/bin/bash
# add-node.sh <name>... -- Hetzner Cloud provisioner: add cpx32 workers (e.g. w3 w4) to the test
# swarm with provision.sh's settings, add them to $TSLINK_TEST_SSH_CONFIG, install the plugin last
# built by install-plugin.sh as tslink, and join their host tailscaled with node.authkey as
# $TSLINK_TEST_TAG-node
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
C=$TSLINK_TEST_SSH_CONFIG
HCLOUD_TOKEN=$(cat "$TSLINK_TEST_DIR/hcloud.token")
export HCLOUD_TOKEN
S="$B/s"
ready() {
	for n in "$@"; do "$S" "$n" test -f /var/lib/cloud-init-done 2>/dev/null || return 1; done
}
for n in "$@"; do
	hcloud server describe "tslink-test-$n" >/dev/null 2>&1 && continue
	hcloud server create --name "tslink-test-$n" --type cpx32 --location fsn1 --image ubuntu-24.04 \
		--ssh-key tslink-test --network tslink-test --firewall tslink-test \
		--user-data-from-file "$B/cloud-init.yaml" --label purpose=tslink-test --label role="$n" >/dev/null &
done
wait
for n in "$@"; do
	grep -qx "Host $n" "$C" && continue
	# insert before the "Host *" block, whose settings apply to every node
	awk -v n="$n" -v ip="$(hcloud server ip "tslink-test-$n")" \
		'$0 == "Host *" { printf "Host %s\n  HostName %s\n", n, ip } { print }' "$C" >"$C.new"
	mv "$C.new" "$C"
done
until ready "$@"; do sleep 10; done

MGR=$(hcloud server describe tslink-test-mgr -o json | jq -r '.private_net[0].ip')
TOKEN=$("$S" mgr docker swarm join-token -q worker)
P=tslink:latest
for n in "$@"; do
	"$S" mgr 'cat /root/plugin.tgz' | "$S" "$n" 'rm -rf /root/plugin && tar -xz -C /root'
	"$S" "$n" "set -e; docker plugin create $P /root/plugin >/dev/null; docker plugin enable $P >/dev/null"
	"$S" "$n" "tailscale up --auth-key=file:/dev/stdin --advertise-tags=$TSLINK_TEST_TAG-node --hostname=tslink-test-$n-host" <"$TSLINK_TEST_DIR/node.authkey"
	"$S" "$n" "docker swarm join --token $TOKEN $MGR:2377"
done
"$S" mgr docker node ls
