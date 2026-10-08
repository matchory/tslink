# tslink diag

`tslink diag` shows the state of each endpoint on a host. `tslink diag --preflight` checks the environment
properties that the guarantees in [SECURITY.md](../../SECURITY.md) rely on.

## Commands

| Command | Effect |
| --- | --- |
| `tslink diag` | Shows each endpoint with its state directory, tailnet address, host name, backend state and status file |
| `tslink diag --preflight` | Checks the environment properties on the host where it runs |
| `tslink diag --preflight --shared-dir <dir>` | Also checks the shared certificate directory, mounted at `<dir>` |
| `tslink help` | Shows the usage |

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

Then run the command:

| Command | Container options |
| --- | --- |
| `diag` | `-v /var/lib/docker-plugins/tailscale:/data` |
| `diag --preflight` | `--network host --cap-add NET_ADMIN -v /var/run/docker.sock:/var/run/docker.sock -v /var/lib/docker-plugins/tailscale:/data` |
| `diag --preflight --shared-dir /shared` | The options of `diag --preflight`, and `-v <shared directory>:/shared:ro` |

For example:

```bash
docker run --rm -v /var/lib/docker-plugins/tailscale:/data \
  --entrypoint /tslink tslink-diag diag
```

```bash
docker run --rm --network host --cap-add NET_ADMIN \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/lib/docker-plugins/tailscale:/data \
  --entrypoint /tslink tslink-diag diag --preflight
```

## scripts/tslink-diag.sh

The script shows the same information from the host, without an image. Run it from a clone of the repository.

| Command | Effect |
| --- | --- |
| `./scripts/tslink-diag.sh` | Shows the diagnostics once |
| `./scripts/tslink-diag.sh --json` | Shows the diagnostics as JSON |
| `./scripts/tslink-diag.sh --watch` | Shows the diagnostics again every 5 seconds |
