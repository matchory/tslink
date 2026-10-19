# Development Guide

## Quick Start

```bash
echo "TS_AUTHKEY=tskey-auth-xxx" > .env
make reinstall
make test-network test-container
```

## Prerequisites

- Go 1.27+
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
from the host veth. The address Docker's IPAM assigns the endpoint, which Docker lists in the container's `/etc/hosts`,
goes on the container's veth as a /32, so a process resolving its own name reaches itself; a /32 adds no route. The
veth is named after the first 8 characters of the endpoint ID: `CreateVethPair` replaces a link of that name without
an address in the veth range (a failed Join's leftover) but refuses one with an address, another endpoint's.

**Container side**: `netutil.SetupContainerSide` configures the container's end of the veth, for Join and again for
recovery: link up, its /30 address, the bypass route and rule, the IPAM /32 (a failure only logged) and the tailnet
blackhole (see Routing), all through one netlink handle in the container's namespace. Opening that handle is the only
time Join switches a thread into the namespace. `inNetNS` (also for `ListenInNetNS`) does that on a goroutine of its
own, and a thread that cannot switch back stays locked, so Go terminates it instead of reusing it.

**Routing**: Docker gives the container its usual gateway (`docker_gwbridge` on Swarm); without one, its embedded DNS
server refuses to resolve public names. tslink's veth carries only tailscaled's own traffic: tailscaled marks its
sockets with `0x80000`, and a rule at priority 5200 sends that mark to table 5200, a default route via the veth. Tailnet
ranges are `unreachable` in the main table, so tailnet traffic leaves through the container's tailscaled (table 52) or
not at all. Those routes stop sockets, not raw frames, and the host's tailscaled accepts all forwarded traffic, so the
host enforces it too: `netutil.SetupHostIsolation` drops traffic from Docker's bridges and veths to `tailscale+` and to
the host's tailnet addresses in the mangle table (`TSLINK_ISOLATE_HOST_TAILNET`), for every container on the host. It
matches Docker's default interface names and the bridge names Docker's bridge networks set
(`com.docker.network.bridge.name`), which the driver lists after start (every second until Docker answers), on every
watchdog run and on a network `create` event, keeping the last list if Docker cannot answer, saved in `/data/bridges`.
The plugin installs it before serving, with the saved list and the bridges the installed rules already cover
(`netutil.IsolatedBridges`), so a start never narrows it: after a host reboot Docker answers no API call involving
plugins until the plugin's socket is open, and disables a plugin that does not open it in time. The watchdog restores it
(a chain and its jump in one `iptables-restore --noflush` of the backend `iptables` uses, so a repair never opens a
gap), and it stays when the plugin stops. The host also forwards between tslink's veths, so `netutil.SetupVethIsolation`
(always on) drops everything forwarded to `10.200.0.0/16`, whatever interface it arrives on, except replies on
connections the container opened and tailscaled's WireGuard port, the fixed `--port=41641` (`tailscale.WireGuardPort`),
from tslink's veths: containers cannot reach each other's veth addresses, and colocated nodes keep their direct path
instead of falling back to DERP. The host masquerades `10.200.0.0/16`, and every tailscaled uses port 41641, so
MASQUERADE would remap a container's port towards destinations another flow from the host uses (STUN, DERP) and keep
it towards others, and hole punching between hosts would fail. So `SetupNAT` maps each container's `--sport 41641` to
a port of its own (`MASQUERADE --to-ports P`, chain `TSLINK-WG-SNAT` jumped to first in nat `POSTROUTING`), from
`TSLINK_WIREGUARD_PORTS` (default 61000-65535): from a hash of the container's veth address on, the first port no rule
uses, under `portMapMu`, which also guards an in-memory copy of the mappings. Each tailscaled also advertises the
host's private addresses with that port (`netutil.HostEndpoints`, `TS_DEBUG_PRETENDPOINT`, the port from that copy, so
without an iptables process), read at start; the supervisor restarts tailscaled when they change.
`TSLINK-WG-IN` (mangle INPUT, always on) drops new UDP to the port range, so a peer's probe that arrives first leaves
no conntrack entry blocking the container's own NAT. Adding a rule deletes the container's UDP conntrack entries from
port 41641, whose NAT would otherwise outlive it (keepalives never let them expire), for all containers mapped at once
in one pass through the conntrack table, after `portMapMu` is released; an existing rule is kept and its flows too. A
rule's `-s` is the container's address, so a restarted plugin keeps the port; `CleanupNAT` removes it, and
`SyncPortMappings` (run by `SetupVethIsolation`, so at start and by the watchdog) maps veths without a rule, removes
rules of addresses no veth has and rebuilds the copy. `SetupVethIsolation` likewise restores the forwarding a firewall
reload removes (`syncForwarding`, under `forwardMu`): `TSLINK-FORWARD`, its jump from filter `FORWARD`, the veths'
ACCEPT rules and MASQUERADE of `10.200.0.0/16`. These checks list each table once with the backend's `iptables-save`
(`ipFamily.list`); a chain whose rules are listed as they were when tslink last found them in place or wrote them
counts as in place, and any other listing is checked rule by rule with `-C`, as before. The iptables of each family,
these listings, the mappings and their locks live in `netutil.hostState` (`defaultHost`; tests make their own).

**Self-healing**: `Endpoint.RunTailscale` retries a failed start with backoff until the endpoint leaves, unless the
error is a `permanentError` (wrong stack, invalid hostname). The supervisor restarts a crashed tailscaled, resumes after
a crash-loop cooldown, and logs a node in again, as a new device, when control answers 404 node not found. After a
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
certificate exists, or once control has refused its domain for `certDomainGrace` (30 s), in case control makes it a
certificate domain only of hosts that advertise it. A drain does not wait for a serve configuration in progress: the
gate holds the cancel func of the change holding its lock (`serviceGate.change`), which the drain calls before taking
the lock, and the change runs nothing more once drained. Plugin shutdown stops all endpoints concurrently within its
deadline (`Driver.Shutdown`), and an endpoint's Tailscale run derives from the driver's context, so a start in progress
ends with the plugin and is abandoned as on `Stop`. A daemon's CLI calls and waits use its context, cancelled by
`daemon.Stop`, and its goroutines run under its wait group. Each state directory serves one endpoint at a time
(`ClaimStateDir`). A drain clears every Service the node advertises in one prefs edit (`daemon.Drain`), so one fetch by
control covers all; at start, Services in the node's state its labels do not name are removed (`pruneServices`); a
node that had any removed, or could not have them removed, is drained at stop like a Service host (`NeedsDrain`), and
waits for control to fetch the removal.

**Binaries**: the plugin image ships a pinned Tailscale (from the `tailscale/tailscale` stage of the Dockerfile, which
Dependabot updates), and tslink runs only these binaries: it downloads nothing at runtime and ignores the old
`TS_VERSION`/`TS_PATH` settings with a warning. tailscaled's output goes to a rotated `tailscaled.log`, read by
`drainLines`, which never stops reading: tailscaled blocks on a full pipe. tslink talks to tailscaled through its
LocalAPI (`tailscale.com/client/local`, `daemon.lc`), whose module must be the bundled Tailscale's version:
`TestLocalAPIClientMatchesBundledTailscale` fails until Dependabot's Dockerfile and Go module updates agree. tslink
runs no `tailscale` CLI: serve configs are built with `ipn.ServeConfig`'s helpers, as the CLI builds them
(`applyServeEndpoint`), and set with the version they were read at; advertising and draining edit the prefs'
`AdvertiseServices`. The image still ships the CLI for debugging.

**Login**: `daemon.logIn` does what `tailscale up` does, over the LocalAPI. The node's settings are an
`ipn.ConfigVAlpha`, the format of tailscaled's config file (as the Kubernetes operator configures its proxies),
applied as the prefs edit tailscaled would make of it, minus Services: the config file would set those on every start.
The tags go in the same edit, and the auth key only into the LocalAPI's start request, never into arguments, the
environment or a file. An OAuth client secret first becomes a single-use key with the tags (`resolveAuthKey`, Tailscale's
API). The login waits on the IPN bus for Running, and when logging in as a new device for a new node key.

**Services**: a container's Services (`tslink.service.<name>.serve.<port>`, or `tslink.service` with
`tslink.serve.<port>`) each get a `serviceBackend` (`pkg/tailscale/backend.go`): its serve config, advertisement,
certificate lease and wait, and approval check. The backends share the supervisor's `serviceGate`. A backend whose
configuration fails is retried in place (`retryLater`), with a status warning until it succeeds; only tags and MagicDNS,
which all need, fail the start. After advertising, a backend waits for control to fetch the node's Service list; control
was seen to skip that fetch after a plugin restart, leaving the host inactive. Without a fetch within 15 s it
unadvertises and advertises the Service again, up to 3 times (`confirmAdvertisedLater`), and then warns in the status
(`control/<svc>`) until a fetch. A backend notes the fetch count when tslink adds the Service to the prefs
(`markAdvertised`; one already in them counts as fetched), and is `awaiting-control` until the count passes it, even
with `service-host`. Control routes nothing to a host that does not serve every port of the Service's definition: from
the node's `services/` capabilities (`tailcfg.ServiceDetails`), or else Tailscale's API when the credential is an OAuth
client with `services:read` (`refreshDefinedPorts`, with each approval check), ports not served make it
`ports-uncovered` with a `ports/<svc>` warning; unknown or `do-not-validate` ports are not checked.

**Readiness endpoint**: with the label `tslink.health=<port>`, `Endpoint.startHealth` serves `GET /ready` on that port
of the container's loopback, from a listener the plugin opens in the container's netns (`netutil.ListenInNetNS`).
Readiness comes from tailscaled (`DaemonSupervisor.Readiness`: backend state, the serve config, the advertised
Services in its prefs, the `service-host` node capability for control's approval, the Service's defined ports and
control's fetch of the Service list, see Services), cached for 2 s. Once ready, the
endpoint latches: `status/<endpoint-id[:12]>.ready` records it, so it answers 200 until the backend is drained or the
container leaves, also after a plugin restart. A latched endpoint answers without waiting for tailscaled, from the cache,
which the request refreshes in the background. Readiness is per Service (`ReadinessReport`); the container's state is
the least ready, read from one status, serve config and prefs query each, and `/ready` lists each Service's current
state under `services`.

## Concurrency Notes

**Lock ordering**: Never hold `driver.mu` when calling endpoint methods (they acquire `endpoint.mu`). Always:
driver.mu → endpoint.mu, never reversed.

**Long operations outside locks**: Network syscalls and logins can block for
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

Goroutines of an endpoint's Tailscale run, its supervisor and tailscaled carry the label `endpoint` (the first 12
characters of its ID), which panics and goroutine dumps show in each goroutine's header.

## Project Structure

```text
pkg/
├── docker/     # Docker network driver (driver.go, events.go)
├── core/       # Endpoint/network logic (endpoint.go, network.go)
├── tailscale/  # Daemon lifecycle (daemon.go, command.go, output.go, supervisor.go, binary.go)
├── netutil/    # Linux networking (veth.go - veth, routing, NAT)
├── preflight/  # `tslink diag --preflight`: SECURITY.md's environment properties, via the Docker API
└── logger/     # Structured logging
internal/
├── leakcheck/  # TestMain helper: fails a package's tests if they leak goroutines
└── secmodel/   # Checks that every guarantee in SECURITY.md is guarded
test/
└── security/   # lib.sh: security probes with control runs, used by test/integration/run.sh
    ├── t1.sh     # The single-host testbed the t1 probes source (plugin, headscale, host tailscaled)
    ├── tn.sh     # The Swarm bed of test/cluster the tn probes source, reached over SSH
    ├── redteam-tn.sh  # Runs the tn probes and writes their report for report.sh
    ├── testbed/  # Scripts for a single-host security testbed (optional Hetzner Cloud provisioner)
    └── redteam/  # Standalone probe scripts for redteam.sh, moved here once their fix ships
```

`pkg/core`, `pkg/docker` and `pkg/tailscale` run their tests through `leakcheck.Main`, which reads Go's `goroutineleak`
profile after the tests: goroutines blocked forever on a channel or lock nothing else can reach fail the package.

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
  namespace tests in `pkg/netutil` and the mount namespace test in `pkg/tailscale` skip without root, so CI runs them
  a second time with `sudo`, and the netutil ones a third time with `iptables-legacy`. It also tests the probe
  library, `test/security/lib_test.sh`. Lint findings go to code scanning as SARIF (category `golangci-lint`); test
  results become annotations and a job summary (JUnit from `gotestsum`); coverage of all runs is merged with
  `go tool covdata` into the job summary and a `coverage` artifact, with no threshold.
- `govulncheck.yml`: govulncheck findings as SARIF in code scanning, on pushes, PRs and weekly. `gotestsum` and
  `govulncheck` are pinned in the `tool` block of `go.mod` (`go tool ...`), so Dependabot updates them.
- `linter.yml`: super-linter for everything except Go (Markdown, YAML, shell, Dockerfile). Configs are in
  `.github/linters/`.
- `e2e.yml`: `test/integration/run.sh`, the end-to-end test. It builds the plugin, installs it as `tslink` and runs
  containers against a headscale control server on the runner (`tslink.loginserver`), so it needs no Tailscale
  account: tailnet reachability, an ACL refusal, identity across a plugin restart, and removal of an ephemeral node.
  Tailscale Services are not covered: headscale does not support them. The headscale and Alpine images are pinned
  by digest in `run.sh`, which Dependabot does not update. It replaces the host's `tslink` plugin; run it locally only
  on a disposable Linux machine.
- `fuzz.yml`: each fuzz target for 10 minutes, weekly and on manual dispatch; a failing input is uploaded as an
  artifact.
- `codeql-analysis.yml`: CodeQL for Go and the workflows.
- `release.yml`: a push to `main` publishes `ghcr.io/matchory/tslink:main-<arch>`; a `vX.Y.Z` tag publishes
  `vX.Y.Z-<arch>` and `latest-<arch>` and creates a GitHub release. Docker plugins have no multi-arch manifests, so
  each architecture is built on a native runner and pushed under its own tag.
- Dependencies are updated by Dependabot (`.github/dependabot.yml`): Actions, Go modules and the Dockerfile base images.

`test/cluster` runs `regress.sh` against any Swarm of three or more nodes reached over SSH and a real tailnet
(`provision.sh` can optionally build one on Hetzner Cloud); see `test/cluster/README.md`.

## Security Model Tests

`SECURITY.md` publishes guarantees (`### G<n>:`) and the environment properties they assume (E1, E2, ...). A test
that verifies a guarantee carries its ID: `// Guards: G1` in the doc comment of a Go test, `\* Guards: G1` in a TLA+
model, and `probe G1 ...` or a `# Guards: G1` header for a shell probe. `internal/secmodel` (`TestRepository`,
part of `go test ./...`) fails if a published guarantee has no marker, unless its section has a `Manual:` line;
markers may name G1 to G10, published or not. A shell probe goes through `test/security/lib.sh`, which runs it once
against the defended system, where it must fail, and once in a control run with the defence off, where it must get
through: a probe whose control run does not get through is reported as BROKEN, since it could not catch a regression.
The red team's probes (`test/security/redteam/`) run on the single host (`t1.sh`, through `redteam.sh`) or the Swarm
bed (`tn.sh`, through `redteam-tn.sh`), and report INFO when their control violates an environment property.

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
