# Node provisioning

What a Docker host needs before it runs tslink containers, in the order a
configuration management tool would apply it. Most of it applies to plain
Docker hosts as well as Swarm nodes. The requirements were measured on
amd64 cloud VMs with Ubuntu 24.04 (kernel 6.8) and Docker 29.8 in Swarm mode;
see the [cluster test](testing/swarm-cluster-test-2026-10-07.md) and its
[follow-up](testing/swarm-cluster-followup-2026-10-07.md). Other distributions should
work if they meet the requirements below, but have not been tested.

## Host

- **Docker Engine**, optionally in Swarm mode, using nftables-backed iptables
  (the default on current distributions). tslink's image uses `iptables-nft`
  to match.
- **Kernel**: `tun`, `veth` and nftables.
- **MTU**: if the hosts' network has an MTU below 1500 (common for private
  networks of cloud providers and for VPN or VXLAN underlays), set the same
  value as `"mtu"` in `/etc/docker/daemon.json` and as
  `com.docker.network.driver.mtu` on every tslink network.
- **DNS**: a resolver that containers can reach. Each task's tailscaled
  resolves names, and forwards the container's queries for names outside the
  tailnet, through the host's resolvers from `/etc/resolv.conf`, without those
  on the host's loopback. When that leaves none, it uses systemd-resolved's
  upstreams from `/run/systemd/resolve/resolv.conf`, as Docker does. With
  neither, it falls back to Docker's embedded resolver, or, for a container
  whose DNS server is `100.100.100.100`, to Google's public resolvers (Docker's
  default), and logs a warning. tailscaled reads them when it starts.
- **Clock**: synchronised (NTP); Tailscale rejects badly skewed clocks.
- **Firewall**: no inbound rule. Each task's tailscaled needs outbound TCP 443
  (control plane, DERP) and outbound UDP (STUN on 3478, direct connections on
  any port). Without UDP, traffic falls back to DERP at about 25 Mbit/s.
- **No host tailscaled required.** If the node runs one, tasks still never use
  it: tslink blocks tailnet ranges outside the task's own tailscaled.

## Throughput

Enable UDP GRO forwarding on the uplink, persistently (for example from a
systemd unit or a networkd-dispatcher hook):

```bash
ethtool -K <uplink> rx-udp-gro-forwarding on rx-gro-list off
```

Without it, a container's tailnet throughput was about 40% of the host's in
the test (1.2 instead of 2.8 Gbit/s on a 4-vCPU VM). The setting needs a
kernel and NIC driver that support it; check with `ethtool -k <uplink>`.

## Plugin

1. **Data directory**, before the plugin is installed, and never removed while
   the plugin is installed (the plugin will not enable without it):

   ```bash
   install -d -m 0755 /var/lib/docker-plugins/tailscale
   ```

   It holds each task's Tailscale state and logs: per endpoint up to 20 MB of
   rotated `tailscaled.log`, plus up to 100 MB of `plugin.log`.

2. **Registry access**: the host pulls the plugin image when it is installed
   or upgraded. If your registry requires authentication (a private mirror,
   for instance), log in first (`docker login <registry>`).

3. **Install a pinned version under the alias `tslink`.** Stack files name the
   driver `tslink:latest`; the image tag carries version and architecture:

   ```bash
   docker plugin install --alias tslink --grant-all-permissions \
     ghcr.io/matchory/tslink:<version>-amd64
   ```

   The plugin gets host networking, `CAP_NET_ADMIN`, `CAP_SYS_ADMIN`,
   `/dev/net/tun`, the Docker socket and `/run`. It ships its own Tailscale, so
   the host needs no Tailscale package and no access to pkgs.tailscale.com.
   To use your own registry, mirror the image and install it from there under
   the same alias.

4. **Cluster credential** (optional, see
   [Cluster Credential](../README.md#cluster-credential)): the OAuth client
   secret, the same on every node, readable by root only:

   ```bash
   install -m 0600 /dev/stdin /var/lib/docker-plugins/tailscale/oauth-client.secret <<<"$SECRET"
   ```

   Replacing the file rotates it; no restart is needed.

5. **Shared certificate directory** (optional): mount a volume shared between
   the nodes, such as GlusterFS or NFS, and point the plugin's `shared` mount
   at it, so a Tailscale Service's certificate is issued once for the cluster
   rather than once per node. `docker plugin set` needs the plugin disabled:

   ```bash
   docker plugin disable tslink
   docker plugin set tslink shared.source=/mnt/shared/tslink
   docker plugin enable tslink
   ```

   tslink keeps the certificates and their ACME account key in `certs/` there.
   Restrict the volume to the nodes: it holds every Service's private key.

6. **Upgrades**: move the host's containers off first if you can (on Swarm,
   drain the node). Then:

   ```bash
   docker plugin disable -f tslink
   docker plugin upgrade --grant-all-permissions tslink ghcr.io/matchory/tslink:<version>-amd64
   docker plugin enable tslink
   ```

   Running tasks keep their identities; their tailnet traffic stops for a few
   seconds (3.5 s measured when reinstalling). This exact command sequence is
   still to be tested on the cluster.

## Drain before Docker stops

Install the drop-in from [deploy/systemd](../deploy/systemd), so restarts of
Docker and reboots drain the node's Tailscale Service backends first:

```bash
install -m 0755 deploy/systemd/tslink-drain /usr/local/sbin/tslink-drain
install -d /etc/systemd/system/docker.service.d
install -m 0644 deploy/systemd/tslink-drain.conf /etc/systemd/system/docker.service.d/
systemctl daemon-reload
```

It needs `sh` and `timeout` (coreutils), and finds the plugin's `tailscale`
CLI under `/var/lib/docker/plugins`. With a different Docker data root, set
`TSLINK_TAILSCALE` in the drop-in (`Environment=`). It takes effect at the next
stop of Docker; no restart is needed to install it.

## Monitoring

A container whose Tailscale is not running looks healthy to Docker. tslink writes
each endpoint's state to `/var/lib/docker-plugins/tailscale/status/<endpoint>.json`:
`running`, `retrying` with the error, or `failed`. Export them, for example to
your monitoring system, and alert when a state is not `running` for more than
a few minutes. For example, for the Prometheus node exporter's textfile
collector:

```bash
out=/var/lib/node_exporter/textfile/tslink.prom
for f in /var/lib/docker-plugins/tailscale/status/*.json; do
  [ -e "$f" ] || continue
  jq -r '"tslink_endpoint_running{hostname=\"\(.hostname)\",stack=\"\(.stack)\"} " +
    (if .state == "running" then "1" else "0" end)' "$f"
done >"$out.tmp" && mv "$out.tmp" "$out"
```

On the tailnet side, watch the number of offline ephemeral devices per tag:
power loss and dockerd crashes leave them behind for about an hour.

## Operations

- On Swarm, drain a node (`docker node update --availability drain`) before
  planned maintenance; the drop-in covers restarts that happen without it.
- `tslink diag` (or `scripts/tslink-diag.sh`) lists endpoints and containers
  whose Tailscale is not running, and why.
- Before rolling out a new tslink or Tailscale version, run
  `test/cluster/regress.sh` against it.
