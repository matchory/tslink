---
title: Install and upgrade tslink
description: Install the plugin on a host, pick the right image tag, upgrade it safely, and change its settings.
category: guide
tags: [installation, upgrade, plugin, settings]
---

# Install and upgrade tslink

tslink is a Docker managed plugin, so Docker installs, upgrades and configures it. This guide covers all three.

## Before you start

- A Linux host with Docker Engine 19.03 or later, or Docker in a Linux VM such as OrbStack or Docker Desktop.
- Root access on the host.
- Access to `ghcr.io`, or to a registry that mirrors `ghcr.io/matchory/tslink`.

## Install

Docker plugins have no multi-architecture images, so every tag names its architecture. Check yours:

```bash
uname -m
```

`x86_64` means `amd64`; `aarch64` or `arm64` means `arm64`. Then pick a tag:

| Tag | What it is |
| --- | --- |
| `<version>-<arch>`, such as `v1.2.0-amd64` | A release. Use this in production, so every host runs the same version. |
| `latest-<arch>` | The newest release |
| `main-<arch>` | The current `main` branch |

Install the plugin under the alias `tslink`:

```bash
docker plugin install --alias tslink --grant-all-permissions \
  ghcr.io/matchory/tslink:<version>-amd64
```

Docker enables the plugin once it's installed, and the plugin creates its data directory,
`/var/lib/docker-plugins/tailscale`, as it starts. Versions up to v0.3.4 don't enable without that directory: for
them, create it first with `sudo install -d -m 0755 /var/lib/docker-plugins/tailscale`.

> [!NOTE]
> Docker stores the alias with the tag `latest`, so networks and stack files always use the driver name
> `tslink:latest`, whatever version and architecture you installed. Plugin commands accept plain `tslink`, but a
> network's driver name has to be complete.

## Upgrade

An upgrade restarts every tailscaled on the host. Containers keep their nodes, but their tailnet traffic stops for a
few seconds while that happens.

1. If you can, move the containers off the host first. On a Swarm, drain the node:

   ```bash
   docker node update --availability drain <node>
   ```

2. Disable the plugin, upgrade it, and enable it again:

   ```bash
   docker plugin disable -f tslink
   docker plugin upgrade --grant-all-permissions --skip-remote-check tslink ghcr.io/matchory/tslink:<version>-amd64
   docker plugin set tslink shared.source=<shared directory>
   docker plugin enable tslink
   ```

   Keep `--skip-remote-check`. Without it, `docker plugin upgrade` asks whether to switch to a different image, which
   a new version always is. Without a terminal, as in configuration management, CI or a script piped to `ssh`, it
   reads the answer from standard input, treats an empty one as "no", and exits with status 0 having changed
   nothing. In a script piped to it, it even takes the script's next line as its answer, so the commands after it
   never run.

   `docker plugin upgrade` keeps the plugin's settings, but resets the sources of its mounts to their defaults. If
   you set [`shared.source`](../reference/plugin-settings.md#sharedsource), set it again before you enable the
   plugin, as above; otherwise leave that line out. If you forget, the host falls back to certificates of its own and
   gets them from Let's Encrypt again.

3. On a Swarm, make the node active again:

   ```bash
   docker node update --availability active <node>
   ```

> [!NOTE]
> On a test host that wasn't drained, these commands brought its Swarm tasks back within 4 to 11 seconds, and the
> containers kept their nodes. Upgrading a seven-node Swarm from v0.3.0 to v0.3.1, one host at a time and without
> draining, brought each host's tasks back within 1 to 5 seconds. Still, upgrade a host that serves nothing important
> first, and check that its containers keep their nodes, before you move on to production.

Tailscale comes with the plugin, so to upgrade Tailscale, upgrade tslink. See
[binaries](../explanation/architecture.md#binaries).

## Change a setting

Disable the plugin, set the values, and enable it again. `docker plugin set` takes several `KEY=VALUE` pairs at once:

```bash
docker plugin disable tslink
docker plugin set tslink TS_AUTHKEY=tskey-auth-…
docker plugin enable tslink
```

`docker plugin disable` refuses while containers use the plugin. Stop them first, or use `docker plugin disable -f`.
[Plugin settings](../reference/plugin-settings.md) lists every setting.

## Check that it works

```bash
docker plugin ls --filter enabled=true
```

The list contains `tslink:latest`, with `ENABLED` set to `true`.

## Next steps

- [Getting started](../getting-started.md): put a first container on your tailnet.
- [Provision a host](provision-a-node.md): get a host ready for production.
- [Update without downtime](zero-downtime-updates.md): upgrade hosts while Services keep answering.
