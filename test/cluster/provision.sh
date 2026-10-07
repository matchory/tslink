#!/bin/bash
# provision.sh -- create the tslink test swarm in the Hetzner Cloud project whose token is in
# ~/.tslink-test/hcloud.token: 3 x cpx32 in fsn1, a private network, a firewall that admits SSH
# from this machine only, Docker and Tailscale via cloud-init. Then joins the swarm and writes
# ssh_config. Everything is labelled purpose=tslink-test; teardown.sh deletes it.
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
HCLOUD_TOKEN=$(cat ~/.tslink-test/hcloud.token)
export HCLOUD_TOKEN
L=(--label purpose=tslink-test)
ME=$(curl -s4 https://ifconfig.me)

hcloud ssh-key create --name tslink-test --public-key-from-file ~/.ssh/id_ed25519.pub "${L[@]}"
hcloud network create --name tslink-test --ip-range 10.77.0.0/16 "${L[@]}"
hcloud network add-subnet tslink-test --type cloud --network-zone eu-central --ip-range 10.77.0.0/24
hcloud firewall create --name tslink-test "${L[@]}"
echo "[{\"direction\":\"in\",\"protocol\":\"tcp\",\"port\":\"22\",\"source_ips\":[\"$ME/32\"],\"description\":\"ssh\"}]" >"$B/.fw.json"
hcloud firewall replace-rules tslink-test --rules-file "$B/.fw.json"
rm "$B/.fw.json"
for n in mgr w1 w2; do
	hcloud server create --name tslink-test-$n --type cpx32 --location fsn1 --image ubuntu-24.04 \
		--ssh-key tslink-test --network tslink-test --firewall tslink-test \
		--user-data-from-file "$B/cloud-init.yaml" "${L[@]}" --label role=$n >/dev/null &
done
wait

{
	for n in mgr w1 w2; do printf 'Host %s\n  HostName %s\n' "$n" "$(hcloud server ip tslink-test-$n)"; done
	printf 'Host *\n  User root\n  IdentityFile ~/.ssh/id_ed25519\n  IdentitiesOnly yes\n'
	printf '  StrictHostKeyChecking accept-new\n  UserKnownHostsFile ~/.tslink-test/known_hosts\n'
	printf '  ControlMaster auto\n  ControlPath ~/.tslink-test/cm-%%h\n  ControlPersist 30m\n'
} >"$B/ssh_config"

until for n in mgr w1 w2; do "$B/s" "$n" test -f /var/lib/cloud-init-done || exit 1; done; do sleep 10; done
"$B/s" mgr "docker swarm init --advertise-addr $(hcloud server describe tslink-test-mgr -o json | jq -r '.private_net[0].ip')" >/dev/null
TOKEN=$("$B/s" mgr docker swarm join-token -q worker)
MGR=$(hcloud server describe tslink-test-mgr -o json | jq -r '.private_net[0].ip')
for n in w1 w2; do "$B/s" "$n" "docker swarm join --token $TOKEN $MGR:2377"; done
"$B/s" mgr docker node ls
