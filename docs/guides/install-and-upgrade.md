# Install and upgrade tslink

## Goal

Install the tslink plugin on a host, upgrade it, and change its settings.

## Prerequisites

- A Linux host with Docker Engine, or Docker in a Linux VM such as OrbStack or Docker Desktop.
- Root access on the host.
- Access to `ghcr.io`, or to a registry that mirrors `ghcr.io/matchory/tslink`.

For a production host, first do the steps in [Provision a host](provision-a-node.md).

## Steps

1. Create the data directory:

   ```bash
   sudo install -d -m 0755 /var/lib/docker-plugins/tailscale
   ```

   The plugin does not enable without this directory.

2. Find the architecture of the host:

   ```bash
   uname -m
   ```

   If the output is `x86_64`, use `amd64`. If the output is `aarch64`, use `arm64`.

3. Select an image tag. Docker plugins have no multi-architecture images, so each tag contains the architecture:

   | Tag | Content |
   | --- | --- |
   | `<version>-<arch>`, for example `v1.2.0-amd64` | A release. Use this tag in production. |
   | `latest-<arch>` | The newest release |
   | `main-<arch>` | The `main` branch |

4. Install the plugin with the alias `tslink`:

   ```bash
   docker plugin install --alias tslink --grant-all-permissions \
     ghcr.io/matchory/tslink:<version>-amd64
   ```

   On an ARM host, use `<version>-arm64`. Docker enables the plugin when the installation is complete.

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
   docker plugin upgrade --grant-all-permissions tslink ghcr.io/matchory/tslink:<version>-amd64
   docker plugin enable tslink
   ```

   On an ARM host, use `<version>-arm64`. Containers keep their nodes. Their tailnet traffic stops for some seconds
   while each tailscaled restarts.

3. On a Swarm, make the node active again:

   ```bash
   docker node update --availability active <node>
   ```

Each tslink release contains one Tailscale version. To upgrade Tailscale, upgrade the plugin.

### Change a setting

1. Disable the plugin, set the value and enable the plugin:

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
