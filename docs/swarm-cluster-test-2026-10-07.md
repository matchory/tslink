# Swarm cluster test, 2026-10-07

Can tslink give replicated Swarm services their own tailnet identity in
production: reachable through a Tailscale Service VIP, calling other tailnet
endpoints as themselves, surviving failures and upgrades without leaking
traffic through the node? The [spike](swarm-spike-2026-10-06.md) answered this
on one node; this test answers it on three, with failure scenarios, against
Matchory's production tailnet.

## Verdict

**Ready for a production pilot, with conditions** (below), once the fixes from
this test are merged. `main` as it was before this test is **not** fit for
production: a node reboot took Docker down for good, and a task's tailscaled
froze at random within minutes on a tailnet of our size.

With the fixes, every case below passes, including the regression run on the
final build (`test/cluster/regress.sh --upgrade`: 19 of 19 checks). Isolation
held in every scenario: no task ever reached the tailnet through its node,
although each node's own tailscaled was granted the callee, so a leak would
have succeeded rather than failed. Nothing found is fundamentally blocked; the
remaining risks are known, bounded, and have operational mitigations.

## Test bed

- Hetzner Cloud project `tslink-test`, fsn1: 3 × cpx32 (4 AMD vCPU, 8 GB,
  amd64), Ubuntu 24.04, kernel 6.8, Docker 29.8.2, Tailscale 1.102.5. One
  manager, two workers; swarm traffic on a private network (MTU 1450), SSH
  from one address only. About €0.17 per hour.
- Each node runs its own tailscaled tagged `tag:tslink-test-node`, granted the
  callee on tcp:80: the production leak path, made observable.
- tslink built from the working tree on the manager and installed on every
  node as `ghcr.io/matchory/tslink:latest`; 15 builds over the day.
- Tailscale: OAuth clients tagged `tag:tslink-test-caller`, `-callee` and
  `-nogrant`, minted through the API; Services `svc:tslink-test-callee` (the
  caller is granted it) and `svc:tslink-test-other` (no grant).
- Stacks: `callee` (3 replicas backing `svc:tslink-test-callee`, one backing
  `svc:tslink-test-other`, an iperf3 server), `caller` (global, overlay +
  tslink, `dns: 100.100.100.100`) and `nogrant` (global, tslink only). Later
  per Matchory's convention: daemon `mtu: 1450`, network MTU 1450.
- The scripts are in [test/cluster](../test/cluster): provisioning, plugin
  install, stacks, probes and the regression suite.

VIP availability was measured with probes in each caller task: an HTTP request
to the VIP every 200 ms, 2 s timeout, logging the answering backend.

## Assumptions

- Production swarms look like the test bed: Hetzner Cloud, amd64, fsn1,
  Docker 29, Ubuntu 24.04, swarm on a private network, nodes with public
  egress. Not tested: more than three nodes, several managers, arm64,
  encrypted overlays, a NAT gateway instead of public addresses.
- Credentials follow [credentials.md](credentials.md): one OAuth client per
  stack, `tslink.tags` on the network, ephemeral nodes.
- Services reachable on the tailnet are exposed as Tailscale Services in HTTP
  mode (`tslink.serve.<port>=http:<port>`, `tslink.direct=false`).
- Applications retry tailnet connections at startup and keep serving a few
  seconds after SIGTERM. Both are verified to matter below.
- Tailscale's control plane and DERP behave as on the test day; the API was
  never rate-limited at our volumes (60 simultaneous registrations).

## Results

| Case | Result | Evidence |
|---|---|---|
| Caller and callee on different nodes | Pass | Callee sees the caller's own tailnet IP from every node |
| Ungranted traffic refused | Pass | nogrant refused on every callee and the VIP, from every node, while each host's tailscaled (node tag) got 200 from the same addresses |
| Ungranted Service VIP refused | Pass | Caller refused on `svc:tslink-test-other`; its name does not resolve |
| DNS, overlay + tslink + quad-100 | Pass | MagicDNS device and Service names, public names, `echo`, `tasks.echo` on all nodes |
| DNS, tslink only | Pass after fix | Public names failed (SERVFAIL); tslink now lets Docker attach its gateway |
| Replicas behind one VIP, spread over nodes | Pass | Each caller sticks to one backend; `X-Forwarded-For` carries the caller's tailnet IP |
| Rolling update, start-first and stop-first | Pass after fix | 1 failed request out of ~6,000 over 6 updates; 0 in the final run |
| Node drain and reactivation | Pass after fix | 0 failed requests, no orphaned devices |
| Hard node failure | Pass after fixes | VIP callers recover in 17 s; node rejoins on its own; leftovers cleaned |
| Plugin upgrade with tasks running | Pass after fixes | Identities kept; node's tasks off the tailnet ~3.5 s |
| Docker daemon restart | Pass, with risk | Back in 15 s; callers pinned to that node's backends fail 6–11 s |
| Graceful reboot | Pass after fix | Node Ready in 33 s; before the fix, Docker never came back |
| Scale to 30 and 60 replicas at once | Pass | All online 4 s after the scale command, no retries |
| Scale 60 → 0 | Pass | All 60 devices removed within 17 s |
| Secret rotation (recreate network) | Pass | 14 s outage on the VIP |
| Invalid secret | Pass | Fails closed, retries with backoff, error in log and status file |
| Revoked secret | Pass after fix | Running tasks unaffected, also across a plugin restart |
| Wrong tag for the secret | Pass | Rejected by Tailscale, task fails closed, retries |
| `ephemeral=false` | Pass | State and device kept; redeploy on the same node reuses the identity |
| Control plane unreachable at task start | Pass after fix | No identity until it is back, then 3 s; running tasks unaffected |
| UDP blocked, DERP only | Pass | Works over DERP; back to direct within 20 s of unblocking |
| Startup gap | Pass | First tailnet success 1.7 s after start; failures before are immediate "host unreachable"; nogrant 0 of 300 succeeded |
| Two replicas, same `tslink.hostname`, one node | Pass after fix | Shared one state file; now the second waits without an identity |
| Invalid hostname label | Pass | Refused permanently, fails closed |
| Task from another stack on a stack network | Pass | Refused: stack mismatch |
| Plain container on a stack network | Pass | Docker refuses: not manually attachable |
| `tslink.tags` on network and container | Pass | Network wins; warning logged |
| Hosting a Tailscale Service | Pass | Backends auto-approved via `autoApprovers`, VIP serves from all replicas |
| Device deleted while running | Pass after fix | Logs in again as a new device within 13 s, Service backend restored |
| No secret in any log | Pass | 0 matches for any secret in plugin data, journal, syslog on all nodes |
| `tslink diag`, `scripts/tslink-diag.sh` | Pass after fix | Now also lists containers without Tailscale and why |
| MTU 1450 | Pass after fix | Network MTU option now applied to tslink's interface |

## Measurements

| What | tslink | Baseline |
|---|---|---|
| Latency, task to task across nodes, direct | 1.1 ms | 0.7–2.2 ms host tailscaled |
| Latency over DERP (fra/nue) | 16.6 ms | |
| Throughput, 1 / 4 streams, default | 1.16 / 1.18 Gbit/s | 2.78 / 2.80 Gbit/s host tailscaled |
| Throughput with UDP GRO forwarding on the uplink | 2.75 / 2.86 Gbit/s | same |
| Throughput over DERP | 25 Mbit/s | |
| Raw private network, no Tailscale | | 7.9 / 16.3 Gbit/s |
| tailscaled CPU at ~2.8 Gbit/s | 171% of a core | 168% host tailscaled |
| tailscaled memory, idle / after load | 41 MB / 273 MB RSS | host tailscaled 193 MB |
| tailscaled CPU, idle | 0.3–0.4% | |
| Join to tailnet IP, one task / 60 at once | 1.5–2 s / 1.4–2.0 s | |
| Device removal after a task stops | immediate (logout) | |
| Orphaned ephemeral device after power loss | removed by Tailscale within about an hour | |

| Disruption | Duration |
|---|---|
| Rolling update, app keeps serving 5 s after SIGTERM | 0–1 failed requests per update |
| Rolling update, app exits on SIGTERM | 1–8 failed requests (0.2–1.5 s) per caller |
| Plugin upgrade, egress of the node's tasks | 3.5 s (7.5 s before the fixes) |
| Docker daemon restart, callers pinned to its backends | 6–11 s |
| Graceful reboot without drain, same | 7 s |
| Hard node failure, same | 17 s |
| Secret rotation | 14 s |

## Bugs found

All reproduced on the cluster and fixed with tests on the branch, except where
noted.

1. **tailscaled froze at random.** Its output was read with `bufio.Scanner`,
   which stops at a 64 KiB line; tailscaled then blocked on its full stderr
   pipe, and with it everything else. On a tailnet of 134 devices this
   happened about 100 times per node within an hour (`RAW-STDERR` lines, a
   thread in `pipe_write`). Fix: read the pipes whatever the line length, cut
   long lines; tailscaled now logs to its own rotated `tailscaled.log`.
2. **A rebooted node lost Docker.** The plugin bind-mounted
   `/var/run/docker/netns`, which does not exist on boot until Docker creates
   a network namespace, after it enables plugins. The plugin failed to start,
   and dockerd then panicked on a nil plugin client (a moby bug) and kept
   restarting. Fix: drop the mount; the `/run` mount covers it. Workaround for
   nodes with the old plugin: `mkdir -p /run/docker/netns` before Docker
   starts. The dockerd panic itself should be reported upstream.
3. **A failed first start was never retried**: a bad secret, an unreachable
   control plane or a blip left the task running without an identity for good,
   and a crash loop stopped the supervisor permanently. Fix: retry with backoff
   until the endpoint leaves; resume after a crash-loop cooldown.
4. **Devices were orphaned on stop.** A task on an overlay network sent
   tailscaled's traffic through `docker_gwbridge`; Docker detached it before
   tslink's `Leave`, and the logout failed. Fix: tailscaled's own traffic (its
   bypass mark) routes through tslink's veth by policy; the logout is retried.
5. **A plugin restart needed the secret.** Recovery ran `tailscale up` with
   the OAuth secret, which the CLI exchanges for an auth key before looking at
   the node: every restart minted keys through the API, and with a revoked or
   rotated secret the tasks lost their identity. Fix: a node still logged in
   comes up without the key.
6. **A deleted or expired device never came back.** tailscaled keeps running
   (state `Running`, logging `404: node not found`). Fix: the supervisor sees
   that line and logs in again with `--force-reauth`.
7. **Tasks on tslink alone could not resolve public names** (seen in the
   spike). Docker's DNS server refuses to forward for a container without a
   gateway. Fix: tslink no longer disables the gateway; with the bypass route
   and the tailnet blackhole, Docker's default route is safe.
8. **Rolling updates dropped VIP callers** until their netmap caught up. Fix:
   drain a task's Service backend on its stop signal and on plugin shutdown;
   advertise it again on start.
9. **Leftovers after power loss**: state directories and sockets of tasks that
   never left. Fix: garbage collection after recovery, of ephemeral state
   (marked as such), sockets and status files no endpoint uses. Its first
   version raced recovery and deleted two live identities; fixed by claiming
   state directories before it runs.
10. **Recovered endpoints leaked their veth and NAT rules** on stop, and kept
    whatever routes an older version had installed. Fix: recovery records the
    veth and reapplies the routes, so upgrades retrofit them.
11. **Two replicas with one `tslink.hostname` on a node shared a node key.**
    Fix: a state directory serves one endpoint at a time; the second waits.
12. **Unbounded logs**: all tailscaled output went to `plugin.log` and the
    Docker journal, about 1 MB per 25 minutes for four tasks. Fix: per-endpoint
    logs rotated at 10 MB, plugin log at 50 MB.
13. **Version drift**: each node downloaded the latest Tailscale when its
    plugin started, and could not start tasks without pkgs.tailscale.com.
    Fix: the image ships a pinned Tailscale (`TS_VERSION=bundled`), updated by
    Dependabot through the Dockerfile.
14. **`com.docker.network.driver.mtu` was ignored**; the veth was always 1500.
15. **The auth key was on `tailscale up`'s command line**, visible in the
    host's process list. Now on stdin.
16. **No way to see a task without an identity**: Swarm reports it Running.
    Fix: status files and `tslink diag` (below).

## Changes to tslink

On branch `swarm-cluster-test`, all with unit tests; the network namespace
tests ran as root on a test node. `go test ./...` and `golangci-lint` (Linux
and macOS) pass.

- Retry loop and permanent errors (`core/endpoint.go`), crash-loop cooldown,
  login watchdog (`tailscale/supervisor.go`).
- Bypass route for tailscaled's own traffic (`netutil/bypass.go`), gateway
  left to Docker, routes reapplied on recovery.
- Drain on stop signal and plugin shutdown, advertise on start.
- `bringUp` without the key for logged-in nodes; logout retries.
- Pipe draining, rotated logs (`tailscale/lines.go`, `logger/rotate.go`).
- Garbage collection, state directory claims, status files
  (`core/gc.go`, `core/status.go`), `tslink diag` reports them.
- Plugin config: no netns mount, `TS_VERSION=bundled`; Dockerfile ships
  Tailscale 1.102.5; MTU option.
- `test/cluster`: the test bed and regression suite.

## Open risks

The [follow-up](swarm-cluster-followup-2026-10-07.md) tested manager failover
and measured or closed these risks: Docker restarts and reboots are drained by
a systemd drop-in, TCP backends can receive the caller's address, and hard
node failure turned out to cost 21-48 s rather than 17 s.

- **Unplanned Docker stops** (daemon restart, reboot without drain): Docker
  sends no events once it shuts down, so tslink cannot drain; callers pinned to
  that node's backends fail for the stop timeout (6–11 s measured). Mitigation:
  drain nodes before maintenance.
- **Hard node failure**: callers pinned to the dead node's backends fail until
  Tailscale marks it offline (17 s measured). The dead tasks' devices stay
  listed, offline and harmless, for about an hour.
- **Backend selection is per caller and sticky**: a caller sends all its VIP
  traffic to one backend; Tailscale Services do not balance per request.
- **Caller identity behind a VIP**: HTTP mode passes `X-Forwarded-For`; TCP
  mode loses the caller's address. `tailscale serve --proxy-protocol` would
  carry it, but tslink does not expose it yet.
- **Credentials are visible to Docker API holders** (`docker network inspect`,
  the raft store), as designed in [credentials.md](credentials.md).
- **Scale beyond the test**: 60 simultaneous registrations caused no rate
  limiting, but hundreds at once (mass reschedule of a large cluster) were not
  tested; each registration exchanges the OAuth secret through the API.
  Retries with backoff absorb failures, at the cost of time.
- **Observed once**: after a rolling update one caller kept routing to a gone
  backend for about 36 s, apparently a slow netmap from control. Not seen in
  eight further updates.
- **Not tested**: more nodes, manager failover, multi-day soak, arm64,
  encrypted overlays, Tailscale upgrades across versions, Docker upgrades, and
  the release pipeline (GitHub Actions are disabled, so no image was published).

## Conditions for the pilot

1. Merge the branch and publish a release; do not run `main` from before it.
2. Node provisioning: UDP GRO forwarding on the uplink
   (`ethtool -K <iface> rx-udp-gro-forwarding on rx-gro-list off`, persisted),
   and `/var/lib/docker-plugins/tailscale` created before the plugin.
3. Operations: drain a node before rebooting it, restarting Docker or
   upgrading the plugin.
4. Services: ephemeral OAuth secrets; `com.docker.network.driver.mtu: "1450"`
   on tslink networks; no `tslink.hostname` on replicated services;
   applications retry the tailnet at startup and keep serving for a few
   seconds after SIGTERM.
5. Monitoring: alert on `/var/lib/docker-plugins/tailscale/status/*.json`
   whose state is not `running` (e.g. through the node exporter's textfile
   collector).
6. Start with one non-critical stack, and rerun `test/cluster/regress.sh`
   against each new tslink or Tailscale version before rolling it out.
