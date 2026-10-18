# tslink diag

`tslink diag` shows the state of each endpoint on a host. `tslink diag --preflight` checks the environment
properties that the guarantees in [SECURITY.md](../../SECURITY.md) rely on.

## Commands

| Command | Effect |
| --- | --- |
| `tslink diag` | Shows each endpoint with its state directory, tailnet address, hostname, backend state and status file |
| `tslink diag --preflight` | Checks the environment properties on the host where it runs |
| `tslink diag --preflight --shared-dir <dir>` | Also checks the owner and mode of the shared certificate directory (E7), mounted at `<dir>` |
| `tslink version` | Shows the versions of tslink and its bundled Tailscale |
| `tslink help` | Shows the usage |

Any other argument exits with status 2 and the usage. Run without arguments, the binary starts the plugin: run it
only as the plugin's entrypoint.

To see which version a host runs, run the binary from the plugin's root filesystem, as root:

```bash
/var/lib/docker/plugins/$(docker plugin inspect -f '{{.Id}}' tslink)/rootfs/tslink version
```

## Exit codes of the preflight check

| Exit code | Meaning |
| --- | --- |
| `0` | No property is violated. A result of "unknown" does not change the exit code. |
| `1` | A property is violated, or a check could not run. |

The preflight check examines only the host where it runs. Run it on each host, and on each node of a Swarm.

## Run tslink diag

`ghcr.io/matchory/tslink` contains Docker plugins, not images that `docker run` can pull. Build the image from a
clone of the repository:

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

To check the owner and mode of the shared certificate directory (E7), add `-v <shared directory>:/shared:ro` and
`--shared-dir /shared`. E2 does not use `--shared-dir`: it protects the host path in each tslink plugin's
`shared.source` setting.

To check more than one host, build the image once and copy it to each host with `docker save` and `docker load`.

### Without an image

On a host where the plugin is installed, the plugin's root file system contains the same binary. Run it in a root
shell on the host, for example after `sudo -i`:

```bash
rootfs="$(docker info -f '{{.DockerRootDir}}')/plugins/$(docker plugin inspect -f '{{.Id}}' tslink)/rootfs"
TS_DATA_DIR=/var/lib/docker-plugins/tailscale PATH="$rootfs/usr/local/bin:/usr/sbin:/sbin:$PATH" \
  "$rootfs/tslink" diag --preflight
```

`TS_DATA_DIR` points `diag` at the plugin's state directory, which is `/data` only inside the plugin. The check of
the isolation (E5) runs the host's `iptables` and `ip6tables`, and the check of Tailnet Lock (E9) runs the `tailscale`
CLI: the `PATH` gives them the host's sbin directories and the plugin's own `tailscale`. Without the plugin's, on a
host without `tailscale`, E9 is unknown. The same command without `--preflight` runs `diag`.

## scripts/tslink-diag.sh

The script shows the same information from the host, without an image. Run it from a clone of the repository.

| Command | Effect |
| --- | --- |
| `./scripts/tslink-diag.sh` | Shows the diagnostics once |
| `./scripts/tslink-diag.sh --json` | Shows the diagnostics as JSON |
| `./scripts/tslink-diag.sh --watch` | Shows the diagnostics again every 5 seconds |
