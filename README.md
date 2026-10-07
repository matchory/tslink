# Tailscale Container Network Plugin

A Docker network plugin that gives each container its own Tailscale identity. Containers appear as individual nodes
in your tailnet with their own Tailscale IPs.

## How It Works

When you create a Docker network with this plugin and run containers on it:

1. Each container gets its own Tailscale node identity
2. The container appears in your Tailscale admin console as a separate device
3. The container can reach other nodes in your tailnet
4. The container also has internet access via NAT

```text
┌──────────────────────────────────────────────────────────┐
│ Docker Host                                              │
│                                                          │
│  ┌────────────────────────────────────────────────────┐  │
│  │ Container (Tailscale IP: 100.x.y.z)                │  │
│  │                                                    │  │
│  │   App ──► tailscaled ──► Tailnet                   │  │
│  │                                                    │  │
│  │   eth0 ──► veth ──► NAT ──► Internet               │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
│  ┌──────────────────────┐                                │
│  │ Plugin               │                                │
│  │ • Manages tailscaled │                                │
│  │ • Creates veth pairs │                                │
│  │ • Sets up NAT        │                                │
│  └──────────────────────┘                                │
└──────────────────────────────────────────────────────────┘
```

## Installation

### Prerequisites

- Docker 19.03+ or OrbStack
- A Tailscale auth key from <https://login.tailscale.com/admin/settings/keys>

### Install the Plugin

```bash
# Create the data directory (required)
sudo mkdir -p /var/lib/docker-plugins/tailscale
```

Docker plugins have no multi-architecture
images, so each image tag names its architecture. Always install under the
alias `tslink`: networks and stack files then name the driver
`tslink:latest`, whatever the version and architecture, and upgrades keep the
name. Docker stores the alias with the tag `latest`; plugin commands accept
`tslink`, but a network driver must be named in full.

```bash
# amd64 (Intel/AMD, most cloud VMs); use <version>-arm64 on ARM
docker plugin install --alias tslink --grant-all-permissions \
  ghcr.io/matchory/tslink:<version>-amd64

# Other images: latest-<arch> (newest release), main-<arch> (main branch)

# The plugin will be enabled automatically
docker plugin ls
```

To upgrade, keep the alias:

```bash
docker plugin disable -f tslink
docker plugin upgrade --grant-all-permissions tslink ghcr.io/matchory/tslink:<version>-amd64
docker plugin enable tslink
```

On production nodes, see [docs/node-provisioning.md](docs/node-provisioning.md).

## Usage

### Create a Network

```bash
# With auth key in command (ephemeral nodes by default)
docker network create \
  --driver tslink:latest \
  --opt tslink.authkey=tskey-auth-xxxxx \
  my-tailnet

# Or set the auth key globally when installing the plugin
docker plugin set tslink TS_AUTHKEY=tskey-auth-xxxxx
docker network create --driver tslink:latest my-tailnet
```

### Run Containers

```bash
# Run a container - it automatically gets a Tailscale IP
docker run --rm --network my-tailnet alpine sh

# Inside the container:
# - Check your Tailscale IP in the Tailscale admin console
# - Ping other tailnet nodes
# - Access the internet normally
```

### Example: Web Server on Tailnet

```bash
# Run nginx, accessible from your tailnet
docker run -d --name web --network my-tailnet nginx

# From any device on your tailnet, access via the node's Tailscale hostname
curl http://web.your-tailnet.ts.net
```

### Example: Docker Compose

```yaml
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      tslink.authkey: ${TS_AUTHKEY}

services:
  api:
    image: my-api
    networks:
      - tailnet

  worker:
    image: my-worker
    networks:
      - tailnet
```

## Configuration

### Network Options

| Option | Description | Default |
| -------- | ------------- | --------- |
| `tslink.authkey` | Tailscale auth key | Required, unless the plugin has a [cluster credential](#cluster-credential) or `TS_AUTHKEY` |
| `tslink.tags` | ACL tags (comma-separated) for every container on the network; overrides the `tslink.tags` label | None |
| `tslink.loginserver` | URL of a control server other than Tailscale's, such as [headscale](https://github.com/juanfont/headscale); passed to `tailscale up --login-server` | Tailscale's |
| `com.docker.network.driver.mtu` | MTU of the interface tslink adds to the container | 1500 |

A network created by `docker stack deploy` serves only that stack's tasks:
tslink does not start Tailscale for containers from other stacks or outside
any stack. Stack tasks keep their state under `by-stack/<stack>/<hostname>`.
See [docs/credentials.md](docs/credentials.md) for running per-stack
identities on Swarm.

### Plugin Settings

Set with `docker plugin set` while the plugin is disabled.

| Setting | Description | Default |
| --------- | ------------- | --------- |
| `TS_AUTHKEY` | Default auth key for networks without `tslink.authkey` | None |
| `shared.source` | Directory holding HTTPS certificates, in `certs/`; point it at a volume shared between hosts so a Tailscale Service's certificate is issued once for the cluster | The data directory |
| `TS_DEBUG_ACME_DIRECTORY_URL`, `SSL_CERT_FILE` | For tests: another ACME directory, such as Let's Encrypt's staging environment, and a root certificate bundle that trusts it | None |

The plugin image ships the Tailscale it runs, so each tslink release pins
one Tailscale version; upgrade the plugin to upgrade Tailscale. Earlier
releases could download Tailscale or use your own binaries with
`TS_VERSION` and `TS_PATH`; tslink now ignores these settings and logs a
warning.

### Container Labels

Configure per-container Tailscale settings using labels:

| Label | Description | Example |
| ------- | ------------- | --------- |
| `tslink.hostname` | Tailscale hostname. On a node, only one container at a time can use a hostname: a second replica waits without an identity until the first one leaves | `tslink.hostname=my-api` |
| `tslink.tags` | ACL tags (comma-separated) | `tslink.tags=tag:server,tag:prod` |
| `tslink.serve.<port>` | Expose port via Tailscale Serve: `<proto>[:<target>][/<path>][?<option>[&<option>]]`, options below | `tslink.serve.443=https:8080` |
| `tslink.service` | Register as Tailscale Service backend | `tslink.service=svc:my-api` |
| `tslink.direct` | Also serve the endpoints on the container's own name. Defaults to `true` without `tslink.service`, `false` with it | `tslink.direct=true` |

```bash
# Example: Container with custom hostname and tags
docker run -d --network my-tailnet \
  --label tslink.hostname=my-api \
  --label tslink.tags=tag:server \
  nginx

# Example: Expose HTTPS on port 443, forwarding to container port 8080
docker run -d --network my-tailnet \
  --label tslink.hostname=web \
  --label tslink.serve.443=https:8080 \
  my-web-app
```

**Caller addresses.** In `http` and `https` mode, Tailscale Serve passes the
caller's tailnet address in `X-Forwarded-For`. In `tcp` and
`tls-terminated-tcp` mode, the target sees connections from `127.0.0.1`;
`?proxy-protocol=1` or `=2` (e.g. `tslink.serve.5432=tcp:5432?proxy-protocol=2`)
makes Tailscale send a PROXY protocol header with the caller's address, which
the application must accept.

**App capabilities.** In `http` and `https` mode,
`?accept-app-caps=<cap>[,<cap>]` (e.g.
`tslink.serve.80=http:8080?accept-app-caps=example.com/cap/api`) forwards the
caller's grants of these [app capabilities](https://tailscale.com/kb/1537/grants-app-capabilities)
as JSON in the `Tailscale-App-Capabilities` header. Tagged callers get no
`Tailscale-User-*` headers, so this is how a backend authorizes another
workload. Serve replaces the header if a caller sends its own.

### Setting Default Auth Key

Instead of passing the auth key with each network, set it as a plugin environment variable:

```bash
# Set default auth key
docker plugin disable tslink
docker plugin set tslink TS_AUTHKEY=tskey-auth-xxxxx
docker plugin enable tslink

# Now create networks without specifying the auth key
docker network create --driver tslink:latest my-tailnet
```

### Cluster Credential

On Swarm, give the plugin one OAuth client for the whole cluster instead of a secret per stack: put its secret
(`tskey-client-…`, nothing appended) in `/var/lib/docker-plugins/tailscale/oauth-client.secret` on every node. Stack
networks then carry only their tags:

```yaml
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      tslink.tags: tag:billing
```

- The file is read whenever a node registers, so replacing it rotates the credential without recreating networks.
- Nodes are ephemeral and pre-approved.
- A stack may only use `tag:<stack>` and `tag:<stack>-*`, where `<stack>` is its stack name. The OAuth client must own
  these tags in the tailnet policy, e.g. through a tag of its own: `"tag:billing": ["tag:tslink"]`.
- A network's own `tslink.authkey` takes precedence over the file, and the file over `TS_AUTHKEY`.

See [docs/credentials.md](docs/credentials.md) for the reasoning.

## Auth Key Types

| Key Type | Behavior |
| ---------- | ---------- |
| **Ephemeral key** | Nodes are automatically removed when the container stops |
| **Reusable key** | Nodes persist in your tailnet after container stops |
| **Pre-approved key** | Nodes don't require manual approval |
| **OAuth client secret** | Nodes are ephemeral unless `?ephemeral=false` is appended; tslink logs them out and deletes their state when the container stops. Requires `tslink.tags` |

For most use cases, use an ephemeral, reusable, pre-approved key.

Keys from headscale do not say whether they are ephemeral, so with `tslink.loginserver` tslink keeps an ephemeral node's
state and does not log it out when the container stops; headscale removes the node after its
`node.ephemeral.inactivity_timeout`.

## Running on Swarm

Measured on three- and five-node swarms; see
[docs/testing/swarm-cluster-test-2026-10-07.md](docs/testing/swarm-cluster-test-2026-10-07.md)
and [docs/testing/swarm-cluster-followup-2026-10-07.md](docs/testing/swarm-cluster-followup-2026-10-07.md).

- **Zero-downtime updates of a Tailscale Service.** tslink drains a task's
  Service backend when Docker sends it the stop signal, and callers move to
  other replicas. They need a moment to learn of it, so the application
  should keep serving for a few seconds after SIGTERM; one that exits at once
  costs its callers a second or so of errors.
- **Restarts and reboots:** Docker stops reporting events once it shuts
  down, so tslink cannot drain the Service backends of a node whose Docker
  stops; callers pinned to them failed for 7 to 16 seconds. Install
  [deploy/systemd](deploy/systemd): `tslink-drain` as
  `/usr/local/sbin/tslink-drain` and `tslink-drain.conf` as
  `/etc/systemd/system/docker.service.d/tslink-drain.conf`, then
  `systemctl daemon-reload`. Before dockerd gets SIGTERM, it drains every
  backend on the node and waits 3 seconds (`TSLINK_DRAIN_WAIT`); with it,
  `systemctl restart docker` and `systemctl reboot` cost callers at most one
  failed request. It does not cover a crash of dockerd. For planned
  maintenance, draining the node in Swarm
  (`docker node update --availability drain`) still moves its tasks first.
- **Upgrading the plugin** restarts every tailscaled on the node, which keeps
  its identity; the node's tasks are off the tailnet for about three seconds.
- **Throughput:** enable UDP GRO forwarding on the node's uplink
  (`ethtool -K <iface> rx-udp-gro-forwarding on rx-gro-list off`, persisted
  by your provisioning). Without it, a container's tailnet throughput is
  about 40% of the host's.
- **Self-healing:** tslink retries a Tailscale start that failed, logs a node
  in again when its device was deleted or expired, and cleans up after
  containers that stopped without telling it, as on power loss: it logs
  their ephemeral nodes out, so a container with a fixed `tslink.hostname`
  gets its name back instead of `<hostname>-1`.
- **HTTPS certificates:** Tailscale fetches a Let's Encrypt certificate for
  every name it serves HTTP on, even plain HTTP (a Tailscale bug since 1.100,
  [tailscale/tailscale#21693](https://github.com/tailscale/tailscale/issues/21693)).
  Let's Encrypt allows 50 new certificates a week per tailnet and 5 per name.
  - Serve through Tailscale Services: replicas share their Service's
    certificate, and tslink issues it once, so only a Service's first
    deployment counts. Set the plugin's `shared.source` to a volume shared
    between the nodes (GlusterFS, NFS); otherwise each node issues it once.
  - Names of their own (`tslink.direct`) get a new certificate for every new
    task, so avoid them for replicated services.
  - Introduce new Services at a pace that stays well below 50 a week, together
    with anything else in the tailnet that uses Tailscale's HTTPS certificates.
- **Monitoring:** a container whose Tailscale is not running looks healthy to
  Docker. tslink writes each endpoint's state to
  `/var/lib/docker-plugins/tailscale/status/<endpoint>.json` (`running`,
  `retrying` with the error, or `failed`), and `tslink diag` lists them.

## Cleanup

```bash
# Remove network (stops all containers using it)
docker network rm my-tailnet

# Ephemeral nodes are automatically removed from Tailscale
# Non-ephemeral nodes remain in your admin console and need manual removal
```

## Troubleshooting

### View Debug Logs

Each container's Tailscale daemon logs to `tailscaled.log` in its state directory, rotated at 10 MB:

```bash
# Show the end of every endpoint's log
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  sh -c 'for d in /data/by-hostname/*/ /data/by-stack/*/*/; do
    echo "=== $d ==="; tail -20 "$d/tailscaled.log" 2>/dev/null
  done'

# Check plugin logs (Linux)
journalctl -u docker -f | grep -i tailscale

# Check plugin logs (macOS/OrbStack)
docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
# Then: journalctl -u docker -f
```

### Container Won't Start

1. Check debug logs (above) for errors
2. Verify auth key is valid and not expired
3. Ensure the plugin is enabled: `docker plugin ls`

### Node Appears "Offline" in Tailscale Console

- Auth key may be expired - create a new one
- Container may have stopped - check `docker ps`
- Network issues - check debug logs for connection errors

### Internet Works but Can't Reach Tailnet

Check debug logs for:

- `Switching ipn state Starting -> Running` = connected successfully
- `Switching ipn state NeedsLogin` = auth key issue
- `network is unreachable` = veth setup failed

### Identity Reuse

Containers with the same hostname reuse the same Tailscale identity. If you need a fresh identity:

```bash
# Clear state for a specific hostname
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  rm -rf /data/by-hostname/<hostname>
```

## Known Limitations

- **macOS/Windows**: Only works with Docker in a Linux VM (OrbStack, Docker Desktop)
- **MagicDNS in container**: Containers can reach tailnet by IP; for MagicDNS names, set `dns: [100.100.100.100]`
- **Tailscale starts after the application**: Docker gives the plugin no way to identify a container while it is
  starting, so tslink brings Tailscale up once the container has started, which usually takes a few seconds. Until
  then, connections to tailnet addresses fail immediately with "host unreachable"; they never fall back to the host's
  own Tailscale. Applications that need the tailnet at startup should retry.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). The end-to-end test, `test/integration/run.sh`, runs
containers against a local headscale and needs no Tailscale account; CI runs it on every pull request.
Report security issues as described in [SECURITY.md](SECURITY.md); changes are listed in
[CHANGELOG.md](CHANGELOG.md).

## License

MIT, see [LICENSE](LICENSE). tslink is a fork of
[aaomidi/tslink](https://github.com/aaomidi/tslink) by Amir Omidi.
