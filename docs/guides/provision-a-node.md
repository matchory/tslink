---
title: Provision a host
description: Get a host ready for production, on its own or as a Swarm node, so tslink runs reliably and fast on it.
category: guide
tags: [provisioning, production, swarm, mtu, performance]
---

# Provision a host

This guide gets a host ready for production use of tslink, on its own or as a Swarm node. Most of it is a checklist
you do once per host, ideally in your configuration management.

## What the host needs

tslink was tested on amd64 cloud VMs with Ubuntu 24.04 (kernel 6.8) and Docker 29.8 in Swarm mode. A host has to
meet these requirements:

| Requirement | How to check |
| --- | --- |
| Docker Engine, with iptables on nftables (the default on current distributions). The plugin image uses `iptables-nft`. | `iptables --version` shows `nf_tables` |
| A kernel with `tun`, `veth` and nftables | `modinfo tun veth nf_tables` |
| A synchronized clock: Tailscale rejects clocks that are far off | `timedatectl show -p NTPSynchronized` shows `yes` |
| Outbound TCP to port 443, for the control server and DERP | Your firewall rules |
| Outbound UDP, for STUN on port 3478 and for direct connections on any port | Your firewall rules |
| Containers can reach a DNS server | See [DNS](../explanation/architecture.md#dns) |

The host needs no inbound firewall rule, no Tailscale package and no access to `pkgs.tailscale.com`. If it runs a
tailscaled of its own, containers don't use it. Without outbound UDP, traffic falls back to DERP relays, which is much
slower: see [throughput](../explanation/architecture.md#throughput).

Containers on hosts that share a network connect to each other directly over it. Containers that reach each other
only over public addresses also need inbound UDP on `TSLINK_WIREGUARD_PORTS` allowed by any firewall in front of the
host.

The drain drop-in and the preflight check below need a clone of the tslink repository, at the tag of the release you
install.

## Set it up

1. **Match Docker's MTU to the network.** If the host network's MTU is below 1500, as it often is on cloud providers'
   private networks and on VPN or VXLAN underlays, set it in `/etc/docker/daemon.json`:

   ```json
   {
     "mtu": 1450
   }
   ```

   Restart Docker to apply it:

   ```bash
   systemctl restart docker
   ```

   Then create each tslink network with the same value: `--opt com.docker.network.driver.mtu=1450`.

2. **Turn on UDP GRO forwarding** on the host's uplink. Without it, a container's tailnet throughput was less than
   half of the host's in our measurements: 1.2 against 2.8 Gbit/s. Make the setting persistent, for example with
   this systemd unit in `/etc/systemd/system/tslink-gro.service`:

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

   Replace `<uplink>` with the interface's name, such as `eth0`, and enable the unit:

   ```bash
   systemctl daemon-reload
   systemctl enable --now tslink-gro.service
   ```

   The kernel and the NIC driver both have to support the setting.

3. **Log in to your registry**, if it needs authentication:

   ```bash
   docker login <registry>
   ```

4. **Install the plugin**, as [Install and upgrade tslink](install-and-upgrade.md) describes.

5. **On a Swarm with one credential for all stacks**, install the cluster credential, as
   [Deploy on Swarm](deploy-on-swarm.md) describes.

6. **On a Swarm, share the certificate directory**, so a Service's certificate is issued once rather than once per
   host. Mount a filesystem every host shares and on which an exclusive create is atomic, such as NFS, and make it
   the source of the plugin's `shared` mount:

   ```bash
   docker plugin disable tslink
   docker plugin set tslink shared.source=/mnt/shared/tslink
   docker plugin enable tslink
   ```

   [`shared.source`](../reference/plugin-settings.md#sharedsource) explains what changes.

   > [!WARNING]
   > The directory holds every Service's private key. Make it owned by root with mode `0700`, and available only to
   > the hosts of the cluster (property `E7` in [SECURITY.md](../../SECURITY.md)).

7. **Install the drain drop-in**, so Service backends are drained before Docker stops. See
   [install the drain drop-in](zero-downtime-updates.md#install-the-drain-drop-in).

8. **Run the preflight check**, which tests the environment properties tslink's security guarantees rely on. See
   [run tslink diag](../reference/diag.md#run-tslink-diag).

9. **Set up monitoring**, so you hear about a node that isn't running. See [Monitor tslink](monitor.md).

## Check that it works

UDP GRO forwarding is on:

```bash
ethtool -k <uplink> | grep rx-udp-gro-forwarding
```

The output is `rx-udp-gro-forwarding: on`. The plugin is enabled:

```bash
docker plugin ls --filter enabled=true
```

And the preflight check from step 8 exits with code `0`.

## Next steps

- [Deploy on Swarm](deploy-on-swarm.md): run stacks, each with its own tags.
- [Update without downtime](zero-downtime-updates.md): upgrade and restart hosts without dropping requests.
- [Security model](../explanation/security-model.md): what tslink protects, and what it relies on.
