# Testing

tslink has tests at four levels: unit tests that run on all platforms, network namespace tests, an end-to-end test,
and cluster tests on a Swarm with a real tailnet. [CONTRIBUTING.md](../CONTRIBUTING.md#tests) tells how to run them.

## Unit tests

`go test -race ./...` tests the driver, the endpoint and network logic, the tailscaled supervisor, the state
directories and the garbage collection. Fakes replace tailscaled, Docker and the network. The tests run on all
platforms, and in CI on each pull request.

`pkg/preflight` checks the environment properties of [SECURITY.md](../SECURITY.md) for `tslink diag --preflight`.
Its tests use a fake Docker API.

## Security model

Each test that verifies a guarantee of [SECURITY.md](../SECURITY.md) has the ID of the guarantee:

| Test type | Marker |
| --- | --- |
| Go test | `// Guards: G1` in the doc comment |
| TLA+ model | `\* Guards: G1` |
| Shell probe | `probe G1 ...` |
| Red-team probe | `# Guards: G1` in its header; a probe of no guarantee has no `Guards:` line |

With each `go test`, `internal/secmodel` checks that each published guarantee has at least one test. A guarantee
with a `Manual:` line is exempt: its check is done by hand before each release.

A shell probe is an attack. It runs through `test/security/lib.sh` two times:

1. Against the defended system, where the attack must fail.
2. In a control run without the defence, where the attack must succeed.

If the control run fails, the library reports the probe as broken, because the probe cannot find a regression.
`test/security/lib_test.sh` tests the library itself, in CI on each pull request.

## Network namespace tests

The tests in `pkg/netutil` create network namespaces, veth pairs, routes and rules. They need Linux and
`CAP_NET_ADMIN`, and skip without them. CI runs them a second time as root.

## In CI

- Test failures show as annotations on the pull request.
- The summary of each run shows the coverage of each package, merged from both runs. The `coverage` artifact keeps
  the profile and an HTML report. Coverage has no threshold.
- Lint and govulncheck findings go to GitHub code scanning.

## End-to-end test

`test/integration/run.sh` builds the plugin, installs it, and runs containers against a
[headscale](https://github.com/juanfont/headscale) control server on the same machine. It needs no Tailscale
account. It checks these properties:

- Containers reach each other over the tailnet.
- The policy refuses connections that it does not permit.
- A container keeps its identity after a plugin restart.
- An ephemeral node is removed with its container.
- The host isolation holds. These checks are probes on `test/security/lib.sh`, with control runs.
- No auth key that the test creates appears in the data directory of the plugin, in the command lines and the
  environments of the plugin and of tailscaled, in the output of `tslink diag`, or in the containers. The test plants
  a key of its own in each place, as the control run for that place.

CI runs the test on each pull request. It does not test Tailscale Services, because headscale does not support them.

## Cluster tests

`test/cluster` installs tslink on a Docker Swarm of three or more nodes, and tests it against a real tailnet,
with Tailscale Services. See [test/cluster/README.md](../test/cluster/README.md). Its regression suite, `regress.sh`,
checks these properties, as the cases R1 to R17 (`regress.sh --list` prints them):

1. The tailscaled of each task runs.
2. A task reaches only what the policy permits, and no traffic leaves through the Tailscale of the host.
3. DNS works for tasks on an overlay network and on tslink.
4. A rolling update of a Service backend fails at most 3 requests per caller.
5. When a node is drained and made active again, no orphaned devices stay in the tailnet.
6. A plugin upgrade keeps the identity and the advertised Services of each node, fails at most 3 requests per caller,
   and `/ready` answers nothing but 200 while the plugin runs.
7. A task can host several named Services: each answers, the HTTPS one with its own certificate, and every replica is
   approved. With a shared certificate directory, one replica issues the certificate, and the others advertise the
   Service only once it exists.
8. Removing a Service's label drains it: the new tasks do not advertise it, control fetches the drain before an old
   node is logged out, and the other Service fails no request.
9. Labels that conflict, legacy and named Service labels or a named one with `tslink.direct=true`, fail the endpoint
   with an error that names them, and no device comes up. Legacy labels cleared to `""` do not conflict.
10. `/ready` reports each Service. A host of a Service that no autoApprovers approve answers 503
    `awaiting-approval` until it is approved through the API, then 200, and stays 200 when the approval is revoked,
    listing the Service as `awaiting-approval`. `/ready` is 200 within 1 s after a plugin restart, and answers 503
    `drained` while the task stops.
11. An upgrade from v0.3.2 keeps what 6 checks, with legacy HTTPS tasks, and named labels work after it. A
    downgrade, which is not supported, advertises none of the named labels' Services.
12. Each task device has exactly the tag of its network and is ephemeral, and no test key outlives its expiry. With
    the cluster credential, a network without `tslink.tags` is refused, and a tag outside the stack's scope never
    starts tailscaled.
13. A node whose device was deleted comes back as a new device after a restart, and callers reach it.
14. A task with `dns: [100.100.100.100, <resolver>]` resolves tailnet names, the resolver's names and public
    names, and its tailscaled forwards to that resolver alone, whose names a task without `dns:` does not resolve.
    Once Docker's `daemon.json` names the resolver, a task without `dns:` resolves through it.
15. At `TSLINK_LOG_LEVEL=warn`, the plugin logs no info lines.
16. `tslink diag --preflight` finds no violated property on any node, except E6 for a plugin created from a
    directory.
17. Nodes on different hosts reach each other directly over the hosts' network, not through DERP, also after the
    filter table of one host is flushed and the watchdog restored tslink's rules.

`disrupt` does one of these actions to a node, a manager included, while callers send requests to a Tailscale
Service. It then measures the recovery time.

- Reboot the node.
- Restart or kill its Docker daemon.
- Reset the node, or switch it off.

The cluster tests need a Swarm and a tailnet that are set up for them. Thus they do not run in CI.

## Security testbed

The red team runs on a disposable Linux host reached over SSH. [`test/security/testbed/README.md`](../test/security/testbed/README.md)
lists the prerequisites; its scripts can optionally provision such a host on Hetzner Cloud.

[`test/security/redteam.sh`](../test/security/redteam.sh) does these steps:

1. It copies a working tree to the server.
2. It runs `test/integration/run.sh`.
3. It runs the probes in [`test/security/redteam/`](../test/security/redteam), and the probes in a private directory
   that you give with `--probes`.
4. It writes a report in `$TSLINK_TEST_DIR/reports/`. Do not commit the report.

Each probe reports PASS (the attack was blocked), FAIL, BROKEN (its control run did not get through), SKIPPED, or
INFO, with exit code 4: it passed, and its control violates an environment property, so it shows what an operator
gives up who violates it; or it only records a behaviour.

Some probes run against the Swarm bed of `test/cluster` in a Tailscale tailnet instead (`Testbed: tn` in their
header): on the single host they report SKIPPED. Install the commit under test on the bed, then run
[`test/security/redteam-tn.sh`](../test/security/redteam-tn.sh): it runs them from your machine over SSH and writes its
report in `$TSLINK_TEST_DIR/reports/` too. [`test/security/report.sh`](../test/security/report.sh) needs both reports
for a release.

The testbed and the red team do not run in CI. Run them by hand. Before a release, follow
[docs/releasing.md](releasing.md).

## Fuzzing

Fuzz targets check code that handles input from containers, stack files and the host:

- State directory paths
- Network options
- Serve labels, and the serve config that they become
- The `resolv.conf` of tailscaled
- Certificate paths

Their seeds run with each `go test`. `fuzz.yml` fuzzes each target for ten minutes each week, and on request. It keeps
a failing input as an artifact. When the bug is fixed, commit the input in `testdata/fuzz/` of the package. It then
becomes a regression test.
