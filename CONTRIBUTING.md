# Contributing

Bug reports, fixes and improvements are welcome. Open an issue first for
larger changes, so we can agree on the approach before you write the code.
Report security issues privately, as described in [SECURITY.md](SECURITY.md).

## Prerequisites

- Go, at the version in [go.mod](go.mod) or newer
- Docker on Linux, or in a Linux VM (OrbStack, Docker Desktop)
- A Tailscale auth key from <https://login.tailscale.com/admin/settings/keys>,
  to try the plugin against a tailnet
- [golangci-lint](https://golangci-lint.run/), the version CI pins (see below)

## Build and run locally

```bash
echo "TS_AUTHKEY=tskey-auth-xxx" > .env
make reinstall        # build the image, create and enable the plugin
make test-network     # create the network tsnet-test with the key from .env
make test-container   # run a container on it
make test-network-rm
```

`make reinstall` builds the plugin from `docker/Dockerfile` and installs it as
`ghcr.io/matchory/tslink:latest` (override with `PLUGIN_NAME` and
`PLUGIN_TAG`). Networks created by hand name that as the driver:

```bash
docker network create --driver ghcr.io/matchory/tslink:latest \
  --opt tslink.authkey="$TS_AUTHKEY" tailnet
docker run --rm --network tailnet alpine sh -c "ip addr && ping -c 2 8.8.8.8"
docker network rm tailnet
```

`make logs` shows the plugin's output. The [readme](README.md#troubleshooting)
lists where each container's `tailscaled.log` is.

## Tests

```bash
go test -race -cover ./...
go tool govulncheck ./...
```

The network namespace tests in `pkg/netutil` need `CAP_NET_ADMIN` and skip
without it. CI builds them as a normal user and runs them as root; on a Linux
machine:

```bash
go test -c -o /tmp/netutil.test ./pkg/netutil
sudo /tmp/netutil.test -test.v -test.count=1
```

`test/integration/run.sh` is the end-to-end test: it builds the plugin,
installs it as `tslink`, and runs containers against a local headscale, so it
needs no Tailscale account. CI runs it on every pull request; locally it needs
Linux, Docker and `sudo`. `test/cluster` builds a Swarm in the cloud and runs
a regression suite against a real tailnet, including Tailscale Services (see
[test/cluster/README.md](test/cluster/README.md)).

## Linters

CI runs two linters; run both before opening a pull request.

**Go**: golangci-lint, with the version pinned in
[.github/workflows/ci.yml](.github/workflows/ci.yml) and the configuration in
`.golangci.toml`:

```bash
golangci-lint run        # check
golangci-lint run --fix  # fix what can be fixed automatically
golangci-lint fmt        # format
```

**Everything else** (Markdown, YAML, shell, Dockerfile, and more):
[super-linter](https://github.com/super-linter/super-linter), configured in
[.github/workflows/linter.yml](.github/workflows/linter.yml) with rule files in
`.github/linters/`. It ignores `specs/`. To run it locally, start its container
image with `RUN_LOCAL=true`, the repository mounted at `/tmp/lint`, and the
`VALIDATE_*` and `FILTER_REGEX_EXCLUDE` settings from `linter.yml`. Or run the
most common checks on their own:

```bash
npx -y markdownlint-cli2 --config .github/linters/.markdown-lint.yml "**/*.md"
shellcheck scripts/*.sh
```

## Code style

- Handle every error; log cleanup failures instead of discarding them.
- Compare errors with `errors.Is` and `errors.As`, and wrap them with `%w`.
- Never hold `driver.mu` while calling endpoint methods, which take
  `endpoint.mu`; and keep network syscalls, downloads and `tailscale up`
  outside locks.

[CLAUDE.md](CLAUDE.md) explains the main design decisions (state directories,
asynchronous Tailscale start, routing, self-healing, draining), and `specs/`
holds the design history.

## Scope

- tslink is for anyone running Docker with Tailscale. Keep code, docs and
  tests free of assumptions about a particular organisation, cloud provider
  or configuration tool; state what a host needs as a requirement instead.
  Docs install the plugin with `--alias tslink` and use `driver: tslink`.
- Tailscale's control server comes first. headscale is supported on a
  best-effort basis: the end-to-end test runs against it, but a change
  should not trade Tailscale behaviour for headscale behaviour.
- Tests that verify a guarantee in [SECURITY.md](SECURITY.md) name it, and
  security probes need a control run; see
  [docs/testing.md](docs/testing.md#security-model).

## Pull requests

- Keep changes focused, with tests for new behaviour and fixed bugs.
- Update the readme and docs when behaviour changes, and add an entry under
  `Unreleased` in [CHANGELOG.md](CHANGELOG.md).
- Dependency updates come from Dependabot.

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE).
