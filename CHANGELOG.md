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
- Cluster credential: networks without `tslink.authkey` register their nodes
  with an OAuth client secret in `oauth-client.secret` in the plugin's data
  directory, so stack files carry only their tags. The file is read at every
  registration, so replacing it rotates the secret. A stack using it may only
  use `tag:<stack>` and `tag:<stack>-*`.
- `tslink.ephemeral=true|false` network option: whether the network's nodes
  are ephemeral, for keys that do not say so, such as auth keys and
  headscale's keys. With `true`, nodes are logged out and their state deleted
  when the container stops. It is appended to an OAuth client secret without
  an `ephemeral` parameter; a contradicting parameter, an invalid value, or
  `false` with the cluster credential fails network creation.
- Shared HTTPS certificates: tailscaled keeps its certificates in the new
  `shared` mount, which can point at a volume shared between hosts. Replicas of
  a Tailscale Service share its certificate: one replica issues it while the
  others wait, and they are advertised once it exists.

- Garbage collection cleans the shared certificate directory: certificates
  (and keys) of names that expired more than 7 days ago, keys without a
  certificate and temporary files, tslink's and tailscaled's, older than a
  day, and certificate leases not refreshed for an hour. The ACME account key
  and unknown files are kept.
- Warnings about shared HTTPS certificates: a renewal the ACME server refused
  because another replica's renewal order is pending (`alreadyReplaced`), and
  a certificate with less than 14 days left. The plugin log warns once an
  hour, or once a day for the expiry, per name; the endpoint's status file
  lists them under the new `warnings` field until a certificate is issued, and
  so does `tslink diag`.

### Changed

- The Go module path is `github.com/matchory/tslink`.
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
- `tslink.direct` defaults to `false` for containers with `tslink.service`, so
  Service replicas no longer each serve on, and fetch a certificate for, a
  name of their own. tslink warns when a container serves HTTP on its own
  name ([tailscale/tailscale#21693](https://github.com/tailscale/tailscale/issues/21693)).
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
- HTTPS serving never got a certificate on hosts whose resolver is on the
  host's loopback, such as systemd-resolved: tailscaled used the host's
  `resolv.conf` inside the container's network namespace. It now uses the
  host's upstream resolvers as Docker picks them for containers:
  systemd-resolved's upstreams instead of `127.0.0.53`. Not Docker's embedded
  resolver, which forwards to tailscaled in a container with
  `dns: [100.100.100.100]`, and tailscaled forwarded back to it, so public
  names did not resolve.
- The first replica of a Tailscale Service with HTTPS waited up to an hour for
  its certificate, since tailscaled only fetches it right away if control has
  already announced the Service's name.
- A hostname's state directory kept its `ephemeral` marker when a
  persistent network used it after an ephemeral one, so garbage collection
  could log the persistent node out and delete its state. The marker now
  follows the network that uses the directory.
- Callers of a Tailscale Service failed for minutes when a backend left right
  after it was drained: its node was logged out before control had processed
  the drain. Leaving now waits for control to fetch the drained Service list,
  at least one and at most ten seconds, before it logs the node out or stops
  its tailscaled.
- A Service backend whose task crashed or completed got no stop signal and
  was never drained. Leaving drains it.
- Leaving or deleting an endpoint held the driver's lock while it logged the
  node out, and blocked every other network call on the node meanwhile.
- A replica drained while it waited for its Service's certificate was
  advertised again once the certificate appeared, or when its tailscaled
  restarted. The replica issuing the certificate was advertised before the
  certificate existed; it now waits for it.
- Such a replica, never advertised, waited ten seconds when it left and
  warned that control did not fetch its drain: there was nothing to fetch.
- Every container leaving logged `Failed to kill tailscaled: os: process
  already finished`: tailscaled exits by itself once it is down.

- A container whose endpoint recovery failed, for example because its
  routing could not be restored, could not start Tailscale on its next
  endpoint: its state directory stayed claimed and the start retried forever.
- A container that joined while the Docker event stream was down never
  started Tailscale. The watcher now asks for missed events when it
  reconnects, and the watchdog starts joined endpoints that never got a start.
- Recovery no longer crashes on containers whose network settings Docker
  leaves out.
- Errors from `tailscale serve`, `advertise`, `drain` and `logout` include
  the CLI's output again, and the hints for untagged Service hosts and
  pending approval appear: tslink dropped the output of every completed
  line.

### Security

- Tailnet ranges are unreachable in the container's main routing table, so
  tailnet traffic leaves through the container's own tailscaled or not at
  all, never through the host's Tailscale.
- The auth key is passed to `tailscale up` on stdin instead of its command
  line, where it was visible in the host's process list.

[Unreleased]: https://github.com/matchory/tslink/compare/v0.1.0...HEAD
