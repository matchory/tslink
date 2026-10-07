#!/bin/bash
# cleanup.sh -- delete every test device (tagged $TSLINK_TEST_TAG*), the test Services and the
# auth keys and OAuth clients created through tsapi (descriptions starting with the tag prefix
# without "tag:", tslink-test by default). The API credential itself and the policy entries are
# left for a human to remove.
set -uo pipefail
B=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=config.sh
. "$B/config.sh"
TN=$TSLINK_TEST_TAILNET
for id in $("$B/tsapi" GET "/tailnet/$TN/devices" | jq -r --arg t "$TSLINK_TEST_TAG" '.devices[]|select(any(.tags[]?; startswith($t)))|.nodeId'); do
	"$B/tsapi" DELETE "/device/$id" >/dev/null && echo "deleted device $id"
done
for svc in "$TSLINK_TEST_SVC-callee" "$TSLINK_TEST_SVC-other"; do "$B/tsapi" DELETE "/tailnet/$TN/services/$svc" >/dev/null && echo "deleted $svc"; done
for id in $("$B/tsapi" GET "/tailnet/$TN/keys" | jq -r --arg d "${TSLINK_TEST_TAG#tag:}" '.keys[]|select((.description // "")|startswith($d))|.id'); do
	"$B/tsapi" DELETE "/tailnet/$TN/keys/$id" >/dev/null && echo "revoked key $id"
done
