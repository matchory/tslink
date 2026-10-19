#!/bin/bash
# Guards: G4
# Testbed: tn
# Needs: services
# Attacker: A3, the author of a stack file that passed review.
# Assumes: E1, E2, E3, E4, E7, and of E8 that control approves the Service only for its owner
# Attack: with a certificate directory shared by every node, stack B (rt-lease, on the caller
#   credential and tag), deployed first, labels stack A's Service ($TSLINK_TEST_SVC-callee) for
#   HTTPS and so holds a certificate lease for its name, which control never lets B issue; then
#   A (rt-victim, on the callee credential and tag) serves the Service over HTTPS. Both use
#   tslink.service and tslink.serve.<port>, which the control's release reads too. It gets
#   through if A's certificate (in certs/by-stack/rt-victim/, or certs/ itself for a release
#   without per-stack directories) is not issued within TSLINK_REDTEAM_LEASE_WAIT (default 180)
#   s while B runs. Each attack starts without a certificate for the name, so the probe issues
#   up to two, as regress.sh R7 issues one.
# Control: v0.3.4, the release whose stacks share one certificate directory, installed with the
#   shared directory. Teardown: the build under test (TSLINK_REDTEAM_REF, default WT) with the
#   shared directory; at exit, both stacks removed, the build under test reinstalled without
#   the shared directory, the shared directory removed and the callee stack's replicas restored.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../tn.sh
. "$(dirname "$0")/../tn.sh" 2>/dev/null || {
	echo "SKIPPED: a probe of the Swarm bed (tn)"
	exit 3
}
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
REF=${TSLINK_REDTEAM_REF:-WT}
OLD=v0.3.4
WAIT=${TSLINK_REDTEAM_LEASE_WAIT:-180}
SVC=${TSLINK_TEST_SVC#svc:}-callee
CERTS=/mnt/tslink/certs
for s in rt-lease rt-victim; do
	tn_stack_exists "$s" && tn_error "a stack $s is deployed: remove it first"
done
suffix=$(tn_s "$M" 'tailscale status --json | jq -r .MagicDNSSuffix')
[ -n "$suffix" ] || tn_error "no MagicDNS suffix from $M's tailscaled"
dom=$SVC.$suffix
# The callee stack's backends serve no HTTPS: rt-victim's task alone is A
r=$(replicas callee_whoami)
ro=$(replicas callee_whoami-other)
scale callee_whoami 0
scale callee_whoami-other 0
tn_on_exit "scale callee_whoami $r; scale callee_whoami-other $ro"
"$C/shared-dir.sh" on >/dev/null || tn_error "could not share a certificate directory"
tn_on_exit "\"$C/shared-dir.sh\" off >/dev/null"
# install REF: installs REF with the shared directory
install() {
	PLUGIN_SETTINGS='shared.source=/mnt/tslink' "$C/install-plugin.sh" "$1" >/dev/null 2>&1 ||
		tn_error "could not install $1 with the shared directory"
}
install "$REF"
tn_on_exit "\"$C/install-plugin.sh\" $REF >/dev/null 2>&1"
tn_on_exit "stack_rm rt-lease; stack_rm rt-victim"

# https_stack TAG: a stack file serving the Service over HTTPS on the credential's TAG suffix
https_stack() {
	cat <<EOF
services:
  web:
    image: python:3.13-alpine
    command: ["sh","-c","exec python -m http.server 80"]
    networks: [tailnet]
    labels:
      tslink.service: \${TSLINK_TEST_SVC}-callee
      tslink.serve.443: https:80
      tslink.direct: "false"
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      tslink.authkey: \${TSLINK_OAUTH_SECRET}?ephemeral=true&preauthorized=true
      com.docker.network.driver.mtu: "\${TSLINK_TEST_MTU:-1450}"
      tslink.tags: \${TSLINK_TEST_TAG:-tag:tslink-test}-$1
EOF
}
leased() { tn_s "$M" "find $CERTS -name '$dom.lease' | grep -q ."; }
issued() { tn_s "$M" "test -e $CERTS/by-stack/rt-victim/$dom.crt || test -e $CERTS/$dom.crt"; }

# blocks_victim: deploys B, then A once B holds the lease, and whether A's certificate is not
# issued while B runs; removes both, and the name's certificate files
blocks_victim() {
	local held=0
	tn_s "$M" "find $CERTS -name '$dom.*' -delete" || return 1
	https_stack caller | tn_deploy rt-lease caller >/dev/null || return 1
	if ! wait_for 180 leased; then
		echo "rt-lease did not take the lease for $dom within 180 s"
		stack_rm rt-lease
		return 1
	fi
	if https_stack callee | tn_deploy rt-victim callee >/dev/null &&
		! wait_for "$WAIT" issued; then
		echo "rt-victim's certificate for $dom not issued within $WAIT s while rt-lease ran"
		held=1
	fi
	stack_rm rt-lease
	stack_rm rt-victim
	[ "$held" = 1 ]
}

probe_group "certificate leases across stacks" "install $OLD" "install $REF" 0
probe G4 "stack rt-lease's lease for $dom holds up stack rt-victim's certificate" blocks_victim
run_probes
probe_exit
