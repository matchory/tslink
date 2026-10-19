# Cluster tests

Scripts that install tslink on a Docker Swarm from a Git ref or the working tree and test it against
a real tailnet, including Tailscale Services. They work on any Swarm whose nodes this machine
reaches over SSH; `provision.sh` can build a throwaway one on Hetzner Cloud. How they fit with
the other tests: [docs/testing.md](../../docs/testing.md).

## Prerequisites

- A Docker Swarm of three or more Linux nodes with `curl` and `jq`, reached as root over SSH. An
  SSH config lists them: each `Host` entry without wildcards is a node (`mgr`, `w1`, `w2`, ...),
  and every helper loops over them. `mgr` is a manager: `install-plugin.sh` builds the plugin
  there, and it runs stack and service commands, unless `MGR` names another node. The first node
  other than that is the worker `regress.sh` drains.
- On every node, Tailscale's own `tailscaled`, logged in to the same tailnet and tagged
  `tag:tslink-test-node`, so a task that leaked through its host would reach the callee as the
  node instead of failing.
- A Tailscale tailnet with HTTPS certificates on, whose policy has:
  - the tags `tag:tslink-test-caller`, `-callee`, `-nogrant` and `-node`, owned by
    `tag:tslink-test-node` so the API client may mint them
  - grants caller → callee (tcp:80, tcp:5201, icmp), caller → caller (icmp, for R17's pings),
    caller → `svc:tslink-test-callee` (tcp:80, tcp:443), caller → `svc:tslink-test-other`
    (tcp:80), node → callee (tcp:80, the leak path) and node → node (tcp:5201, icmp)
  - `autoApprovers.services` letting `tag:tslink-test-callee` host `svc:tslink-test-callee` and
    `svc:tslink-test-other`; `svc:tslink-test-mixed` needs none, tslink must refuse to host it.
    No autoApprovers for `svc:tslink-test-manual`: R10 approves and revokes its host through the
    API, and revoking does not undo an approval autoApprovers gave
  - the Services `svc:tslink-test-callee` with the ports tcp:80 and tcp:443, and
    `svc:tslink-test-other` with tcp:80; R10 creates `svc:tslink-test-manual` (tcp:80) if it is
    missing
  - optional, for the red team's `test/security/redteam/G5-canaries.sh` to read the task nodes'
    peerapi `/v0/env`: a grant node → callee with `"ip": ["tcp:*"]` and
    `"app": {"https://tailscale.com/cap/debug-peer": [{}]}`, and `nodeAttrs` giving
    `tag:tslink-test-callee` `"https://tailscale.com/cap/debug"`; without them that probe reports
    INFO, saying it did not check `/v0/env`
- On every node, the cluster credential: `oauth-client.secret` in the plugin's data directory
  (`/var/lib/docker-plugins/tailscale`), an OAuth client that may mint `tag:tslink-test-callee`,
  for the `tslink-test-*` stacks (`regress.sh` R11 and R12).
- In `~/.tslink-test/` (mode 600, never on a command line):
  - `tailscale-api.secret`: OAuth client with `auth_keys`, `oauth_keys`, `devices:core` and
    `services` write scopes and the test tags
  - `caller.secret`, `callee.secret`, `nogrant.secret`: OAuth clients for the stacks, tagged
    `tag:tslink-test-caller`, `-callee` and `-nogrant`
  - `node.authkey`: auth key for the host `tailscaled`s, tagged `tag:tslink-test-node`

## Configuration

`config.sh` holds the settings; set any of them in the environment to override it.

| Variable | Default | Is |
| --- | --- | --- |
| `TSLINK_TEST_SSH_CONFIG` | `test/cluster/ssh_config` | SSH config whose hosts are the nodes |
| `TSLINK_TEST_DIR` | `~/.tslink-test` | credentials, API token cache, SSH state |
| `TSLINK_TEST_TAG` | `tag:tslink-test` | tag prefix: `-caller`, `-callee`, `-nogrant`, `-node` |
| `TSLINK_TEST_SVC` | `svc:tslink-test` | Service prefix: `-callee`, `-other`, `-mixed`, `-manual` |
| `TSLINK_TEST_SERVICES` | the four of `TSLINK_TEST_SVC` | the Services `cleanup.sh` deletes |
| `TSLINK_TEST_API` | `https://api.tailscale.com/api/v2` | Tailscale API |
| `TSLINK_TEST_TAILNET` | `-` | tailnet; `-` is the API client's |
| `TSLINK_TEST_MTU` | `1450` | MTU of the stacks' tslink networks (suits the Hetzner provisioner) |
| `TSLINK_TEST_POWER` | `hetzner-power` | power actions for `disrupt` |
| `MGR` | `mgr` | the manager that builds the plugin and runs stack and service commands |

`deploy` passes `TSLINK_TEST_TAG`, `TSLINK_TEST_SVC` and `TSLINK_TEST_MTU` to `docker stack deploy`, and the stacks
default to the values above. It also passes three settings of single stacks:

- `TSLINK_TEST_PIN`: the node hostname the stack pins a task to: `perf` in `callee` and
  `tslink-test-callee` (default `tslink-test-w1`), `cp` and `dnsmix` (`tslink-test-w2`) and
  `misc`'s `dup` (`tslink-test-mgr`)
- `TSLINK_TEST_DNS`: the site resolver in `dnsmix`'s `dns:` list (default `172.17.0.1`)
- `TSLINK_TEST_VIP`: the address `fan` and `gap` probe; by default `deploy` looks up the VIP of
  `$TSLINK_TEST_SVC-callee`, and the deploy fails without one

The `tslink-test-*` stacks run on the cluster credential (`./deploy tslink-test-callee -`), whose
tag scope requires a stack's tag to be exactly `tag:<stack>` (unless the plugin sets
`TSLINK_TAG_SCOPE=prefix`): with another `TSLINK_TEST_TAG`, deploy them under the matching stack
names. The tests need the Tailscale API and Tailscale
Services, so they do not run against headscale.

## Run

```bash
./install-plugin.sh WT             # or a git ref; installs it as tslink on every node
./deploy callee && ./deploy caller && ./deploy nogrant
./regress.sh --upgrade             # exits non-zero if a check fails
./regress.sh --list                # the cases R1 to R17; --cases R7,R8 runs only those
./cleanup.sh                       # delete the test devices, Services and keys
```

Let's Encrypt issues at most 5 certificates a week for one name. tslink keeps a Service's certificate
in the host's certificate directory, not in the node's state
([HTTPS certificates](../../docs/explanation/https-certificates.md)), so new ephemeral nodes reuse it.
On fresh hosts, a run issues one certificate for `$TSLINK_TEST_SVC-callee` on each host that runs one
of its backends, and one more in R7's shared variant, which starts with an empty shared directory. The
callee stack's plain-HTTP backends count too (tailscaled fetches certificates for plain HTTP,
tailscale/tailscale#21693), and two of them that start together on one host can each issue one. A run
on five fresh nodes can thus exceed the limit, and runs on new beds in one week add up. Run R7, R8,
R10 and R11 each with a `TSLINK_TEST_SVC` of its own,
whose Services, grants and autoApprovers exist (`deploy` renames the Services in the multi stack's
label keys after it), e.g.
`TSLINK_TEST_SVC=svc:tslink-r7 TSLINK_TEST_SERVICES="svc:tslink-r7-callee svc:tslink-r7-other"
./regress.sh --cases R7`. A Service whose name is over the limit does not start: its replicas wait
for their certificate (`429 ... rateLimited` in `tailscaled.log`). R10 creates its
`<prefix>-manual` Service itself; no autoApprovers may cover it.

R6 and R11 upgrade the plugin one node at a time (`ONE_BY_ONE=1 ./install-plugin.sh`), as the
upgrade guide does: each node's endpoints run again before the next node's plugin stops, which
drains that node's Service backends.

`install-plugin.sh` installs the plugin as `tslink` (`tslink:latest`), the name the documented
`docker plugin install --alias tslink` gives it, and the stacks name the driver `tslink:latest`.
A network keeps its driver: to move a Swarm from another plugin name, remove the stacks and that
plugin first.

## Optional: provisioning on Hetzner Cloud

`provision.sh` builds the Swarm on Hetzner Cloud: five `cpx32` servers named `tslink-test-mgr` and
`-w1` to `-w4`, the nodes of the reference run, a private network, a firewall that admits SSH
from this machine only, and Docker and Tailscale through `cloud-init.yaml`. It writes the SSH
config. It needs `hcloud`, an
API token of a Hetzner Cloud project used only for this in `~/.tslink-test/hcloud.token`, and
`~/.ssh/id_ed25519`.

```bash
./provision.sh                     # servers, network, firewall, swarm; writes ssh_config
./add-node.sh w5                   # optional: more workers (promote with docker node promote)
./teardown.sh                      # cleanup.sh, then delete the servers, network and firewall
```

`provision.sh` does not log in the host `tailscaled`s; `add-node.sh` does, with `node.authkey`.
`hetzner-power` gives `disrupt` its `reset` and `poweroff` actions through the Hetzner API; on
another provider, point `TSLINK_TEST_POWER` at a script with the same arguments.

## Helpers

| Script | Does |
| --- | --- |
| `s <node> <cmd>` | SSH to a node (`mgr`, `w1`, ...) |
| `nodes` | the nodes in the SSH config; every helper loops over them |
| `cx <node> <service> <cmd>` | run a command in the service's task on a node |
| `cxs <node> <service> <script> [args]` | run a local script there |
| `deploy <stack> [secret]` | `docker stack deploy` with the secret from `~/.tslink-test` |
| `tsapi <method> <path> [json]` | Tailscale API with the test OAuth client |
| `health` | every task tailscaled answers and is Running |
| `probes start\|stop\|report`, `failspans` | VIP probes from the callers, five requests a second |
| `probelog <node>` | the probe log of the node's caller, also after its task was replaced |
| `await <service>` | wait for a service update to finish (`MGR=w1` to ask another manager) |
| `aftercase <since>` | health, devices orphaned since `<since>`, nogrant refused everywhere |
| `disrupt <node> <action>` | reboot, restart or kill Docker over SSH, or reset or power off through the provider API, under probes; times recovery |
| `PROBE=probe-retry.sh probes start <vip> <ip>` | probes that retry through the VIP, then against `<ip>` |
| `install-drain.sh [--remove]` | install `deploy/systemd`'s drain drop-in on every node |
| `endpoints.sh` | on a node (`s <node> sh -s <endpoints.sh`): each endpoint's state and advertised Services |
| `lib.sh` | sourced by `regress.sh` and `test/security/tn.sh`: `wait_for`, `eps`, `stack_rm`, `scale` and the like |
| `ready.sh <port> [log]` | in a container: `GET /ready` of the readiness endpoint, once or into a log |
| `shared-dir.sh on\|off` | an NFS directory from `mgr` at `/mnt/tslink` on every node, for `shared.source` |
| `cleanup.sh` | delete the test devices, Services and the keys created through `tsapi` |
