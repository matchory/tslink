---
title: tslink diag
description: Show every endpoint's state on a host, and check the environment properties tslink's security guarantees rely on.
category: reference
tags: [diag, preflight, diagnostics, security]
---

# tslink diag

`tslink diag` shows the state of every endpoint on a host. `tslink diag --preflight` checks the environment
properties that the guarantees in [SECURITY.md](../../SECURITY.md) rely on, so you know they hold on a host before
you depend on them.

## Commands

| Command | What it does |
| --- | --- |
| `tslink diag` | Shows every endpoint with its state directory, tailnet address, hostname, backend state and status file |
| `tslink diag --preflight` | Checks the environment properties on the host where it runs |
| `tslink diag --preflight --shared-dir <dir>` | Also checks the owner and mode of the shared certificate directory (E7), mounted at `<dir>` |
| `tslink version` | Shows the versions of tslink and its bundled Tailscale |
| `tslink help` | Shows the usage |

Any other argument exits with status 2 and prints the usage. Without arguments, the binary starts the plugin, so
leave that to Docker.

To see which version a host runs, run the binary from the plugin's root filesystem, as root:

```bash
/var/lib/docker/plugins/$(docker plugin inspect -f '{{.Id}}' tslink)/rootfs/tslink version
```

## Exit codes of the preflight check

| Exit code | Meaning |
| --- | --- |
| `0` | No property is violated. A result of "unknown" doesn't change the exit code. |
| `1` | A property is violated, or a check couldn't run. |

The preflight check looks only at the host it runs on, so run it on every host, every Swarm node included.

## Run tslink diag

`ghcr.io/matchory/tslink` holds Docker plugins, not images `docker run` can pull, so build an image from a clone of
the repository:

```bash
docker build -t tslink-diag -f docker/Dockerfile .
```

Then run `diag`:

```bash
docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
  --entrypoint /tslink tslink-diag diag
```

Or run the preflight check:

```bash
docker run --rm --network host --cap-add NET_ADMIN \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/docker-plugins/tailscale:/data \
  --entrypoint /tslink tslink-diag diag --preflight
```

To check the owner and mode of the shared certificate directory (E7) too, add `-v <shared directory>:/shared:ro` and
`--shared-dir /shared`. E2 doesn't need `--shared-dir`: it checks the host path in each tslink plugin's
`shared.source` setting.

For several hosts, build the image once and copy it to each with `docker save` and `docker load`.

### Without an image

On a host with the plugin installed, the plugin's root filesystem has the same binary. Run it from a root shell on
the host, for example after `sudo -i`:

```bash
rootfs="$(docker info -f '{{.DockerRootDir}}')/plugins/$(docker plugin inspect -f '{{.Id}}' tslink)/rootfs"
TS_DATA_DIR=/var/lib/docker-plugins/tailscale PATH="$rootfs/usr/local/bin:/usr/sbin:/sbin:$PATH" \
  "$rootfs/tslink" diag --preflight
```

`TS_DATA_DIR` points `diag` at the plugin's data directory, which is `/data` only inside the plugin. The isolation
check (E5) runs the host's `iptables` and `ip6tables`, and the Tailnet Lock check (E9) runs the `tailscale` CLI; the
`PATH` gives them the host's sbin directories and the plugin's own `tailscale`. Without that, on a host with no
`tailscale` of its own, E9 is unknown. Leave out `--preflight` to run `diag`.

## scripts/tslink-diag.sh

The script shows the same information from the host, without any image. Run it from a clone of the repository.

| Command | What it does |
| --- | --- |
| `./scripts/tslink-diag.sh` | Shows the diagnostics once |
| `./scripts/tslink-diag.sh --json` | Shows the diagnostics as JSON |
| `./scripts/tslink-diag.sh --watch` | Shows the diagnostics again every 5 seconds |
