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

## Scope

tslink is privileged by design. Keep this in mind when judging the impact of a
finding:

- **The plugin runs as root on the host** with host networking,
  `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`, `/dev/net/tun`, the Docker socket, the
  host's `/run` and `/var/lib/docker-plugins/tailscale`. It enters container
  network namespaces and changes interfaces, routes and firewall rules. A flaw
  that lets a container or a tailnet peer influence what the plugin does on
  the host is in scope.
- **Credentials are visible to Docker API holders.** Tailscale auth keys and
  OAuth client secrets passed as network options (`tslink.authkey`) or plugin
  settings (`TS_AUTHKEY`) can be read with `docker network inspect` or
  `docker plugin inspect`, and on Swarm they are stored in the managers' raft
  store. Access to the Docker API is root on the node, so this is expected,
  not a vulnerability. See [docs/credentials.md](docs/credentials.md).
  Credentials that leak elsewhere, for example into logs, the host's process
  list or the application container, are in scope.
- **Isolation between containers and stacks is in scope**: a container
  reaching the tailnet through the host's or another container's Tailscale, a
  task choosing tags or an identity its network does not grant it, or a stack
  using another stack's network or state.
- **State on disk**: `/var/lib/docker-plugins/tailscale` holds each node's
  Tailscale state (its private keys). Anyone who can read it can impersonate
  those nodes.
