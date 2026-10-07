#!/bin/bash
# teardown.sh -- delete every test device, the test Services and OAuth clients created through
# tsapi (descriptions starting with tslink-test), and all Hetzner resources labelled
# purpose=tslink-test. The API credential itself, the Hetzner project and the policy entries
# are left for a human to remove.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
for id in $("$B/tsapi" GET '/tailnet/-/devices' | jq -r '.devices[]|select(any(.tags[]?; startswith("tag:tslink-test")))|.nodeId'); do
	"$B/tsapi" DELETE "/device/$id" >/dev/null && echo "deleted device $id"
done
for svc in svc:tslink-test-callee svc:tslink-test-other; do "$B/tsapi" DELETE "/tailnet/-/services/$svc" >/dev/null && echo "deleted $svc"; done
for id in $("$B/tsapi" GET '/tailnet/-/keys' | jq -r '.keys[]|select((.description // "")|startswith("tslink-test"))|.id'); do
	"$B/tsapi" DELETE "/tailnet/-/keys/$id" >/dev/null && echo "revoked key $id"
done
HCLOUD_TOKEN=$(cat ~/.tslink-test/hcloud.token)
export HCLOUD_TOKEN
for n in $(hcloud server list -l purpose=tslink-test -o noheader -o columns=name); do hcloud server delete "$n"; done
hcloud firewall delete tslink-test
hcloud network delete tslink-test
hcloud ssh-key delete tslink-test
hcloud server list
rm -f "$B/ssh_config"
