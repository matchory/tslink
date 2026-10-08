# Testing

tslink is tested at four levels, from unit tests that run anywhere to a
Swarm in the cloud against a real tailnet. [CONTRIBUTING.md](../CONTRIBUTING.md#tests)
says how to run each of them.

## Unit tests

`go test -race ./...` covers the driver, endpoint and network logic, the
tailscaled supervisor, state directories and garbage collection, with
tailscaled, Docker and the network replaced by fakes. They run on any
platform and in CI on every pull request.

`pkg/preflight` checks the environment properties of SECURITY.md (`tslink
diag --preflight`), tested against a fake Docker API.

## Network namespace tests

The tests in `pkg/netutil` create network namespaces, veth pairs, routes and
rules, so they need Linux and `CAP_NET_ADMIN`; without it they skip. CI runs
them a second time as root.

## In CI

Test failures appear as annotations on the pull request, and each run's
summary lists coverage per package, merged across both runs; the profile and
an HTML report are kept as the `coverage` artifact. Coverage has no
threshold. Lint and govulncheck findings go to GitHub code scanning.

## End-to-end test

`test/integration/run.sh` builds the plugin, installs it and runs containers
against a [headscale](https://github.com/juanfont/headscale) control server
on the same machine, so it needs no Tailscale account. It checks that
containers reach each other over the tailnet, that the policy refuses what it
does not grant, that a container keeps its identity across a plugin restart,
and that an ephemeral node is removed with its container. CI runs it on every
pull request. Tailscale Services are not covered: headscale does not support
them.

## Cluster tests

`test/cluster` installs tslink on a Docker Swarm of three or more nodes and
tests it against a real tailnet, including Tailscale Services; see
[test/cluster/README.md](../test/cluster/README.md). Its regression suite,
`regress.sh`, checks that:

- every task's tailscaled is running;
- a task reaches only what the policy grants it, and traffic does not leak
  out through its host's own Tailscale;
- DNS works for tasks on an overlay network and on tslink;
- a rolling update of a Tailscale Service host costs its callers at most a
  few failed requests;
- draining and reactivating a node leaves no orphaned devices;
- a plugin upgrade keeps every node's identity.

`disrupt` reboots a node, a manager included, restarts or kills its Docker
daemon, or resets or powers it off, while callers probe a Tailscale Service,
and times the recovery. The cluster tests need a Swarm and a tailnet set up
for them, so they do not run in CI.

## Fuzzing

Fuzz targets check properties of code that handles input from containers,
stack files and the host: state directory paths, network options, serve
labels and the `tailscale serve` arguments they become, tailscaled's
`resolv.conf`, and certificate paths. Their seeds run with every `go test`.
`fuzz.yml` fuzzes each target for ten minutes every week and on demand, and
keeps a failing input as an artifact; once fixed, commit it under the
package's `testdata/fuzz/`, where it becomes a regression test.
