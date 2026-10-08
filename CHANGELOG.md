# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Changes since upstream [aaomidi/tslink](https://github.com/aaomidi/tslink)
v0.1.0, of which this project is a fork. The Swarm features were tested on
three- and five-node swarms; see [docs/testing.md](docs/testing.md).

### Added

- `tslink.health=<port>` container label: tslink serves `GET /ready` on that
  port of the container's loopback, `200` once the node is up, its serve
  configuration applied and, for a Tailscale Service, the node advertised and
  approved by control as a backend; `503` with the state otherwise. It lets a
  Docker healthcheck gate start-first updates on the Service having its new
  backend. Once ready, a task stays ready until it is drained or leaves, so a
  plugin restart does not make Swarm replace it.
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
- A `dns-upstreams` warning in the endpoint's status and `tslink diag` when
  tailscaled falls back to resolvers neither the container nor the host
  lists; a later start with resolvers of either clears it.
- Panics and goroutine dumps name the endpoint of each goroutine that runs
  its Tailscale: they carry the label `endpoint` with the first 12
  characters of the endpoint ID.
- SECURITY.md describes tslink's security model: the guarantees it makes and
  the environment properties they rely on. A test fails if a published
  guarantee has no test that verifies it.
- `tslink diag --preflight` checks, on the host it runs on, the environment
  properties the security model relies on: privileges and mounts of
  containers on tslink networks, Swarm tasks outside stacks, the host's
  tailnet isolation (its setting and iptables chains), how the plugin was
  installed, the shared certificate directory and Tailnet Lock on every
  node. Run it on every node of a Swarm.
- SECURITY.md publishes two more guarantees: containers do not reach each
  other through tslink's veths (G2), and a stack's nodes get only what its
  own network grants (G4).

### Changed

- Building tslink requires Go 1.27.1 or later.
- CI reports golangci-lint and govulncheck findings to GitHub code scanning,
  and test results and per-package coverage in each run's summary.
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
- Network creation fails when `tslink.authkey` or `TS_AUTHKEY` is a key other
  than an OAuth client secret with parameters (`?...`) appended: `tailscale
  up` would pass them to control as part of the key, which is then invalid.

### Removed

- Downloading Tailscale at runtime: tslink runs only the bundled binaries.
  `TS_VERSION` other than `bundled` and `TS_PATH` are ignored with a warning.

### Fixed

- Two containers on one host could get the same veth subnet, with even
  odds at about 150 tslink containers per host, and 1% at about 18. The host
  then sent the second container's Tailscale traffic to the first, and the
  second stayed offline. A container now gets the first subnet, from the one
  its endpoint ID names, that no host interface uses.
- After a plugin restart, nodes that were still logged in could stay offline
  for good against a control server not on port 443, such as headscale on
  port 8080: `tailscale up` restarted their control client right after
  tailscaled had dialled control, and tailscaled then dials port 443 only,
  retry after retry. Such nodes now get their settings with `tailscale set`,
  and `tailscale up` runs only when their tags or control server changed.
- On hosts with systemd-resolved, tailscaled fell back to the plugin's
  `resolv.conf` after any change of the host's DNS, so Tailscale's resolver
  stopped forwarding to the container's other DNS servers: systemd-resolved
  replaces the file the plugin's `resolv.conf` is a bind mount of, and the
  kernel refuses to mount over a replaced file. tailscaled's mount namespace
  now drops that mount first.
- Stopping tailscaled waited for its process in two goroutines at once,
  which `os/exec` does not allow: a data race.
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
- A container with `dns: [100.100.100.100, 10.0.0.53]` never reached
  `10.0.0.53`: Docker asks the first server and accepts its "not found", and
  tailscaled forwarded to the host's resolvers. tailscaled now forwards to
  the container's own DNS servers other than Tailscale's and Docker's, as
  Docker's embedded resolver does, and to the host's only without any.
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
- A connect event or watchdog start that arrived while a container was
  leaving could start its tailscaled again, which then ran until Docker
  deleted the endpoint. Once a container begins to leave, nothing starts it.
- Errors from `tailscale serve`, `advertise`, `drain` and `logout` include
  the CLI's output again, and the hints for untagged Service hosts and
  pending approval appear: tslink dropped the output of every completed
  line.

### Security

- tailscaled no longer stores Taildrop files. They went to its state
  directory on the host, where no container sees them: a peer the control
  server lets send files, or an untagged node's own container, could fill the
  host's disk. A read-only mount in tailscaled's own mount namespace now
  refuses them, files received before are deleted when tailscaled starts, and
  tailscaled does not start if the mount fails.
- Containers no longer reach each other through tslink's veths. The host
  forwards between the veths' subnets, so a container reached any tslink
  container's veth address, on any network, past Docker's network isolation
  and the tailnet's ACLs: with an ordinary socket through its Docker gateway,
  or with raw frames through its veth. Only tailscaled's WireGuard port passes
  between them now, so colocated nodes keep their direct connection rather
  than falling back to DERP; tailscaled listens on the fixed port 41641 for
  this. The rules are always on and stay when the plugin stops.
- A stack using the cluster credential is now confined to the exact tag
  `tag:<stack>`, not `tag:<stack>-*`. The prefix form let a stack claim a
  longer-named stack's base tag: stack `a` could register `tag:a-b`, stack
  `a-b`'s identity. The plugin setting `TSLINK_TAG_SCOPE=prefix` restores the
  old behaviour for operators who need it.
- Containers no longer reach the tailnet through the host's own tailscaled.
  On a host running tailscaled, any container reached the tailnet with the
  host's identity: one on Docker's bridges with an ordinary socket, and one on
  tslink with raw frames (`CAP_NET_RAW`, granted by default) past the
  unreachable routes in its namespace, through its veth or `docker_gwbridge`.
  tslink now drops traffic from container interfaces to `tailscale*` and to the
  host's tailnet addresses, in the host's mangle table, where containers cannot
  change it. DNS to `100.100.100.100` stays allowed. This applies to all
  containers on the host; `TSLINK_ISOLATE_HOST_TAILNET=false` turns it off.
- The certificate domain, built from the `tslink.service` label and the
  control server's MagicDNS suffix, is checked to be a DNS name before it
  names files in the certificate directory. A label or suffix with `..` or a
  slash could otherwise make the plugin create and delete lease files outside
  that directory, as root.
- tailscaled no longer inherits the plugin's `TS_AUTHKEY`. tailscaled does
  not use it, and its peerapi serves its environment to peers that the
  control server grants debug access.
- Tailnet ranges are unreachable in the container's main routing table, so
  tailnet traffic leaves through the container's own tailscaled or not at
  all, never through the host's Tailscale.
- The auth key is passed to `tailscale up` on stdin instead of its command
  line, where it was visible in the host's process list.
- tailscaled no longer reaches the host's D-Bus system bus, which the plugin
  sees in the host's `/run`. When tailscaled fell back to the plugin's
  `resolv.conf` (see Fixed), it configured the host's systemd-resolved, as
  root, with the control server's DNS settings, for the host link with the
  index its `tailscale0` has in the container: often the host's primary
  interface. tailscaled does not start if the bus cannot be hidden.

[Unreleased]: https://github.com/matchory/tslink/compare/v0.1.0...HEAD
