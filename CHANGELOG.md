# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Changes since upstream [aaomidi/tslink](https://github.com/aaomidi/tslink)
v0.1.0, of which this project is a fork. The Swarm features were tested on
three- and five-node swarms; see [docs/testing](docs/testing).

### Added

- Docker Swarm support. The driver has global scope, so the options of a
  network created on a manager reach every node. A network created by
  `docker stack deploy` serves only that stack's tasks, and stack tasks keep
  their state under `by-stack/<stack>/<hostname>`.
- `tslink.tags` network option. It overrides the `tslink.tags` container
  label, so containers cannot choose their own tags.
- Tailscale OAuth client secrets as `tslink.authkey`. Their nodes are
  ephemeral unless `?ephemeral=false` is appended; ephemeral nodes are logged
  out, with retries, and their state deleted when the container leaves.
- Self-healing: a failed Tailscale start is retried with backoff until the
  container leaves, unless the error is permanent (wrong stack, invalid
  hostname). The supervisor resumes after a crash-loop cooldown, and logs a
  node in again with `--force-reauth` when its device was deleted or expired.
- Recovery after a plugin restart reapplies routes and records the veth of
  running containers, and brings up nodes that are still logged in without
  the auth key, so a revoked or rotated secret does not take them down.
- Garbage collection after recovery and after every watchdog scan removes
  ephemeral state, sockets and status files that no endpoint uses. It logs
  orphaned ephemeral nodes out first, so their devices disappear at once and
  a fixed `tslink.hostname` keeps its name after a crash.
- Draining: Tailscale Service backends are drained when Docker sends a
  container its stop signal and when the plugin shuts down, and advertised
  again on start.
- `deploy/systemd`: a `docker.service` drop-in and `tslink-drain` script that
  drain every backend on the node before dockerd stops, for restarts and
  reboots.
- Status files: each endpoint's state (`running`, `retrying` with the error,
  or `failed`) in `/var/lib/docker-plugins/tailscale/status/<endpoint>.json`.
  `tslink diag` and `scripts/tslink-diag.sh` list containers whose Tailscale
  is not running, and why.
- The plugin image ships a pinned Tailscale, updated by Dependabot.
- `com.docker.network.driver.mtu` sets the MTU of tslink's interface.
- `tslink.loginserver` network option: a custom control server, such as
  headscale.
- `tslink.serve` options: `?proxy-protocol=1|2` on `tcp` and
  `tls-terminated-tcp` endpoints sends the caller's address in a PROXY
  protocol header; `?accept-app-caps=<cap>[,<cap>]` on `http` and `https`
  endpoints forwards the caller's app capabilities in the
  `Tailscale-App-Capabilities` header.
- tailscaled writes to its own `tailscaled.log` per endpoint, rotated at
  10 MB, instead of the plugin log; `plugin.log` is rotated at 50 MB.
- Documentation: [node provisioning](docs/node-provisioning.md),
  [credentials and tags on Swarm](docs/credentials.md), the Swarm test
  reports, `SECURITY.md` and `CONTRIBUTING.md`.
- `test/cluster`: scripts that build a Swarm test bed and a regression suite.
- `test/integration`: an end-to-end test against a local headscale, run in CI
  on every pull request.

### Changed

- Images are published as `ghcr.io/matchory/tslink:<version>-<arch>`. The
  documentation installs the plugin under the alias `tslink`, so networks and
  stack files name the driver `tslink:latest` whatever the version and
  architecture.
- The plugin recognises its networks by ID, not by the driver name, so it
  works under any alias.
- Routing: containers keep Docker's gateway, so Docker's DNS server resolves
  public names for containers on a tslink network alone. Only tailscaled's
  own traffic, marked `0x80000`, routes through tslink's veth.
- A state directory serves one endpoint at a time; a second container with
  the same hostname on a node waits without an identity.
- Container names that are not valid hostnames are converted to valid
  hostnames; an invalid `tslink.hostname` label is refused.
- CI moved to GitHub Actions with golangci-lint, super-linter and CodeQL;
  dependency updates come from Dependabot instead of Renovate.

### Removed

- Downloading Tailscale at runtime: tslink runs only the bundled binaries.
  `TS_VERSION` other than `bundled` and `TS_PATH` are ignored with a warning.

### Fixed

- tailscaled froze when it wrote a line longer than 64 KiB: the plugin
  stopped reading its output, and tailscaled blocked on the full pipe.
- After a reboot the plugin failed to start, because it mounted
  `/var/run/docker/netns` before Docker had created it, and dockerd then kept
  restarting.
- Devices were orphaned when Docker detached a container's other networks
  before tslink logged it out.
- Recovered endpoints leaked their veth and NAT rules when they stopped.
- Two containers with the same `tslink.hostname` on one node shared a node
  key.
- After a plugin restart, networks without running containers were
  forgotten, and new containers on them failed with "network not found".
- A Service whose configuration arrived after Tailscale was up was never
  advertised again, so a backend drained in its state stayed drained.

### Security

- Tailnet ranges are unreachable in the container's main routing table, so
  tailnet traffic leaves through the container's own tailscaled or not at
  all, never through the host's Tailscale.
- The auth key is passed to `tailscale up` on stdin instead of its command
  line, where it was visible in the host's process list.

[Unreleased]: https://github.com/matchory/tslink/compare/v0.1.0...HEAD
