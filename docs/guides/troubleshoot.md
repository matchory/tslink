# Troubleshoot

Find the symptom below, then do the checks in the sequence of the table. Before you start, collect the logs.

## Collect information

1. Show the status of each endpoint:

   ```bash
   jq -r '[.hostname, .state, (.error // "")] | @tsv' /var/lib/docker-plugins/tailscale/status/*.json
   ```

2. Show the end of the tailscaled log of each node:

   ```bash
   docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
     sh -c 'for d in /data/by-hostname/*/ /data/by-stack/*/*/; do
       echo "=== $d ==="; tail -20 "$d/tailscaled.log" 2>/dev/null
     done'
   ```

3. Show the plugin log. On Linux:

   ```bash
   journalctl -u docker -f | grep -i tailscale
   ```

   On macOS with OrbStack, open a shell in the Linux VM first, then run `journalctl -u docker -f`:

   ```bash
   docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
   ```

   The plugin also writes `plugin.log` in the data directory.

4. Run `tslink diag`. See [tslink diag](../reference/diag.md).

## The plugin does not enable

| Cause | Check | Fix |
| --- | --- | --- |
| The data directory does not exist | `ls -d /var/lib/docker-plugins/tailscale` | Create it: `sudo install -d -m 0755 /var/lib/docker-plugins/tailscale` |

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
| The plugin is not enabled | `docker plugin ls` | `docker plugin enable tslink` |
| The credential is not valid, or it expired | `NeedsLogin` in `tailscaled.log`, or `retrying` in the status file | Create a new credential, and recreate the network or replace the cluster credential. |
| A tag is not permitted for the stack | `failed` in the status file, with `outside stack` in the error | Use `tag:<stack>`. See [Tag rules](../reference/credentials.md#tag-rules). |
| The container is not in the stack of the network | `failed` in the status file, with `does not match network stack` in the error | Deploy the container in the same stack as the network. |
| Another container on the host uses the hostname | `tslink.hostname` of the containers on the host | Wait until the other container leaves, or give each container its own hostname. |

## The node is offline in the admin console

| Cause | Check | Fix |
| --- | --- | --- |
| The container stopped | `docker ps -a` | Start the container. |
| The credential expired | `NeedsLogin` in `tailscaled.log` | Create a new credential. |
| The host cannot reach the control server | Connection errors in `tailscaled.log` | Permit outbound TCP to port 443. See [Provision a host](provision-a-node.md). |

## The container cannot reach the tailnet

| Cause | Check | Fix |
| --- | --- | --- |
| tailscaled has not started yet | `Switching ipn state Starting -> Running` is not in `tailscaled.log` | Wait. Make the application try again. See [Start-up delay](../explanation/limitations.md#start-up-delay). |
| The node must log in | `Switching ipn state NeedsLogin` in `tailscaled.log` | Correct the credential. |
| The veth setup failed | `network is unreachable` in `tailscaled.log` | Read the plugin log for the error, and restart the container. |
| The tailnet policy does not permit the connection | The ACLs in the admin console | Change the policy. |

## MagicDNS names do not resolve

| Cause | Check | Fix |
| --- | --- | --- |
| The container does not use the DNS server of tailscaled | `cat /etc/resolv.conf` in the container | Set `dns: [100.100.100.100]` on the container. See [MagicDNS](../explanation/limitations.md#magicdns). |

## The node gets the name hostname-1

| Cause | Check | Fix |
| --- | --- | --- |
| An earlier node with the same name is still in the tailnet | The admin console shows an offline node with the name | tslink logs out ephemeral nodes that no container uses. For other nodes, remove the old node in the admin console. |

## Get a new identity for a hostname

A container keeps the identity of the earlier container with the same hostname. To get a new identity, stop the
container and delete the state directory:

```bash
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  rm -rf /data/by-hostname/<hostname>
```

For a container in a stack, the directory is `/data/by-stack/<stack>/<hostname>`.

> [!WARNING]
> This deletes the private keys of the node. The node cannot come back. Remove the old node in the admin console.

## Remove a network

Stop the containers on the network, then remove the network:

```bash
docker network rm my-tailnet
```

tslink removes ephemeral nodes from the tailnet. Other nodes stay in the admin console. Remove them there.
