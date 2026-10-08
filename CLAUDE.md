# Development Guide

## Quick Start

```bash
echo "TS_AUTHKEY=tskey-auth-xxx" > .env
make reinstall
make test-network test-container
```

## Prerequisites

- Go 1.26+
- Docker (via OrbStack, Docker Desktop, or native Linux)
- Tailscale auth key from <https://login.tailscale.com/admin/settings/keys>

## Development Cycle

```bash
# Edit code in pkg/ or cmd/, then:
make reinstall

# Test
source .env
docker network create --driver tslink:latest --opt tslink.authkey=$TS_AUTHKEY tailnet
docker run --rm --network tailnet alpine sh -c "ip addr && ping -c 2 8.8.8.8"
docker network rm tailnet
```

## Key Design Decisions

**Hostname-based state directories**: Tailscale state is stored in `/data/by-hostname/<hostname>/`, or
`/data/by-stack/<stack>/<hostname>/` for Swarm stack tasks, not by endpoint ID. This enables identity reuse - if a
container restarts with the same name, it keeps its Tailscale identity and IP.

**Async Tailscale setup**: Docker's `Join()` must return quickly, but Tailscale auth can take 60+ seconds. Solution:

1. `Join()` sets up veth networking and returns immediately
2. Docker event watcher detects container start, gets container name
3. Tailscale setup runs async in background goroutine

**Veth IP allocation**: Each container gets a /30 subnet from 10.200.0.0/16: the one its endpoint ID hashes to, or
the first one after it that no host interface uses, chosen and assigned under one lock. Recovery reads the address back
from the host veth.

**Routing**: Docker gives the container its usual gateway (`docker_gwbridge` on Swarm); without one, its embedded DNS
server refuses to resolve public names. tslink's veth carries only tailscaled's own traffic: tailscaled marks its
sockets with `0x80000`, and a rule at priority 5200 sends that mark to table 5200, a default route via the veth. Tailnet
ranges are `unreachable` in the main table, so tailnet traffic leaves through the container's tailscaled (table 52) or
not at all. Those routes stop sockets, not raw frames, and the host's tailscaled accepts all forwarded traffic, so the
host enforces it too: `netutil.SetupHostIsolation` drops traffic from Docker's bridges and veths to `tailscale+` and to
the host's tailnet addresses in the mangle table (`TSLINK_ISOLATE_HOST_TAILNET`), for every container on the host. The
plugin installs it before serving, the watchdog restores it, and it stays when the plugin stops. The host also forwards
between tslink's veths, so `netutil.SetupVethIsolation` (always on) lets only tailscaled's WireGuard port, the fixed
`--port=41641` (`tailscale.WireGuardPort`), through from container interfaces to `10.200.0.0/16`: containers cannot
reach each other's veth addresses, and colocated nodes keep their direct path instead of falling back to DERP.

**Self-healing**: `Endpoint.RunTailscale` retries a failed start with backoff until the endpoint leaves, unless the
error is a `permanentError` (wrong stack, invalid hostname). The supervisor restarts a crashed tailscaled, resumes after
a crash-loop cooldown, and logs a node in again with `--force-reauth` when control answers 404 node not found. After a
plugin restart, recovery adopts running containers' endpoints, and a node still logged in comes up without the auth
key, so a rotated or revoked secret does not take it down. Garbage collection then removes ephemeral state (marked by
an `ephemeral` file), sockets and status files no endpoint uses. It first logs each such ephemeral node out, which
deletes its device and frees its name: `tailscale.LogoutState` runs tailscaled with userspace networking on a copy of
the state with only the machine key and the current profile, its prefs logged out (so it never logs in or comes
online) and without services or serve config. The state goes even if the logout fails.

**Stop and drain**: a Docker `kill` event with the container's stop signal drains its Tailscale Service backends before
the container exits; plugin shutdown drains all of them. `Leave` drains a Service host itself if that did not happen
(a task that crashed or completed gets no `kill` event), then waits for control to fetch the drained Service list:
tailscaled logs `c2n: GET /vip-services received`, at least 1 s and at most 10 s after the drain
(`DaemonSupervisor.DrainAndWait`). Only then does it log ephemeral nodes out, with retries, and delete their state, or
stop tailscaled. A node gone before control processed its drain leaves callers pointed at it for minutes. A drain is
final for the supervisor (`serviceGate`, shared by its daemons): neither a certificate that appears later nor a
restarted tailscaled advertises the backend again, and the certificate lease holder is advertised only once its
certificate exists. Each state directory serves one endpoint at a time (`ClaimStateDir`).

**Binaries**: the plugin image ships a pinned Tailscale (from the `tailscale/tailscale` stage of the Dockerfile, which
Dependabot updates), and tslink runs only these binaries: it downloads nothing at runtime and ignores the old
`TS_VERSION`/`TS_PATH` settings with a warning. tailscaled's output goes to a rotated `tailscaled.log`, read by
`drainLines`, which never stops reading: tailscaled blocks on a full pipe.

**Readiness endpoint**: with the label `tslink.health=<port>`, `Endpoint.startHealth` serves `GET /ready` on that port
of the container's loopback, from a listener the plugin opens in the container's netns (`netutil.ListenInNetNS`).
Readiness comes from tailscaled (`DaemonSupervisor.Readiness`: backend state, `serve status --json`, the advertised
Services in its prefs, and the `service-host` node capability for control's approval), cached for 2 s. Once ready, the
endpoint latches: `status/<endpoint-id[:12]>.ready` records it, so it answers 200 until the backend is drained or the
container leaves, also after a plugin restart.

## Concurrency Notes

**Lock ordering**: Never hold `driver.mu` when calling endpoint methods (they acquire `endpoint.mu`). Always:
driver.mu → endpoint.mu, never reversed.

**Long operations outside locks**: Network syscalls and `tailscale up` can block for
seconds. Don't hold locks during these.

## Debugging

```bash
# View endpoint logs, and containers whose Tailscale is not running
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine \
  sh -c 'for d in /data/by-hostname/*/ /data/by-stack/*/*/; do
    echo "=== $d ==="; tail -20 "$d/tailscaled.log" 2>/dev/null
  done'
docker run --rm -v /var/lib/docker-plugins/tailscale:/data alpine sh -c 'cat /data/status/*.json'

# Plugin logs (Linux)
journalctl -u docker -f | grep -i tailscale

# Plugin logs (macOS/OrbStack)
docker run --rm -it --privileged --pid=host alpine nsenter -t 1 -m -u -n -i sh
# Then: journalctl -u docker -f
```

## Project Structure

```text
pkg/
├── docker/     # Docker network driver (driver.go, events.go)
├── core/       # Endpoint/network logic (endpoint.go, network.go)
├── tailscale/  # Daemon lifecycle (daemon.go, supervisor.go, binary.go)
├── netutil/    # Linux networking (veth.go - veth, routing, NAT)
└── logger/     # Structured logging
```

**Key paths at runtime:**

- State: `/data/by-hostname/<hostname>/tailscaled.state`, or `/data/by-stack/<stack>/<hostname>/` for stack tasks
- Socket: `/data/sock/<endpoint-id[:12]>.sock` (kept short: Unix socket paths are limited to 108 bytes), linked
  from `<state dir>/tailscaled.sock`
- Logs: `<state dir>/tailscaled.log` (rotated at 10 MB), `/data/plugin.log` (rotated at 50 MB)
- Status: `/data/status/<endpoint-id[:12]>.json` while the endpoint exists

## Code Style

### Linting

```bash
golangci-lint run        # Check for issues
golangci-lint run --fix  # Auto-fix where possible
golangci-lint fmt        # Format code
```

Config is in `.golangci.toml`. Key linters enabled:

- `errcheck`, `errorlint`, `nilerr` - error handling
- `gosec` - security
- `govet`, `staticcheck` - correctness
- `modernize` - Go 1.22+ idioms

Formatters are `gci`, `gofumpt` and `golines` (100 columns). `golines` counts trailing comments, so put a
`//nolint` that would not fit on the line before. Functions stay under a cyclomatic complexity of 15.

### Error Handling

**Always handle errors explicitly** - never ignore silently:

```go
// GOOD - log cleanup errors
if err := cleanup(); err != nil {
    logger.Warnf("cleanup failed: %v", err)
}

// BAD - silent ignore
cleanup()
_ = cleanup()
```

**Use `errors.Is`/`errors.As`** for sentinel errors:

```go
// GOOD
if errors.Is(err, io.EOF) { ... }
if errors.As(err, &netlink.LinkNotFoundError{}) { ... }

// BAD - breaks with wrapped errors
if err == io.EOF { ... }
```

**Wrap errors with context** using `%w`:

```go
return fmt.Errorf("failed to create endpoint: %w", err)
```

## CI

- `ci.yml`: golangci-lint, pinned to the version `.golangci.toml` is written for, and the Go tests. The network
  namespace tests in `pkg/netutil` and the mount namespace test in `pkg/tailscale` skip without root, so CI
  runs them a second time with `sudo`.
- `linter.yml`: super-linter for everything except Go (Markdown, YAML, shell, Dockerfile). Configs are in
  `.github/linters/`.
- `e2e.yml`: `test/integration/run.sh`, the end-to-end test. It builds the plugin, installs it as `tslink` and runs
  containers against a headscale control server on the runner (`tslink.loginserver`), so it needs no Tailscale
  account: tailnet reachability, an ACL refusal, identity across a plugin restart, and removal of an ephemeral node.
  Tailscale Services are not covered: headscale does not support them. The headscale and Alpine images are pinned
  by digest in `run.sh`, which Dependabot does not update. It replaces the host's `tslink` plugin; run it locally only
  on a disposable Linux machine.
- `codeql-analysis.yml`: CodeQL for Go and the workflows.
- `release.yml`: a push to `main` publishes `ghcr.io/matchory/tslink:main-<arch>`; a `vX.Y.Z` tag publishes
  `vX.Y.Z-<arch>` and `latest-<arch>` and creates a GitHub release. Docker plugins have no multi-arch manifests, so
  each architecture is built on a native runner and pushed under its own tag.
- Dependencies are updated by Dependabot (`.github/dependabot.yml`): Actions, Go modules and the Dockerfile base images.

`test/cluster` builds a three-node Swarm on a cloud provider (the scripts currently target Hetzner Cloud) and runs
`regress.sh` against a real tailnet; see `test/cluster/README.md`.

## Troubleshooting

### Plugin won't enable

Check that the state directory exists:

```bash
docker run --rm --privileged -v /var/lib:/var/lib alpine \
  mkdir -p /var/lib/docker-plugins/tailscale
```

### Options not being passed

Docker passes options with the full key. Debug by adding logging:

```go
log.Printf("Options: %+v", req.Options)
```

### Container networking issues

Check that tailscaled is running in the container's netns:

```bash
# From inside container
ps aux | grep tailscale
ip addr
```
