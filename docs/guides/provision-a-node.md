# Provision a host

## Goal

Prepare a production host, alone or as a Swarm node, so that tslink runs reliably on it.

## Prerequisites

The host must meet these requirements. tslink was tested on amd64 cloud VMs with Ubuntu 24.04 (kernel 6.8) and
Docker 29.8 in Swarm mode.

| Requirement | Check |
| --- | --- |
| Docker Engine, with iptables on nftables (the default on current distributions). The plugin image uses `iptables-nft`. | `iptables --version` shows `nf_tables` |
| The kernel has `tun`, `veth` and nftables | `modinfo tun veth nf_tables` |
| The clock is synchronized. Tailscale rejects clocks with a large error. | `timedatectl show -p NTPSynchronized` shows `yes` |
| Outbound TCP to port 443, for the control server and DERP | Your firewall rules |
| Outbound UDP, for STUN on port 3478 and for direct connections on all ports | Your firewall rules |
| Containers can reach a DNS server | See [DNS](../explanation/architecture.md#dns) |

The host needs no inbound firewall rule, no Tailscale package, and no access to `pkgs.tailscale.com`. If the host
runs its own tailscaled, containers do not use it. Without outbound UDP, traffic goes through DERP relays and is
slower. See [Throughput](../explanation/architecture.md#throughput).

Steps 7 and 8 need a clone of the tslink repository, at the tag of the release that you install.

## Steps

1. If the MTU of the host network is below 1500, set the MTU for Docker. Put the value in
   `/etc/docker/daemon.json`:

   ```json
   {
     "mtu": 1450
   }
   ```

   Restart Docker to apply it:

   ```bash
   systemctl restart docker
   ```

   Then set the same value on each tslink network with `--opt com.docker.network.driver.mtu=1450`. Private networks
   of cloud providers and VPN or VXLAN underlays often have a smaller MTU.

2. Enable UDP GRO forwarding on the uplink of the host. Make the setting persistent, for example with this systemd
   unit in `/etc/systemd/system/tslink-gro.service`:

   ```ini
   [Unit]
   Description=Enable UDP GRO forwarding for tslink
   After=network-online.target
   Wants=network-online.target

   [Service]
   Type=oneshot
   ExecStart=/usr/sbin/ethtool -K <uplink> rx-udp-gro-forwarding on rx-gro-list off

   [Install]
   WantedBy=multi-user.target
   ```

   Replace `<uplink>` with the name of the interface, for example `eth0`. Then enable the unit:

   ```bash
   systemctl daemon-reload
   systemctl enable --now tslink-gro.service
   ```

   The kernel and the NIC driver must support the setting.

3. If your registry requires authentication, log in:

   ```bash
   docker login <registry>
   ```

4. Install the plugin. See [Install and upgrade tslink](install-and-upgrade.md).

5. If the host is in a Swarm, and you use one credential for all stacks, install the cluster credential. See
   [Deploy on Swarm](deploy-on-swarm.md).

6. If the host is in a Swarm, share the certificate directory. Mount a volume that all hosts share, for example
   GlusterFS or NFS. Then set it as the source of the plugin's `shared` mount:

   ```bash
   docker plugin disable tslink
   docker plugin set tslink shared.source=/mnt/shared/tslink
   docker plugin enable tslink
   ```

   For the effect, see [shared.source](../reference/plugin-settings.md#sharedsource).

   > [!WARNING]
   > The directory holds the private key of each Service. Make it owned by root, with mode `0700`, and make it
   > available only to the hosts of the cluster (property `E7` in [SECURITY.md](../../SECURITY.md)).

7. Install the drain drop-in for Docker. See
   [Install the drain drop-in](zero-downtime-updates.md#install-the-drain-drop-in).

8. Check the environment properties with the preflight check. See
   [Run tslink diag](../reference/diag.md#run-tslink-diag).

9. Set up monitoring. See [Monitor tslink](monitor.md).

## Verify

1. Make sure that UDP GRO forwarding is on:

   ```bash
   ethtool -k <uplink> | grep rx-udp-gro-forwarding
   ```

   The output is `rx-udp-gro-forwarding: on`.

2. Make sure that the plugin is enabled:

   ```bash
   docker plugin ls --filter enabled=true
   ```

3. Make sure that the preflight check of step 8 exits with code `0`.

## Next steps

- [Deploy on Swarm](deploy-on-swarm.md)
- [Update without downtime](zero-downtime-updates.md)
- [Security model](../explanation/security-model.md)
