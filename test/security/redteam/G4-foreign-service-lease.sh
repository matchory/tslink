#!/bin/bash
# Guards: G4
# Testbed: tn
# Needs: services
# Attacker: A3, the author of a stack file that passed review.
# Assumes: E1, E2, E3, E4, E7, and of E8 that control approves the Service only for its owner
# Attack: with a certificate directory shared by every node, stack B (rt-lease, on the caller
#   credential and tag) labels stack A's Service ($TSLINK_TEST_SVC-callee) for HTTPS and so
#   holds its certificate lease; then A, the multi stack, comes up. The probe records whether
#   A's certificate is issued while B runs, and how long after B leaves it is: SECURITY.md lists
#   this as a known limitation (F2), and until its fix is designed the probe reports INFO,
#   never FAIL. It issues one certificate for the Service, as regress.sh R7 does.
# Control: none: it records a behaviour.
# Teardown: both stacks removed, the plugin reinstalled without the shared directory
#   (TSLINK_REDTEAM_REF, default WT), the shared directory removed, the callee stack's replicas
#   restored.
# shellcheck disable=SC2329 # called through tn_wait_for and the exit hooks
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
REF=${TSLINK_REDTEAM_REF:-WT}
WAIT=${TSLINK_REDTEAM_LEASE_WAIT:-300}
SVC=${TSLINK_TEST_SVC#svc:}-callee
for s in rt-lease multi; do
	tn_stack_exists "$s" && tn_error "a stack $s is deployed: remove it first"
done
suffix=$(tn_s "$M" 'tailscale status --json | jq -r .MagicDNSSuffix')
[ -n "$suffix" ] || tn_error "no MagicDNS suffix from $M's tailscaled"
dom=$SVC.$suffix
replicas() { tn_s "$M" "docker service inspect $1 --format '{{.Spec.Mode.Replicated.Replicas}}' 2>/dev/null"; }
scale() { [ -z "$2" ] || tn_s "$M" "docker service scale -d $1=$2 >/dev/null"; }
# The callee stack's backends serve no HTTPS: the multi tasks alone are A, as in R7
r=$(replicas callee_whoami)
ro=$(replicas callee_whoami-other)
scale callee_whoami 0
scale callee_whoami-other 0
tn_on_exit "scale callee_whoami $r; scale callee_whoami-other $ro"
"$C/shared-dir.sh" on >/dev/null || tn_error "could not share a certificate directory"
tn_on_exit "\"$C/shared-dir.sh\" off >/dev/null"
PLUGIN_SETTINGS='shared.source=/mnt/tslink' "$C/install-plugin.sh" "$REF" >/dev/null 2>&1 ||
	tn_error "could not install $REF with the shared directory"
tn_on_exit "\"$C/install-plugin.sh\" $REF >/dev/null 2>&1"
tn_on_exit "tn_stack_rm rt-lease; tn_stack_rm multi"

has() { tn_s "$M" "test -e /mnt/tslink/$dom.$1"; }
tn_deploy rt-lease caller >/dev/null <<EOF || tn_error "could not deploy rt-lease"
services:
  web:
    image: python:3.13-alpine
    command: ["sh","-c","exec python -m http.server 80"]
    networks: [tailnet]
    labels:
      tslink.service.$SVC.serve.443: https:80
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      tslink.authkey: \${TSLINK_OAUTH_SECRET}?ephemeral=true&preauthorized=true
      com.docker.network.driver.mtu: "\${TSLINK_TEST_MTU:-1450}"
      tslink.tags: \${TSLINK_TEST_TAG:-tag:tslink-test}-caller
EOF
tn_wait_for 180 has lease || tn_error "rt-lease did not take the lease for $dom within 180 s"
echo "rt-lease holds the certificate lease for $dom"
"$C/deploy" multi callee >/dev/null || tn_error "could not deploy multi"
start=$SECONDS
if tn_wait_for "$WAIT" has crt; then
	during="issued $((SECONDS - start)) s after A came up, while B held the lease"
else
	during="not issued within $WAIT s while B held the lease"
fi
echo "$during"
tn_stack_rm rt-lease
start=$SECONDS
if [[ $during == issued* ]]; then
	after="B left after it"
elif tn_wait_for "$WAIT" has crt; then
	after="issued $((SECONDS - start)) s after B left"
else
	after="not issued within $WAIT s after B left"
fi
echo "$after"
echo "INFO    A's certificate for $dom: $during; $after"
exit 4
