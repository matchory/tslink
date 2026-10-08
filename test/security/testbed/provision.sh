#!/bin/bash
# provision.sh: create the security testbed, one Hetzner Cloud server (cpx22,
# Ubuntu 24.04, fsn1) in the project whose token is in
# $TSLINK_TEST_DIR/hcloud.token, with a firewall that admits SSH from this
# machine only. Writes $TSLINK_SEC_DIR/ssh_config. Everything is named and
# labelled tslink-security; teardown.sh deletes it.
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
: "${TSLINK_TEST_DIR:=$HOME/.tslink-test}"
: "${TSLINK_SEC_DIR:=$TSLINK_TEST_DIR/security}"
NAME=tslink-security
HCLOUD_TOKEN=$(cat "$TSLINK_TEST_DIR/hcloud.token")
export HCLOUD_TOKEN
L=(--label "purpose=$NAME")
ME=$(curl -fsS4 https://ifconfig.me)
mkdir -p "$TSLINK_SEC_DIR"
chmod 700 "$TSLINK_SEC_DIR"

hcloud ssh-key create --name $NAME --public-key-from-file ~/.ssh/id_ed25519.pub "${L[@]}" >/dev/null
hcloud firewall create --name $NAME "${L[@]}" >/dev/null
printf '[{"direction":"in","protocol":"tcp","port":"22","source_ips":["%s/32"],"description":"ssh"}]' \
	"$ME" >"$TSLINK_SEC_DIR/firewall.json"
hcloud firewall replace-rules $NAME --rules-file "$TSLINK_SEC_DIR/firewall.json" >/dev/null
hcloud server create --name $NAME --type cpx22 --location fsn1 --image ubuntu-24.04 \
	--ssh-key $NAME --firewall $NAME --user-data-from-file "$B/cloud-init.yaml" "${L[@]}" >/dev/null

cat >"$TSLINK_SEC_DIR/ssh_config" <<EOF
Host testbed
  HostName $(hcloud server ip $NAME)
  User root
  IdentityFile ~/.ssh/id_ed25519
  IdentitiesOnly yes
  StrictHostKeyChecking accept-new
  UserKnownHostsFile $TSLINK_SEC_DIR/known_hosts
  ControlMaster auto
  ControlPath $TSLINK_SEC_DIR/cm-%h
  ControlPersist 30m
EOF
until "$B/s" test -f /var/lib/cloud-init-done 2>/dev/null; do sleep 10; done
echo "testbed ready: Docker $("$B/s" docker version --format '{{.Server.Version}}')"
