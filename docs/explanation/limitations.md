---
title: Limitations
description: What tslink can't do, why, and how to work with it, from start-up timing to headscale and Tailnet Lock.
category: explanation
tags: [limitations, dns, startup, headscale, tailnet-lock]
---

# Limitations

Every design has edges. These are tslink's, with the reason for each and what you can do about it.

## Linux only

tslink is a Linux network plugin. On macOS and Windows, it runs inside the Linux VM of OrbStack or Docker Desktop.

## MagicDNS

Containers reach tailnet nodes by address out of the box. To resolve MagicDNS names too, make `100.100.100.100` the
container's first DNS server:

```yaml
services:
  app:
    dns:
      - 100.100.100.100
      - 10.0.0.53
```

tailscaled then forwards every other name to the container's other DNS servers, here `10.0.0.53`, or to the host's
if the container has none (see [DNS](architecture.md#dns)).

It has to be first: Docker sends each query to the first server only, and takes whatever it answers.

## Start-up delay

While a container starts, Docker doesn't tell the plugin which container it is. So tslink brings up the node once the
container is running, and your application starts before its node does. tailscaled needs a few seconds.

In that window, connections to tailnet addresses fail at once with "host unreachable"; they never leak out through the
host's Tailscale. An application that needs the tailnet the moment it starts has to retry.

## Plugin upgrades pause tailnet traffic

An upgrade restarts every tailscaled on the host. The containers keep their nodes, but their tailnet traffic stops
for 3 to 3.5 seconds, measured in a cluster test and on a reinstall.

## Docker restarts without the drain drop-in

When Docker stops, it sends no events, so without the
[drain drop-in](../guides/zero-downtime-updates.md#install-the-drain-drop-in) tslink can't drain the host's backends.
In the cluster test, callers connected to them failed for 7 to 16 seconds. With the drop-in, `systemctl restart
docker` and `systemctl reboot` cost one failed request or none. The drop-in can't help when dockerd crashes.

## Power loss and dockerd crashes

After a power loss or a dockerd crash, ephemeral nodes stay in your tailnet, offline, until the control server
removes them, which took an hour in the cluster test. When tslink starts again, it logs out the ephemeral nodes no
container uses.

## Slow registrations

When a node registers, tslink waits up to 5 minutes for the control server's answer. If it comes later, tslink
restarts tailscaled, which registers a second node. The first node stays in your tailnet, offline, holding the
hostname, so the control server names the second one `<hostname>-1`.

tslink can't remove the first node: it has no key for it. It reports the name as a `node-name` warning in the status
file and the plugin log, and you remove the first node in the admin console; see
[troubleshooting](../guides/troubleshoot.md#the-node-gets-the-name-hostname-1).

## headscale

tslink is built and tested for Tailscale's control server. It also works with
[headscale](https://github.com/juanfont/headscale), through the plugin setting
[`TSLINK_LOGIN_SERVER`](../reference/plugin-settings.md#tslink_login_server), and the end-to-end test runs against
headscale. Support for headscale is best-effort:

- Features headscale doesn't have, such as Tailscale Services, aren't available.
- tslink won't get worse with Tailscale to get better with headscale.
- Issues that occur only with headscale get fixed where possible.

## Tailnet Lock

tslink isn't tested with [Tailnet Lock](https://tailscale.com/docs/features/tailnet-lock). Each container's
tailscaled is a node of its own, and an ephemeral container is a new node every time it starts, while Tailnet Lock
needs a trusted signing key to sign each new node before it can talk to other nodes.

tslink is expected to work only with a reusable auth key that's pre-signed with `tailscale lock sign`: with such a
key, each node signs its own node key as it registers. It isn't expected to work with an OAuth client secret or the cluster
credential, because the auth keys tslink makes from them aren't signed.

## Workload identity federation

tslink doesn't support workload identity federation yet. This section sketches how it could.

[Workload identity federation](https://tailscale.com/docs/features/workload-identity-federation) would do away with
OAuth client secrets. Since Tailscale v1.92.1, `tailscale up` accepts
`--client-id=<id>?ephemeral=true&preauthorized=true --id-token=file:<path>` and exchanges a signed OIDC token for a
node key. Each federated credential matches the token's `sub` claim, with wildcards, and optional custom claims, and
sets the tags. A custom token issuer works if Tailscale can reach its discovery document and JWKS on the public
internet.

Swarm has no token issuer, and neither do hosts without a cloud provider's workload identity, such as bare-metal
servers. So a cluster would need an issuer of its own:

1. A token issuer service on the managers holds one signing key for the cluster, as a Docker secret only it mounts,
   and serves its discovery document and JWKS at a public HTTPS URL.
2. tslink asks the issuer for a token for a task, authenticating with the node certificate Swarm gives each node,
   whose common name is the node ID.
3. The issuer asks the manager whether the task runs on that node, reads its stack, and signs a short-lived token
   with the `sub` claim `swarm:<cluster>:<stack>`.
4. The stack's network then holds only a client ID, which isn't a secret.

The trust boundary would stay the same: whoever can deploy into a stack, or is root on a host with the stack's tasks,
acts as that stack. The cost is a new component, a public URL, and a dependency on the issuer whenever tasks start.
The tag rules and the stack check wouldn't change, only where the credential comes from.

Before this is built, it has to be proved end to end: a minimal issuer, one federated credential, and one
`tailscale up --id-token` from a task.
