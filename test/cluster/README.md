# Cluster tests

Scripts that build a throwaway Swarm on Hetzner Cloud (three nodes, grown with `add-node.sh`),
install tslink from a Git ref or the working tree, and test it against a real tailnet. Results:
[docs/swarm-cluster-test-2026-10-07.md](../../docs/swarm-cluster-test-2026-10-07.md) and
[docs/swarm-cluster-followup-2026-10-07.md](../../docs/swarm-cluster-followup-2026-10-07.md).

## Prerequisites

In `~/.tslink-test/` (mode 600, never on a command line):

- `hcloud.token`: API token of a Hetzner Cloud project used only for this
- `tailscale-api.secret`: OAuth client with `auth_keys`, `oauth_keys`, `devices:core` and
  `services` write scopes and the test tags
- `caller.secret`, `callee.secret`, `nogrant.secret`: OAuth clients for the stacks, tagged
  `tag:tslink-test-caller`, `-callee` and `-nogrant`; `node.authkey` for the hosts

The tailnet policy needs those tags, owned by `tag:tslink-test-node` so the API client may mint
them; grants caller → callee (tcp:80, tcp:5201, icmp), caller → `svc:tslink-test-callee`
(tcp:80), node → callee (tcp:80, the leak path) and node → node (tcp:5201, icmp); and
`autoApprovers.services` letting `tag:tslink-test-callee` host `svc:tslink-test-callee` and
`svc:tslink-test-other`.

## Run

```bash
./provision.sh                     # servers, network, firewall, swarm; writes ssh_config
./add-node.sh w3 w4                # optional: more workers (promote with docker node promote)
./install-plugin.sh WT             # or a git ref; replaces the plugin on every node
./deploy callee && ./deploy caller && ./deploy nogrant
./regress.sh --upgrade             # exits non-zero if a check fails
./teardown.sh
```

The host tailscaleds join with `node.authkey` and `--advertise-tags=tag:tslink-test-node`, so a
task that leaked through its host would reach the callee as the node instead of failing.

## Helpers

| Script | Does |
| --- | --- |
| `s <node> <cmd>` | SSH to a node (`mgr`, `w1`, ...) |
| `nodes` | the nodes in `ssh_config`; every helper loops over them |
| `cx <node> <service> <cmd>` | run a command in the service's task on a node |
| `cxs <node> <service> <script> [args]` | run a local script there |
| `deploy <stack> [secret]` | `docker stack deploy` with the secret from `~/.tslink-test` |
| `tsapi <method> <path> [json]` | Tailscale API with the test OAuth client |
| `health` | every task tailscaled answers and is Running |
| `probes start\|stop\|report`, `failspans` | VIP probes from the callers, five requests a second |
| `await <service>` | wait for a service update to finish (`MGR=w1` to ask another manager) |
| `aftercase <since>` | health, devices orphaned since `<since>`, nogrant refused everywhere |
| `disrupt <node> <action>` | reboot, reset, power off, restart or kill Docker under probes; times recovery |
| `PROBE=probe-retry.sh probes start <vip> <ip>` | probes that retry through the VIP, then against `<ip>` |
| `install-drain.sh [--remove]` | install `deploy/systemd`'s drain drop-in on every node |
