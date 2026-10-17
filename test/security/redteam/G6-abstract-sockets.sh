#!/bin/bash
# Guards: G6
# Testbed: t1
# Attacker: A1, a process in a container on a tslink network, as root inside it, with Docker's
#   default capabilities.
# Assumes: E1, E2, E3
# Attack: the container lists the Unix sockets of its network namespace, which it shares with
#   its tailscaled, in /proc/net/unix, and connects to every stream socket there: each abstract
#   one (@...) and each one whose path it can open. It gets through if one answers a LocalAPI
#   status request (BackendState) as tailscaled's LocalAPI does.
# Control: a second container with the plugin's socket directory bind-mounted where tailscaled
#   binds its socket (/data/sock), which violates E2, so a passing probe reports INFO.
#   Teardown: that container is removed.
# shellcheck disable=SC2329,SC2119 # attacks and controls run through lib.sh's eval
set -uo pipefail
# shellcheck source=../t1.sh
. test/security/t1.sh
# shellcheck source=../lib.sh
. test/security/lib.sh
t1_up

t1_net rt-bob bob || t1_error "could not create rt-bob"
docker run -d --name rt-sock --network rt-bob "$T1_PYTHON" sleep 3600 >/dev/null
t1_wait_ip rt-sock
TARGET=rt-sock

# localapi_answers: whether a socket $TARGET finds answers as the LocalAPI
localapi_answers() {
	docker exec "$TARGET" python3 -c 'import socket, sys
req = b"GET /localapi/v0/status?peers=false HTTP/1.0\r\nHost: local-tailscaled.sock\r\nSec-Tailscale: localapi\r\n\r\n"
tried = 0
for line in open("/proc/net/unix").read().splitlines()[1:]:
    f = line.split()
    # Type 0001 is SOCK_STREAM; a socket without a path is unnamed
    if len(f) < 8 or f[4] != "0001":
        continue
    name = f[7]
    addr = "\0" + name[1:] if name.startswith("@") else name
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(2)
    try:
        s.connect(addr)
        tried += 1
        s.sendall(req)
        data = b""
        while len(data) < 1 << 16:
            chunk = s.recv(4096)
            if not chunk:
                break
            data += chunk
    except OSError:
        continue
    finally:
        s.close()
    if b"BackendState" in data:
        print("the LocalAPI answers on", name)
        sys.exit(0)
print(tried, "sockets connected, none is the LocalAPI")
sys.exit(1)'
}
mount_sockets() {
	docker run -d --name rt-sockmount --network rt-bob -v "$T1_DATA/sock:/data/sock" "$T1_PYTHON" sleep 3600 >/dev/null
	t1_wait_ip rt-sockmount
	TARGET=rt-sockmount
}
unmount_sockets() {
	TARGET=rt-sock
	docker rm -f rt-sockmount >/dev/null
}

probe_group "sockets of the container's namespace" mount_sockets unmount_sockets 30
probe G6 "a container reaches its tailscaled's LocalAPI through a socket of its namespace" localapi_answers
run_probes
probe_exit "with the plugin's socket directory mounted (E2 violated), a container reaches its tailscaled's LocalAPI"
