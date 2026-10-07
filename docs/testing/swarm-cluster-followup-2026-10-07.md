# Swarm cluster follow-up, 2026-10-07

The [first cluster test](swarm-cluster-test-2026-10-07.md) left open risks
and could not test manager failover. This follow-up closes or measures them on
a five-node swarm with three managers: manager failover and quorum loss,
unplanned Docker stops, hard node failure, backend selection behind a
Tailscale Service VIP, and the caller's identity behind it.

## Verdict

**Still ready for a production pilot, with updated conditions** (below).
Manager failover, manager reboots and loss of quorum do not affect running
tasks' identities or traffic. Unplanned Docker restarts and reboots no longer
cost callers anything once the new systemd drop-in is installed. Backends can
now learn the caller's address in TCP mode and its app capabilities in HTTP
mode.

Two findings make the picture worse than the first report said. A hard node
failure costs callers pinned to its backends **21 to 48 seconds**, not 17, and
it can hit callers that were not pinned to the dead node before. Every
unplanned stop that skips tslink's `Leave` (power loss, hard reset, dockerd
crash) leaves offline devices in the tailnet for about an hour. With a fixed
`tslink.hostname` and ephemeral nodes, the restarted task then comes back
under a different MagicDNS name. Neither is a blocker for a pilot, but both
shape how services should be deployed.

Isolation held in every case: no nogrant task reached the callee, the VIP or
anything else, although every host's tailscaled was granted the callee.

## Test bed

As in the first test (Hetzner Cloud project `tslink-test`, fsn1, cpx32,
Ubuntu 24.04, Docker 29.8.2, Tailscale 1.102.5 bundled), grown to five nodes:
`mgr`, `w1` and `w2` managers, `w3` and `w4` workers, added with
`test/cluster/add-node.sh` and the same cloud-init, firewall and private
network. Daemon MTU 1450 and UDP GRO forwarding are now set by cloud-init.

Stacks `callee` (three replicas backing `svc:tslink-test-callee`), `caller`
and `nogrant` (global, so one task per node) as before. VIP availability was
measured with probes in each caller task (one HTTP request every 200 ms, 2 s
timeout) and `test/cluster/disrupt`, which disrupts one node, times its
recovery and runs the per-case checks (`aftercase`): every task tailscaled
`Running`, no ephemeral test device gone offline since the case started, and
nogrant refused from every node.

A "pinned caller" below is a caller whose VIP traffic went to a backend on the
disrupted node: callers stick to one backend, so only they can see failures.

## Results

| Case | Result | Evidence |
| --- | --- | --- |
| Leader powered off during a rolling update | Pass | w1 leader within ~20 s; the update completed on the new leader; callers not pinned to the dead node 0 failed requests |
| Old leader powered on again | Pass | Ready and Reachable 47 s after poweron, new global tasks on the tailnet |
| New stack deployed after failover | Pass | New leader created the network; the task had its identity in ~4 s and reached the VIP |
| Graceful reboot of the leader, no Swarm drain | Pass | Leadership moved; tasks on the tailnet 51 s after the reboot; pinned callers failed 16 s (no drop-in yet) |
| Hard reset of a manager | Pass | Tasks back 42 s after the reset |
| Quorum lost (two of three managers off, 7 min) | Pass | 0 failed VIP requests; Swarm refused `node ls` and `service scale` ("no leader"); quorum back 48 s after poweron, all replicas restored |
| `systemctl restart docker`, with drain drop-in | Pass | 0 failed requests for 7 pinned callers (without: 0.2, 15.2, 8.8 s) |
| `systemctl reboot`, with drain drop-in | Pass | 1 failed request for 11 pinned callers (without: 16, 7.3, 6.9 s) |
| dockerd `kill -9` | Not covered, harmless for the VIP | systemd skips ExecStop; 0 failed requests in 3 runs; 3-4 orphaned devices per crash |
| Hard node failure, caller retries through the VIP | Fail, as expected | The retry goes to the same dead backend; pinned callers failed 21-48 s |
| Hard node failure, caller falls back to another backend's address | Pass | 0 failed requests for every caller |
| 30 callers, 3 healthy backends | Pass | Spread 16/3/11, later 9/11/10; one simultaneous switch of 21 callers, no failures |
| `?proxy-protocol=1` and `=2` on a TCP Service backend | Pass | nginx behind the VIP saw each caller's own tailnet address; invalid value fails closed with a status `failed` |
| `?accept-app-caps=` on an HTTP Service backend | Pass | `Tailscale-App-Capabilities` carried the granted capability; forged headers replaced |
| GC after a crash shortly after a task started | Pass after fix | Dead sockets and a status file still saying `running` were kept until the next plugin restart; now removed within two watchdog scans |
| `regress.sh --upgrade`, final build, five nodes | Pass | 28 of 28 checks, twice |

## Measurements

| What | Value |
| --- | --- |
| Leader election after the leader lost power | ≤ 21 s |
| Manager Ready again after poweron | 47-48 s |
| Node's tasks back on the tailnet after a graceful reboot | 32-53 s |
| Same after `systemctl restart docker` | 14-49 s |
| Propagation of `tailscale serve drain` to callers | ≤ 1.3 s |
| Control marks a powered-off device offline (`lastSeen`) | 25-78 s after poweroff |
| Swarm reschedules a dead node's task, new backend registered | +24 s (heartbeat 5 s) |
| Tailscale deletes an offline ephemeral device | ~60-75 min after `lastSeen` |

| Disruption, pinned callers | Before | After |
| --- | --- | --- |
| Docker restart | 0.2-15.2 s | 0 failed requests (drain drop-in) |
| Graceful reboot | 6.9-16 s | 0-1 failed requests (drain drop-in) |
| dockerd crash | 0 failed requests | not changed |
| Hard node failure | 21-48 s | 0 with a client-side fallback; no fix in tslink |

## Findings

### Manager failover

Swarm's raft store holds the network options, credential included, so a new
leader allocates tslink networks and starts tasks without any help from
tslink. tslink talks only to the local Docker API: losing quorum does not
touch running tasks, their tailscaleds or their traffic. Tasks cannot be
rescheduled without quorum, which is Swarm's limit, not tslink's.

### Unplanned Docker stops

`deploy/systemd/tslink-drain.conf` adds `ExecStop=/usr/local/sbin/tslink-drain`
to `docker.service`. systemd runs it before it sends dockerd SIGTERM: the
script drains every Tailscale Service the node backs, through each tslink
socket and the plugin's bundled `tailscale` CLI, and waits 3 seconds. Callers
moved within 1.3 s of a drain in every test. Installation is in the
[readme](../../README.md#running-on-swarm).

A dockerd crash skips ExecStop. It turned out harmless for the VIP: the
containers keep serving while dockerd is gone, and the restarted dockerd stops
the old plugin, whose shutdown drains every backend before the containers are
killed. It leaves orphaned devices (below).

### Hard node failure

A caller's netmap gives the VIP to exactly one backend: the VIP's address
appears in that peer's `AllowedIPs` only. A caller therefore keeps sending to
a dead backend until control marks it offline and pushes a new netmap, 25 to
78 seconds after the power is cut in this test (the first test saw 17 s).
Retrying through the VIP does not help. Tailscale documents stable
pseudorandom backend preferences per client and, for app connectors, failover
"for more than 15 seconds" disconnected from control; it offers no health
check or setting for Services.

Worse, the outage is not confined to the callers pinned to the dead node. When
Swarm's replacement task registered as a new backend (+24 s), control
reassigned callers; one healthy caller was moved onto the dead backend and
failed from +28 s to +53 s.

What shortens it:

- **A client-side fallback**: on failure, retry against another backend's own
  tailnet address. It removed every failure, but needs a grant on the backend
  devices and a way to know them (their hostnames here); Tailscale Services
  publish neither.
- **A controller** watching Swarm for down nodes and deleting their tasks'
  devices through the Tailscale API would cut the outage to Swarm's detection
  time (about 20 s). It needs an API credential in the cluster; not built.
- Swarm's heartbeat (`docker swarm update --dispatcher-heartbeat`) only moves
  the replacement task earlier, which in this test made things worse.

### Orphaned devices

Whenever a task dies without tslink's `Leave` (power loss, hard reset, dockerd
crash), its ephemeral device stays in the tailnet, offline, until Tailscale
deletes it about an hour later. tslink's garbage collection deletes the local
state but cannot log the node out. Offline devices take no traffic, but:

- **A fixed `tslink.hostname` changes its MagicDNS name.** After w1 lost
  power, the `callee-perf` task came back 11 s after GC removed its state and
  registered as a new device, `callee-perf-1`, with a new address, because the
  orphan still held `callee-perf`. Reproduction: an ephemeral network, a
  service with `tslink.hostname` pinned to one node, power that node off until
  control marks the device offline, power it on.
- Devices pile up: 3-4 per crashed node, 7 for two nodes powered off.

Proposed fix: before deleting ephemeral state, GC starts a temporary
tailscaled with it (userspace networking, its own socket) and logs the node
out, which deletes the device and frees the name. Not built: the dead node
briefly comes online with its Service configuration, so it must first clear
its advertised Services from the state, which needs care and its own tests.
Workaround: `ephemeral=false` for services with a fixed hostname, which reuses
the identity on the same node.

### Backend selection

30 callers on five nodes spread 16/3/11 over three healthy backends, each
sticking to one. At one moment 21 of them switched backend together, with no
failed request, to 9/11/10; it coincided with Tailscale deleting the offline
ephemeral backends of earlier cases, which changes the Service's host set.
Tailscale Services choose per client, not per request. Per-request balancing
needs a proxy: for example a few proxy replicas as the Service backends,
balancing over the overlay network to the application tasks, passing on the
caller's identity in a header.

### Caller identity

- **TCP:** `tslink.serve.<port>=tcp:<target>?proxy-protocol=1|2` (also
  `tls-terminated-tcp`) makes Tailscale send a PROXY protocol header; nginx
  behind the VIP saw each caller's tailnet address with both versions, and
  `127.0.0.1` as the TCP peer.
- **HTTP:** `X-Forwarded-For` already carries the address. Tagged callers get
  no `Tailscale-User-*` headers, so a backend can authorize another workload
  only through app capabilities:
  `tslink.serve.<port>=http:<target>?accept-app-caps=<cap>[,<cap>]` forwards
  the caller's grants as JSON in `Tailscale-App-Capabilities`. A caller that
  sent its own `Tailscale-App-Capabilities` and `X-Forwarded-For` had both
  replaced. Tested with grants on both the Service and the hosting tag, so
  which of the two serve reads is not established.

## Changes to tslink

On branch `swarm-cluster-test`, with unit tests; `go test ./...`,
`golangci-lint` (Linux and macOS) and the netutil namespace tests as root on a
test node pass.

- Garbage collection after every watchdog scan, skipping claimed state
  (`core/gc.go`, `docker/driver.go`).
- `?proxy-protocol=` and `?accept-app-caps=` options on `tslink.serve`
  labels (`docker/events.go`, `tailscale/daemon.go`).
- `deploy/systemd`: the drain script and `docker.service` drop-in.
- `test/cluster`: any number of nodes, `add-node.sh`, `disrupt`, `aftercase`,
  retrying probes, `install-drain.sh`, test stacks; `regress.sh` runs on
  macOS's Bash 3.2 again, and `install-plugin.sh` copies builds to every node.

## Remaining risks

- **Hard node failure**: 21-48 s for affected callers, including callers
  re-pinned to the dead backend; only a client-side fallback or an
  API-credentialed controller shortens it.
- **Orphaned devices** after unplanned stops, and the renaming of fixed
  hostnames with ephemeral nodes, until the proposed GC logout exists.
- **dockerd crashes** are not drained; harmless in the test only because the
  restarted dockerd stops the plugin before the containers.
- **Backend balance** is per caller and can be uneven (16/3/11).
- From the first test and still untested: hundreds of simultaneous
  registrations, multi-day soak, arm64, encrypted overlays, Tailscale and
  Docker upgrades, the release pipeline.

## Conditions for the pilot

The [first report's conditions](swarm-cluster-test-2026-10-07.md#conditions-for-the-pilot),
updated:

1. Merge the branch and publish a release.
2. Node provisioning: UDP GRO forwarding on the uplink,
   `/var/lib/docker-plugins/tailscale` created before the plugin, and the
   `deploy/systemd` drain drop-in on every node.
3. Operations: drain a node in Swarm before planned maintenance; the drop-in
   covers restarts and reboots that happen without it.
4. Services: ephemeral OAuth secrets, but `ephemeral=false` for services with
   a fixed `tslink.hostname`; network MTU 1450; applications retry the tailnet
   at startup, keep serving a few seconds after SIGTERM, and, where 30-60 s of
   errors after a node failure are not acceptable, fall back to another
   backend or sit behind a proxy tier.
5. Monitoring: alert on status files whose state is not `running`, and on the
   number of offline ephemeral devices per tag.
6. Start with one non-critical stack; rerun `test/cluster/regress.sh` against
   each new tslink or Tailscale version.
