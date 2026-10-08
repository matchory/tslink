# Limitations

This page lists what tslink cannot do, and why.

## Linux only

tslink runs only on Linux. On macOS and Windows, it runs in the Linux VM of OrbStack or Docker Desktop.

## MagicDNS

Containers reach tailnet nodes by address. To resolve MagicDNS names, set the DNS server of the container to
`100.100.100.100`:

```yaml
services:
  app:
    dns:
      - 100.100.100.100
      - 10.0.0.53
```

tailscaled then forwards other names to the other DNS servers of the container, here `10.0.0.53`. If the container
has no other DNS server, tailscaled uses the DNS servers of the host. See [DNS](architecture.md#dns).

Docker sends each query to the first server and accepts its answer. Thus `100.100.100.100` must be first.

tslink reads only the `dns` setting of the container or the service. It does not read the `dns` setting in
`/etc/docker/daemon.json`.

## Start-up delay

Docker gives the plugin no way to identify a container while it starts. Thus tslink starts Tailscale after the
container has started, and the application starts first. tailscaled needs some seconds to start.

During this period, connections to tailnet addresses fail at once with "host unreachable". They do not go through the
Tailscale of the host. An application that needs the tailnet when it starts must try again.

## Plugin upgrades stop tailnet traffic

An upgrade of the plugin restarts each tailscaled on the host. The containers keep their nodes, but their tailnet
traffic stops for 3 to 3.5 seconds (measured on a cluster test and on a reinstall).

## Docker restarts without the drain drop-in

When Docker stops, it sends no events. Without the
[drain drop-in](../guides/zero-downtime-updates.md#install-the-drain-drop-in), tslink cannot drain the backends of
the host. In the cluster test, callers that were connected to these backends failed for 7 to 16 seconds. With the
drop-in, `systemctl restart docker` and `systemctl reboot` caused one failed request or none. The drop-in does not
help when dockerd crashes.

## Power loss and dockerd crashes

After a power loss or a crash of dockerd, ephemeral nodes stay in the tailnet, offline, until the control server
removes them. In the cluster test, this took one hour. When tslink starts again, it logs out the ephemeral nodes
that no container uses.

## headscale

tslink works with [headscale](https://github.com/juanfont/headscale) as the control server, but headscale does not
support Tailscale Services.

## Workload identity federation

tslink does not support workload identity federation. This section describes how it could work.

[Workload identity federation](https://tailscale.com/docs/features/workload-identity-federation) would remove the
OAuth client secrets. Since Tailscale v1.92.1, `tailscale up` accepts
`--client-id=<id>?ephemeral=true&preauthorized=true --id-token=file:<path>`. It exchanges a signed OIDC token for a
node key. Each federated credential matches the `sub` claim of the token, with wildcards, and optional custom claims.
It also sets the tags. Custom token issuers work if Tailscale can reach their discovery document and JWKS on the
public internet.

Swarm has no token issuer. Hosts without a workload identity of a cloud provider, such as bare-metal servers, have
none either. Thus a cluster needs its own issuer:

1. A token issuer service on the managers holds one signing key for the cluster, as a Docker secret that only this
   service mounts. It serves its discovery document and JWKS at a public HTTPS URL.
2. tslink asks the issuer for a token for a task. It authenticates with the node certificate that Swarm gives to each
   node. The common name of the certificate is the node ID.
3. The issuer asks the manager if the task runs on that node, and reads its stack. Then it signs a token with a short
   lifetime and the `sub` claim `swarm:<cluster>:<stack>`.
4. The network of the stack then has only a client ID, which is not a secret.

The trust boundary stays the same: a person who can deploy into a stack, or who is root on a host with tasks of the
stack, acts as that stack. The costs are a new component, a public URL, and a dependency on the issuer when tasks
start. The tag rules and the stack check stay the same. Only the source of the credential changes.

Before this is built, it must be proved from end to end: a minimal issuer, one federated credential, and one
`tailscale up --id-token` from a task.
