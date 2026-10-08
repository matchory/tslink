# Documentation

The documentation has four types of pages. Start with the tutorial. Use the guides for tasks, the reference for
facts, and the explanations for reasons.

## Tutorial

- [Getting started](getting-started.md): install tslink on one host, and open a container over HTTPS from the
  tailnet.

## Guides

- [Install and upgrade tslink](guides/install-and-upgrade.md): install, upgrade and configure the plugin.
- [Provision a host](guides/provision-a-node.md): prepare a production host or Swarm node.
- [Deploy on Swarm](guides/deploy-on-swarm.md): give each stack its own tags and credential.
- [Expose a service](guides/expose-a-service.md): serve a container over HTTPS, and as a Tailscale Service.
- [Update without downtime](guides/zero-downtime-updates.md): update Services, restart Docker and upgrade tslink.
- [Monitor tslink](guides/monitor.md): get an alert when a node is not running.
- [Troubleshoot](guides/troubleshoot.md): find the cause of a symptom.

## Reference

- [Network options](reference/network-options.md): options of `docker network create`.
- [Container labels](reference/container-labels.md): labels of a container.
- [Plugin settings](reference/plugin-settings.md): settings of `docker plugin set`.
- [Credentials](reference/credentials.md): credential types, precedence, ephemeral nodes and tag rules.
- [Readiness endpoint](reference/readiness-endpoint.md): the endpoint for Docker healthchecks.
- [Files and paths](reference/files-and-paths.md): the data directory, logs and status files.
- [tslink diag](reference/diag.md): diagnostics and the preflight check.

## Explanation

- [Architecture](explanation/architecture.md): how tslink connects a container to the tailnet.
- [Security model](explanation/security-model.md): how tslink keeps stacks apart.
- [HTTPS certificates](explanation/https-certificates.md): certificates and the limits of Let's Encrypt.
- [Limitations](explanation/limitations.md): what tslink cannot do.

## Project

- [Style guide](STYLE.md): the rules for this documentation.
- [Testing](testing.md): how tslink is tested.
- [Contributing](../CONTRIBUTING.md), [Security policy](../SECURITY.md) and [Changelog](../CHANGELOG.md).
