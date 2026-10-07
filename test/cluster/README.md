# Cluster tests

Scripts that install tslink on a Docker Swarm from a Git ref or the working tree and test it against
a real tailnet, including Tailscale Services. They work on any Swarm whose nodes this machine
reaches over SSH; `provision.sh` can build a throwaway one on Hetzner Cloud. Results:
[docs/testing/swarm-cluster-test-2026-10-07.md](../../docs/testing/swarm-cluster-test-2026-10-07.md) and
[docs/testing/swarm-cluster-followup-2026-10-07.md](../../docs/testing/swarm-cluster-followup-2026-10-07.md).

## Prerequisites

- A Docker Swarm of three or more Linux nodes with `curl` and `jq`, reached as root over SSH. An
  SSH config lists them: each `Host` entry without wildcards is a node (`mgr`, `w1`, `w2`, ...),
  and every helper loops over them. `mgr` is a manager: `install-plugin.sh` builds the plugin
  there, and it runs stack and service commands, unless `MGR` names another node. The first node
  other than that is the worker `regress.sh` drains.
- On every node, Tailscale's own `tailscaled`, logged in to the same tailnet and tagged
  `tag:tslink-test-node`, so a task that leaked through its host would reach the callee as the
  node instead of failing.
- A Tailscale tailnet whose policy has the tags `tag:tslink-test-caller`, `-callee`, `-nogrant` and
  `-node`, owned by `tag:tslink-test-node` so the API client may mint them; grants caller → callee
  (tcp:80, tcp:5201, icmp), caller → `svc:tslink-test-callee` (tcp:80), node → callee (tcp:80,
  the leak path) and node → node (tcp:5201, icmp); and `autoApprovers.services` letting
  `tag:tslink-test-callee` host `svc:tslink-test-callee` and `svc:tslink-test-other`.
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
| `TSLINK_TEST_SVC` | `svc:tslink-test` | Service prefix: `-callee`, `-other` |
| `TSLINK_TEST_API` | `https://api.tailscale.com/api/v2` | Tailscale API |
| `TSLINK_TEST_TAILNET` | `-` | tailnet; `-` is the API client's |
| `TSLINK_TEST_POWER` | `hetzner-power` | power actions for `disrupt` |
| `MGR` | `mgr` | the manager that builds the plugin and runs stack and service commands |

`deploy` passes `TSLINK_TEST_TAG` and `TSLINK_TEST_SVC` to `docker stack deploy`, and the stacks
default to the values above. It also passes two settings of single stacks:

- `TSLINK_TEST_PIN`: the node hostname the stack pins a task to: `perf` in `callee` and
  `tslink-test-callee` (default `tslink-test-w1`), `cp` (`tslink-test-w2`) and `misc`'s `dup`
  (`tslink-test-mgr`)
- `TSLINK_TEST_VIP`: the address `fan` and `gap` probe; by default `deploy` looks up the VIP of
  `$TSLINK_TEST_SVC-callee`, and the deploy fails without one

The `tslink-test-*` stacks run on the cluster credential (`./deploy tslink-test-callee -`), whose
tag scope requires a stack's tags to start with `tag:<stack>`: with another `TSLINK_TEST_TAG`,
deploy them under the matching stack names. The tests need the Tailscale API and Tailscale
Services, so they do not run against headscale.

## Run

```bash
./install-plugin.sh WT             # or a git ref; installs it as tslink on every node
./deploy callee && ./deploy caller && ./deploy nogrant
./regress.sh --upgrade             # exits non-zero if a check fails
./cleanup.sh                       # delete the test devices, Services and keys
```

`install-plugin.sh` installs the plugin as `tslink` (`tslink:latest`), the name the documented
`docker plugin install --alias tslink` gives it, and the stacks name the driver `tslink:latest`.
A network keeps its driver: to move a Swarm from another plugin name, remove the stacks and that
plugin first.

## Optional: provisioning on Hetzner Cloud

`provision.sh` builds the Swarm on Hetzner Cloud: three `cpx32` servers named `tslink-test-mgr`,
`-w1` and `-w2`, a private network, a firewall that admits SSH from this machine only, and
Docker and Tailscale through `cloud-init.yaml`. It writes the SSH config. It needs `hcloud`, an
API token of a Hetzner Cloud project used only for this in `~/.tslink-test/hcloud.token`, and
`~/.ssh/id_ed25519`.

```bash
./provision.sh                     # servers, network, firewall, swarm; writes ssh_config
./add-node.sh w3 w4                # optional: more workers (promote with docker node promote)
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
| `cleanup.sh` | delete the test devices, Services and the keys created through `tsapi` |
