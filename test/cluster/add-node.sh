#!/bin/bash
# add-node.sh <name>... -- add cpx32 workers (e.g. w3 w4) to the test swarm with provision.sh's
# settings, add them to ssh_config, install the plugin last built by install-plugin.sh, and join
# their host tailscaled with node.authkey as tag:tslink-test-node
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
HCLOUD_TOKEN=$(cat ~/.tslink-test/hcloud.token)
export HCLOUD_TOKEN
S="$B/s"
for n in "$@"; do
	hcloud server create --name "tslink-test-$n" --type cpx32 --location fsn1 --image ubuntu-24.04 \
		--ssh-key tslink-test --network tslink-test --firewall tslink-test \
		--user-data-from-file "$B/cloud-init.yaml" --label purpose=tslink-test --label role="$n" >/dev/null &
done
wait
for n in "$@"; do
	# insert before the "Host *" block, whose settings apply to every node
	awk -v n="$n" -v ip="$(hcloud server ip "tslink-test-$n")" \
		'$0 == "Host *" { printf "Host %s\n  HostName %s\n", n, ip } { print }' "$B/ssh_config" >"$B/ssh_config.new"
	mv "$B/ssh_config.new" "$B/ssh_config"
done
until for n in "$@"; do "$S" "$n" test -f /var/lib/cloud-init-done || exit 1; done; do sleep 10; done

MGR=$(hcloud server describe tslink-test-mgr -o json | jq -r '.private_net[0].ip')
TOKEN=$("$S" mgr docker swarm join-token -q worker)
P=ghcr.io/matchory/tslink:latest
for n in "$@"; do
	"$S" mgr 'cat /root/plugin.tgz' | "$S" "$n" 'rm -rf /root/plugin && tar -xz -C /root'
	"$S" "$n" "set -e; docker plugin create $P /root/plugin >/dev/null; docker plugin enable $P >/dev/null"
	"$S" "$n" "tailscale up --auth-key=file:/dev/stdin --advertise-tags=tag:tslink-test-node --hostname=tslink-test-$n-host" <~/.tslink-test/node.authkey
	"$S" "$n" "docker swarm join --token $TOKEN $MGR:2377"
done
"$S" mgr docker node ls
