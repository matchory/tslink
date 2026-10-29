# Notes for agents

How it works is in `docs/explanation/architecture.md`; how to test is in `docs/testing.md` and `CONTRIBUTING.md`.

- Lock order is `driver.mu` then `endpoint.mu`, never the reverse: do not hold `driver.mu` while calling endpoint
  methods. Do not hold any lock across netlink calls, logins or other calls that can block for seconds.
- The auth key goes only into the LocalAPI start request: never into arguments, the environment or a file. The
  end-to-end test checks this.
- tslink downloads nothing at runtime and runs no `tailscale` CLI; it uses the pinned binaries in the plugin image and
  the LocalAPI. The Go module `tailscale.com` must match the Tailscale version in `docker/Dockerfile`
  (`TestLocalAPIClientMatchesBundledTailscale`), so Dependabot's Dockerfile and Go module updates land together.
- `test/integration/run.sh` replaces the host's `tslink` plugin. Run it only on a disposable Linux machine. The image
  digests in it are pinned by hand; Dependabot does not update them.
- The netns tests in `pkg/netutil` and the mount namespace test in `pkg/tailscale` skip without root, so a green
  `go test` on macOS or as a normal user did not run them.
- `golines` counts trailing comments, so put a `//nolint` that would not fit on the line before.
- A test that verifies a `SECURITY.md` guarantee carries a `Guards: G<n>` marker (see `docs/testing.md`).
