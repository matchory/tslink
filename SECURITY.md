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
check in the test suite fails if a guarantee listed here has none.

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

### G3: Labels, options and names do not make the plugin act outside their container

Attackers: a process in a container on a tslink network, as root inside it,
with Docker's default capabilities; the author of a stack file that passed
review.
Assumes: E1, E2, E3.

The plugin runs as root on the host. Labels (`tslink.hostname`,
`tslink.service`, `tslink.serve.<port>`,
`tslink.service.<name>.serve.<port>`), network options, container and stack
names and the container's DNS settings do not make it act on anything but
the container they belong to and its own state:

- A Service name is a valid, lower-case Tailscale Service name, and a port
  is a number.
- A serve value forwards only to a port on the container's loopback, under
  its own Service or the node's own name.
- A hostname or stack name names a state directory inside the plugin's data
  directory only (see G6).
- tailscaled's `resolv.conf` holds only nameserver lines with an address and
  at most one search and one options line.

The plugin configures tailscaled through its LocalAPI with typed values,
never through the `tailscale` CLI, and runs fixed binaries with argument
arrays. Its one shell script, which starts tailscaled, takes values only as
arguments.

### G4: A stack's nodes get only what its own network grants

Attackers: the author of a stack file that passed review.
Assumes: E1, E2, E3, E4, E8.

A network that belongs to a stack gives tailnet nodes only to that stack's
tasks. With the cluster credential, a stack's nodes get only the tag
`tag:<stack>`, with the plugin setting `TSLINK_TAG_SCOPE` at its default,
`exact`; `prefix` also allows `tag:<stack>-*`, which lets a stack claim a
longer-named stack's base tag. Networks outside a stack cannot use the
cluster credential.

A stack can label any Service name; whether control makes its nodes backends
depends on the tailnet policy's auto-approvers (E8).

### G5: Credentials do not leak

Attackers: a process in a container on a tslink network; a tailnet peer; the
control server.
Assumes: E2, E3.

Auth keys and OAuth client secrets do not appear in these places:

- the plugin log and status files;
- the command line and environment of the plugin and of tailscaled, except
  that the plugin setting `TS_AUTHKEY` is in the plugin's environment;
- tailscaled's state and log, and the other files in the plugin's data
  directory, but for `oauth-client.secret`, which holds the cluster
  credential;
- the containers;
- the output of `tslink diag`.

tslink hands a key to tailscaled only through its LocalAPI. An OAuth client
secret goes only to the Tailscale API (`api.tailscale.com`, or the server
its `baseURL` attribute names), which mints a single-use auth key from it;
only that key goes to the control server.

Persons with access to the Docker API can read them in network options and
plugin settings. See "Scope".

### G6: A container reaches neither its node's state nor its tailscaled's LocalAPI

Attackers: a process in a container on a tslink network, as root inside it,
with Docker's default capabilities; the author of a stack file that passed
review.
Assumes: E1, E2, E3.

Each node's Tailscale state, with its private keys, and its tailscaled's
LocalAPI are reachable only by the plugin and root on the host. tailscaled
shares only the network namespace with its container: it runs in a mount
namespace of its own, and its LocalAPI is a Unix socket at a path in the
plugin's data directory, which the container does not see. State
directories are `by-hostname/<hostname>/`, or `by-stack/<stack>/<hostname>/`
for stack tasks, in the plugin's data directory, created with mode 0700. A
hostname or stack name names no other path, and each state directory serves
one endpoint at a time. A certificate's files are named after a validated DNS
name, so neither a Service name nor the tailnet's DNS suffix from control
names a file outside the certificate directory.

### Environment properties

| ID | Property | Ensured by | `tslink diag --preflight` |
| --- | --- | --- | --- |
| E1 | Containers on tslink networks, and the other containers of their stacks, are not privileged and have neither `NET_ADMIN` nor `SYS_ADMIN`, nor namespaced `sysctls` that enable forwarding, nor unconfined seccomp or AppArmor | stack review, CI policy | checks privileges, capabilities, forwarding sysctls and unconfined seccomp or AppArmor |
| E2 | No container on a tslink network, nor another container of its stack, bind-mounts Docker's socket, `/run/netns`, Docker's data root (`/var/lib/docker` by default), the plugin's data directory (`/var/lib/docker-plugins/tailscale`, or wherever it was moved), the shared certificate directory, or a directory containing them, or shares the host's PID namespace | stack review, CI policy | checks, including the shared certificate directory (each tslink plugin's own `shared.source` setting, the host path), but not a moved plugin data directory |
| E3 | Only operators have Docker API access and root on hosts | operator | does not check |
| E4 | Stacks are deployed with `docker stack deploy`, which sets `com.docker.stack.namespace` | operator, CI | lists Swarm tasks outside stacks |
| E5 | The host supports the iptables mangle table for IPv4 and IPv6, the plugin runs with the privileges of its `config.json`, and its setting `TSLINK_ISOLATE_HOST_TAILNET` is at its default, `true` | operator | checks the setting, and that tslink's isolation chains lead FORWARD and INPUT and drop traffic; not the plugin's privileges |
| E6 | The plugin is installed from a release image pinned by digest | operator | checks the digest pin |
| E7 | The shared certificate directory is owned by root, mode 0700 or stricter, and reachable only by the cluster's hosts | operator | checks owner and mode, with `--shared-dir` |
| E8 | The cluster credential owns only stack-prefixed tags, stack names follow tslink's rules, the plugin setting `TSLINK_TAG_SCOPE` is at its default, `exact`, and the policy auto-approves each Service only for the tag of the stack that owns it | operator, tailnet policy | checks the tag scope setting |
| E9 | The identity of tslink's nodes depends on the control server, unless Tailnet Lock is on, which tslink is not tested with ([Limitations](docs/explanation/limitations.md#tailnet-lock)) | operator | reports whether it is on |

Run `tslink diag --preflight` on each host, and on each node of a Swarm. It
checks only the properties that it can see on the host where it runs.
[tslink diag](docs/reference/diag.md) shows how.

tslink is not tested with Tailnet Lock (E9). It is expected to work only with
a reusable auth key pre-signed with `tailscale lock sign`, not with an OAuth
client secret or the cluster credential, because tslink makes auth keys from
them that are not signed. See
[Limitations](docs/explanation/limitations.md#tailnet-lock).

The range of `TSLINK_WIREGUARD_PORTS` is reserved: a host service listening
in it gets no unsolicited UDP.

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
  Tailscale state (its private keys), and `oauth-client.secret`, the cluster
  credential. Anyone who can read it can impersonate those nodes, or register
  nodes with the tags of every stack. The shared certificate directory holds
  every Service's TLS private key and the ACME account key: anyone who can
  read it can impersonate those Services (E7).
- **Known limitation**: A stack that names another stack's Service can hold
  that Service's certificate lease in the shared directory, which blocks its
  issuance and renewal for as long as that task runs, and after
  `certDomainGrace` advertises the Service; operators who share a certificate
  directory across stacks rely on E8 and stack review.
