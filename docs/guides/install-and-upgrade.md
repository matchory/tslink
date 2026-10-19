# Install and upgrade tslink

## Goal

Install the tslink plugin on a host, upgrade it, and change its settings.

## Prerequisites

- A Linux host with Docker Engine 19.03 or later, or Docker in a Linux VM such as OrbStack or Docker Desktop.
- Root access on the host.
- Access to `ghcr.io`, or to a registry that mirrors `ghcr.io/matchory/tslink`.

## Steps

1. Find the architecture of the host:

   ```bash
   uname -m
   ```

   If the output is `x86_64`, use `amd64`. If the output is `aarch64` or `arm64`, use `arm64`.

2. Select an image tag. Docker plugins have no multi-architecture images, so each tag contains the architecture:

   | Tag | Content |
   | --- | --- |
   | `<version>-<arch>`, for example `v1.2.0-amd64` | A release. Use this tag in production. |
   | `latest-<arch>` | The newest release |
   | `main-<arch>` | The `main` branch |

3. Install the plugin with the alias `tslink`:

   ```bash
   docker plugin install --alias tslink --grant-all-permissions \
     ghcr.io/matchory/tslink:<version>-amd64
   ```

   On an ARM host, use `<version>-arm64`. Docker enables the plugin when the installation is complete. The plugin
   creates its data directory, `/var/lib/docker-plugins/tailscale`, when it starts. Versions up to v0.3.4 do not
   enable without it: for them, create it first with `sudo install -d -m 0755 /var/lib/docker-plugins/tailscale`.

   > [!NOTE]
   > Docker stores the alias with the tag `latest`. Thus networks and stack files use the driver name
   > `tslink:latest` for all versions and architectures. Plugin commands accept `tslink`, but the driver name of a
   > network must be complete.

### Upgrade

1. If possible, move the containers off the host. On a Swarm, drain the node:

   ```bash
   docker node update --availability drain <node>
   ```

2. Disable the plugin, upgrade it and enable it again:

   ```bash
   docker plugin disable -f tslink
   docker plugin upgrade --grant-all-permissions --skip-remote-check tslink ghcr.io/matchory/tslink:<version>-amd64
   docker plugin set tslink shared.source=<shared directory>
   docker plugin enable tslink
   ```

   Without `--skip-remote-check`, `docker plugin upgrade` asks whether to upgrade to an image other than the
   installed one, which a new version always is. Without a terminal, as in configuration management, CI or a script
   piped to `ssh`, it reads the answer from standard input, takes an empty one as "no", and exits with status 0
   having changed nothing. In a script piped to it, it reads the script's next line as its answer, so the commands
   after it do not run.

   `docker plugin upgrade` keeps the plugin's settings but resets the source of its mounts to their defaults. If you set
   [`shared.source`](../reference/plugin-settings.md#sharedsource), set it again before you enable the plugin, as
   above. Otherwise leave out that command. Without it, the host uses certificates of its own and gets them again from
   Let's Encrypt.

   On an ARM host, use `<version>-arm64`. Containers keep their nodes. Their tailnet traffic stops for some seconds
   while each tailscaled restarts.

   > [!NOTE]
   > On a test host that was not drained, these commands brought its Swarm tasks back within 4 to 11 seconds,
   > and the containers kept their nodes. An upgrade from v0.3.0 to v0.3.1 on a seven-node Swarm, one host at a
   > time and without draining, brought each host's tasks back within 1 to 5 seconds, and the containers kept their
   > nodes. Before you upgrade production hosts, upgrade a host that serves nothing important, and check that its
   > containers keep their nodes.

3. On a Swarm, make the node active again:

   ```bash
   docker node update --availability active <node>
   ```

To upgrade Tailscale, upgrade tslink. See [Binaries](../explanation/architecture.md#binaries).

### Change a setting

Disable the plugin, set the values and enable the plugin. `docker plugin set` accepts more than one `KEY=VALUE`:

```bash
docker plugin disable tslink
docker plugin set tslink TS_AUTHKEY=tskey-auth-…
docker plugin enable tslink
```

If containers use the plugin, `docker plugin disable` fails. Stop the containers first, or use
`docker plugin disable -f`.

For all settings, see [Plugin settings](../reference/plugin-settings.md).

## Verify

```bash
docker plugin ls --filter enabled=true
```

The output contains `tslink:latest`, with `ENABLED` set to `true`.

## Next steps

- [Getting started](../getting-started.md): connect a first container to the tailnet.
- [Provision a host](provision-a-node.md): prepare a production host.
- [Update without downtime](zero-downtime-updates.md): upgrade a host while Services keep working.
