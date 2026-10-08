#!/bin/bash
# sync.sh [TREE]: copy a working tree (tracked and new, not ignored, files) to
# /root/tslink on the security testbed. TREE defaults to this checkout.
set -euo pipefail
B=$(cd "$(dirname "$0")" && pwd)
cd "${1:-$B/../../..}"
{ git ls-files; git ls-files --others --exclude-standard; } |
	COPYFILE_DISABLE=1 tar --no-xattrs -T - -czf - |
	"$B/s" 'rm -rf /root/tslink && mkdir -p /root/tslink && tar -xzf - -C /root/tslink'
