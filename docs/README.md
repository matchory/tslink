# tslink documentation

These pages are published, with search, at <https://matchory.github.io/tslink/>. They read the same here on
GitHub. New to tslink? Start with the tutorial.

## Start here

- [Getting started](getting-started.md): install tslink on one host, and open a container over HTTPS from your
  tailnet.

## Guides

How to get one task done.

- [Install and upgrade tslink](guides/install-and-upgrade.md): install the plugin, upgrade it safely, and change its
  settings.
- [Provision a host](guides/provision-a-node.md): get a host ready for production, on its own or as a Swarm node.
- [Deploy on Swarm](guides/deploy-on-swarm.md): give each stack its own tags and credential.
- [Expose a service](guides/expose-a-service.md): serve a container over HTTPS, and put replicas behind a Tailscale
  Service.
- [Update without downtime](guides/zero-downtime-updates.md): roll out versions, restart Docker and upgrade tslink
  without dropping requests.
- [Monitor tslink](guides/monitor.md): get an alert when a node isn't running.
- [Troubleshoot](guides/troubleshoot.md): find the cause of a symptom.

## Reference

Every fact about one interface, to look up.

- [Network options](reference/network-options.md): the options of `docker network create`.
- [Container labels](reference/container-labels.md): what a container can configure.
- [Plugin settings](reference/plugin-settings.md): the settings of `docker plugin set`.
- [Credentials](reference/credentials.md): credential types, precedence, ephemeral nodes and tag rules.
- [Readiness endpoint](reference/readiness-endpoint.md): the endpoint for Docker healthchecks.
- [Files and paths](reference/files-and-paths.md): the data directory, logs and status files.
- [tslink diag](reference/diag.md): diagnostics and the preflight check.

## Explanation

Why tslink works the way it does.

- [Architecture](explanation/architecture.md): how a container becomes a node of its own.
- [Security model](explanation/security-model.md): how tslink keeps stacks apart, and whom you have to trust.
- [HTTPS certificates](explanation/https-certificates.md): how certificates are shared, and Let's Encrypt's limits.
- [Limitations](explanation/limitations.md): what tslink can't do, and why.

## For contributors

[CONTRIBUTING.md](../CONTRIBUTING.md) covers building, testing and releasing tslink, and how these pages are
published. The [style guide](STYLE.md) says how to write them, [testing](testing.md) and [releasing](releasing.md)
describe the test suites and the release checklist, and [specs](specs/) hold the design history. Security reports go
through [SECURITY.md](../SECURITY.md); every release's changes are in the [changelog](../CHANGELOG.md).
