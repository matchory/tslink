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
checks these properties:

- The tailscaled of each task runs.
- A task reaches only what the policy permits.
- No traffic leaves through the Tailscale of the host.
- DNS works for tasks on an overlay network and on tslink.
- A rolling update of a Service backend causes few or no failed requests for its callers.
- When a node is drained and made active again, no orphaned devices stay in the tailnet.
- A plugin upgrade keeps the identity of each node.

`disrupt` does one of these actions to a node, a manager included, while callers send requests to a Tailscale
Service. It then measures the recovery time.

- Reboot the node.
- Restart or kill its Docker daemon.
- Reset the node, or switch it off.

The cluster tests need a Swarm and a tailnet that are set up for them. Thus they do not run in CI.

## Security testbed

[`test/security/testbed`](../test/security/testbed) provisions one Hetzner Cloud server for the end-to-end test and
the red team. [`test/security/testbed/README.md`](../test/security/testbed/README.md) lists the prerequisites.

[`test/security/redteam.sh`](../test/security/redteam.sh) does these steps:

1. It copies a working tree to the server.
2. It runs `test/integration/run.sh`.
3. It runs the probes in [`test/security/redteam/`](../test/security/redteam), and the probes in a private directory
   that you give with `--probes`.
4. It writes a report in `$TSLINK_TEST_DIR/reports/`. Do not commit the report.

The testbed and the red team do not run in CI. Run them by hand. Before a release, follow
[docs/releasing.md](releasing.md).

## Fuzzing

Fuzz targets check code that handles input from containers, stack files and the host:

- State directory paths
- Network options
- Serve labels, and the `tailscale serve` arguments that they become
- The `resolv.conf` of tailscaled
- Certificate paths

Their seeds run with each `go test`. `fuzz.yml` fuzzes each target for ten minutes each week, and on request. It keeps
a failing input as an artifact. When the bug is fixed, commit the input in `testdata/fuzz/` of the package. It then
becomes a regression test.
