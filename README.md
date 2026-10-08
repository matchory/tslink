# tslink

tslink is a Docker network plugin that makes each container a node in your
[Tailscale](https://tailscale.com) tailnet, with its own identity, name and address.

## Features

- **One node for each container.** Each container has its own identity and ACL tags, separate from the host.
- **Tailscale Serve and Services.** Labels serve a container over HTTPS, or make it a backend of a Tailscale Service.
- **Docker Swarm.** Each stack gets its own tags, from one cluster credential or from an OAuth client for each stack.
- **Updates without downtime.** tslink drains a Service backend before its container stops.
- **Self-healing.** tslink restarts tailscaled, logs nodes in again and removes the nodes of lost containers.

```text
┌────────────────────────────────────────────────────────────┐
│ Host                                                       │
│  ┌──────────────────────────────────────────┐              │
│  │ Container (tailnet address 100.x.y.z)    │              │
│  │ App ─► tailscale0 ─► tailscaled ─► veth ─┼─► NAT ───────┼─► Tailnet
│  │ App ─► eth0 ─────────────────────────────┼─► Docker ────┼─► Internet
│  └──────────────────────────────────────────┘              │
└────────────────────────────────────────────────────────────┘
```

The container reaches the tailnet only as its own node, never through the Tailscale of the host.

## Requirements

- Linux with Docker Engine, or Docker in a Linux VM, for example OrbStack or Docker Desktop.
- A Tailscale tailnet.
- An auth key or an OAuth client secret.

## Quick start

Replace `amd64` with `arm64` on an ARM host, and `tskey-auth-…` with your auth key:

```bash
sudo install -d -m 0755 /var/lib/docker-plugins/tailscale
docker plugin install --alias tslink --grant-all-permissions ghcr.io/matchory/tslink:latest-amd64
docker network create --driver tslink:latest --opt tslink.authkey=tskey-auth-… my-tailnet
docker run -d --network my-tailnet --label tslink.hostname=web nginx:alpine
```

The container is now the node `web` in your tailnet. For a complete example with HTTPS, see
[Getting started](docs/getting-started.md).

## Documentation

| Section | Content |
| --- | --- |
| [Getting started](docs/getting-started.md) | A tutorial for the first container |
| [Guides](docs/README.md#guides) | Install, provision, deploy on Swarm, expose services, update, monitor and troubleshoot |
| [Reference](docs/README.md#reference) | Network options, container labels, plugin settings, credentials, files and `tslink diag` |
| [Explanation](docs/README.md#explanation) | Architecture, security model, HTTPS certificates and limitations |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report vulnerabilities as described in [SECURITY.md](SECURITY.md). The
changes are in [CHANGELOG.md](CHANGELOG.md).

## License

MIT, see [LICENSE](LICENSE). tslink is a fork of [aaomidi/tslink](https://github.com/aaomidi/tslink) by Amir Omidi.

The plugin image contains Tailscale (BSD 3-Clause) and Go modules under their own licenses. Their notices are in the
image under `/usr/share/licenses`.
