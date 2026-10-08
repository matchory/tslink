# Security policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/matchory/tslink/security/advisories/new)
on matchory/tslink. Do not open a public issue, pull request or discussion for
them.

Include what is affected, how to reproduce it, and the impact you expect. We
will confirm receipt, keep you informed while we work on a fix, and credit you
in the advisory unless you prefer otherwise.

Vulnerabilities in Tailscale itself go to
[Tailscale](https://tailscale.com/security), and those in Docker to the
[Moby project](https://github.com/moby/moby/security/policy).

## Supported versions

Fixes go into the latest release. Older releases are not patched.

## Security model

tslink makes the guarantees below, provided the environment properties after
them hold. Each guarantee names who it defends against and which properties
it relies on. Tests that verify a guarantee carry its ID (`Guards: G1`); a
check in the test suite fails if a guarantee listed here has none. Models of
tslink's state machines verify their design, not the code.

### G1: A container reaches the tailnet only as its own node

Attackers: a process in a container on a tslink network, as root inside it,
with Docker's default capabilities.
Assumes: E1, E2, E3, E5.

A container cannot reach the tailnet through the host's own Tailscale, by
sockets or by raw frames, through tslink's veth or through Docker's gateway.
This holds with the plugin setting `TSLINK_ISOLATE_HOST_TAILNET` at its
default, `true`.

### G2: Containers do not reach each other through tslink's veths

Attackers: a process in any container on the host, on a tslink network or
any other, as root inside it, with Docker's default capabilities; a host on
the LAN that routes tslink's veth range through the Docker host.
Assumes: E1, E2, E3, and of E5 the mangle table and the plugin's privileges:
this isolation is on whatever `TSLINK_ISOLATE_HOST_TAILNET` says.

Each tslink container's tailscaled has a veth pair in `10.200.0.0/16`, and
the host forwards between them. Nobody reaches a tslink container's veth
address through that, by sockets or by raw frames, past Docker's network
isolation and the tailnet's ACLs, whatever interface the traffic comes from,
a bridge with its own name included. The host forwards to tslink's veths only
replies on connections their container opened, and WireGuard (UDP 41641)
from other tslink veths, so colocated nodes keep their direct path. That UDP
port is open to tslink's other containers: WireGuard drops what is not from a
peer.

### G4: A stack's nodes get only what its own network grants

Attackers: the author of a stack file that passed review.
Assumes: E1, E2, E3, E4, E8.

A network that belongs to a stack gives tailnet nodes only to that stack's
tasks. With the cluster credential, a stack's nodes get only the tag
`tag:<stack>`, with the plugin setting `TSLINK_TAG_SCOPE` at its default,
`exact`; `prefix` also allows `tag:<stack>-*`, which lets a stack claim a
longer-named stack's base tag. Networks outside a stack cannot use the
cluster credential.

### G5: Credentials do not leak

Attackers: a process in a container on a tslink network; a tailnet peer; the
control server.
Assumes: E2, E3.
Manual: OAuth client secrets (the cluster credential) are searched for by
hand on a test tailnet before each release.

Auth keys and OAuth client secrets do not appear in these places:

- the plugin log;
- the command lines of tailscale and tailscaled;
- the environment and the log of tailscaled;
- status files;
- the output of `tslink diag`.

Persons with access to the Docker API can read them in network options and
plugin settings. See "Scope".

### Environment properties

| ID | Property | Ensured by | `tslink diag --preflight` |
| --- | --- | --- | --- |
| E1 | Containers on tslink networks are not privileged and have neither `NET_ADMIN` nor `SYS_ADMIN` | stack review, CI policy | checks |
| E2 | No container on a tslink network bind-mounts Docker's socket, `/run/netns`, Docker's data root (`/var/lib/docker` by default), the plugin's data directory or a directory containing them, or shares the host's PID namespace | stack review, CI policy | checks |
| E3 | Only operators have Docker API access and root on hosts | operator | does not check |
| E4 | Stacks are deployed with `docker stack deploy`, which sets `com.docker.stack.namespace` | operator, CI | lists Swarm tasks outside stacks |
| E5 | The host supports the iptables mangle table for IPv4 and IPv6, the plugin runs with the privileges of its `config.json`, and its setting `TSLINK_ISOLATE_HOST_TAILNET` is at its default, `true` | operator | checks the setting, and that tslink's isolation chains lead FORWARD and INPUT |
| E6 | The plugin is installed from a release image pinned by digest | operator | checks the digest pin |
| E7 | The shared certificate directory is owned by root, mode 0700 or stricter, and reachable only by the cluster's hosts | operator | checks owner and mode, with `--shared-dir` |
| E8 | The cluster credential owns only stack-prefixed tags, stack names follow tslink's rules, and the plugin setting `TSLINK_TAG_SCOPE` is at its default, `exact` | operator, tailnet policy | checks the tag scope setting |
| E9 | Tailnet Lock is on where peer identity must not depend on the control server | operator | reports whether it is on |

Run `tslink diag --preflight` on each host, and on each node of a Swarm. It
checks only the properties that it can see on the host where it runs.
[tslink diag](docs/reference/diag.md) shows how.

## Scope

tslink is privileged by design. Think of this when you assess the impact of a
finding:

- **The plugin runs as root on the host** with host networking,
  `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `/dev/net/tun`, the Docker socket, the
  host's `/run` and `/var/lib/docker-plugins/tailscale`. It enters container
  network namespaces and changes interfaces, routes and firewall rules. A flaw
  that lets a container or a tailnet peer influence what the plugin does on
  the host is in scope.
- **Credentials are visible to persons with access to the Docker API.**
  `docker network inspect` shows the network option `tslink.authkey`, and
  `docker plugin inspect` shows the plugin setting `TS_AUTHKEY`. On a Swarm,
  the Raft store of the managers keeps them. Access to the Docker API is root
  on the host, so this is expected and not a vulnerability. See
  [Where credentials are visible](docs/explanation/security-model.md#where-credentials-are-visible).
  A credential that leaks to another place is in scope, for example to a log,
  the process list of the host or the application container.
- **Isolation between containers and stacks is in scope.** Examples:
  - A container reaches the tailnet through the Tailscale of the host or of
    another container.
  - A task gets tags or an identity that its network does not give it.
  - A stack uses the network or the state of another stack.
- **State on disk**: `/var/lib/docker-plugins/tailscale` holds each node's
  Tailscale state (its private keys). Anyone who can read it can impersonate
  those nodes.
