---
title: Troubleshoot
description: Find the cause of a symptom, from a plugin that won't enable to a Service backend that gets no traffic.
category: guide
tags: [troubleshooting, logs, diagnostics]
---

# Troubleshoot

Find your symptom below and work through its causes from the top, checking each before you move to the next. Most
checks need the logs, so collect them first.

## Collect information

1. Run [`tslink diag`](../reference/diag.md), which shows the state of every endpoint.

2. Show the end of each node's tailscaled log:

   ```bash
   docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
     sh -c 'for d in /data/by-hostname/*/ /data/by-stack/*/*/; do
       echo "=== $d ==="; tail -20 "$d/tailscaled.log" 2>/dev/null
     done'
   ```

3. Show the last hour of the plugin's log. On Linux:

   ```bash
   journalctl -u docker --since -1h | grep -i tailscale
   ```

   On macOS with OrbStack, open a shell in the Linux VM first, then run the same command there:

   ```bash
   docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
   ```

   The plugin also keeps its log in `plugin.log` in the data directory.

## The plugin doesn't enable

| Cause | Check | Fix |
| --- | --- | --- |
| The plugin can't create or mount its data directory | `tslink:` in the Docker daemon log, for example `journalctl -u docker \| grep tslink:` | Fix what the message names, such as a full or read-only `/var/lib`. |

## `docker network create` fails

| Cause | Check | Fix |
| --- | --- | --- |
| No credential | The error contains `no Tailscale credential` | Set `tslink.authkey`, `TS_AUTHKEY` or the cluster credential. See [Precedence](../reference/credentials.md#precedence). |
| `tslink.tags` is missing | The error contains `tslink.tags is required` | Add `tslink.tags` to the network. |
| An auth key has parameters | The error contains `has parameters appended` | Remove `?…` from the auth key. |
| `tslink.ephemeral` contradicts the credential | The error names `tslink.ephemeral` | See [tslink.ephemeral](../reference/network-options.md#tslinkephemeral). |

## The container runs, but has no node

| Cause | Check | Fix |
| --- | --- | --- |
| The plugin isn't enabled | `docker plugin ls` | `docker plugin enable tslink` |
| The credential is invalid or expired | `NeedsLogin` in `tailscaled.log`, or `retrying` in the status file | Create a new credential, and recreate the network or replace the cluster credential. |
| The stack isn't allowed the tag | `failed` in the status file, with `outside stack` in the error | Use `tag:<stack>`. See [Tag rules](../reference/credentials.md#tag-rules). |
| The container isn't in the network's stack | `failed` in the status file, with `does not match network stack` in the error | Deploy the container in the same stack as the network: stacks can't share each other's networks. |
| Another container on the host has the same hostname | `tslink.hostname` of the host's containers | Wait for the other container to leave, or give each container its own hostname. |

## The node is offline in the admin console

| Cause | Check | Fix |
| --- | --- | --- |
| The container stopped | `docker ps -a` | Start the container. |
| The node's key expired | The admin console shows the node as expired | Renew the node's key in the admin console, or disable its key expiry. A logged-in node doesn't use its credential, so a new credential won't help. See [Rotation and revocation](../explanation/security-model.md#rotation-and-revocation). |
| The host can't reach the control server | Connection errors in `tailscaled.log` | Permit outbound TCP to port 443. See [Provision a host](provision-a-node.md). |

## The container can't reach the tailnet

| Cause | Check | Fix |
| --- | --- | --- |
| tailscaled hasn't started yet | `tailscaled.log` doesn't contain `Switching ipn state Starting -> Running` yet | Wait, and make the application retry. See [Start-up delay](../explanation/limitations.md#start-up-delay). |
| The node needs to log in | `Switching ipn state NeedsLogin` in `tailscaled.log` | Correct the credential. |
| The veth setup failed | `network is unreachable` in `tailscaled.log` | Find the error in the plugin's log, and restart the container. |
| Your tailnet policy doesn't allow the connection | The grants in the admin console | Change the policy. |

## MagicDNS names don't resolve

| Cause | Check | Fix |
| --- | --- | --- |
| The container doesn't use tailscaled's DNS server | `cat /etc/resolv.conf` in the container | Set `dns: [100.100.100.100]` on the container. See [MagicDNS](../explanation/limitations.md#magicdns). |

## A Service backend gets no traffic

| Cause | Check | Fix |
| --- | --- | --- |
| The container doesn't serve every port of the Service | `ports-uncovered` from `/ready`, or a `ports/<service>` warning in the status file, which names the ports | Serve every port of the Service, or remove the port from the Service in the admin console. The control server sends nothing to a backend that serves only some of them. |
| The control server hasn't fetched the node's Service list | `awaiting-control` from `/ready`, or a `control/<service>` warning in the status file | tslink advertises the Service again, up to 3 times. If the warning stays, restart the container. |
| The control server hasn't approved the backend | `awaiting-approval` from `/ready` | Approve the tag of the node for the Service in the admin console. |

## The node gets the name hostname-1

| Cause | Check | Fix |
| --- | --- | --- |
| An earlier node with the same name is still in the tailnet | The admin console shows an offline node with the name | tslink logs out ephemeral nodes no container uses. Remove any other old node in the admin console. |
| The control server answered a registration after more than 5 minutes, and tailscaled registered a second node | A `node-name` warning in the status file, and the admin console shows an offline node with the name | Remove the offline node in the admin console. See [Slow registrations](../explanation/limitations.md#slow-registrations). |

## Get a new identity for a hostname

A container with the same hostname picks up the earlier container's identity. To start over as a new node, stop
the container and delete its state directory:

```bash
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  rm -rf /data/by-hostname/<hostname>
```

For a container in a stack, the directory is `/data/by-stack/<stack>/<hostname>`.

> [!WARNING]
> This deletes the node's private keys, so the old node can never come back. Remove it in the admin console.

## Remove a network

Stop the network's containers, then remove it:

```bash
docker network rm my-tailnet
```

tslink removes ephemeral nodes from your tailnet. Other nodes stay in the admin console until you remove them there.
