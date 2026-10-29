---
title: Files and paths
description: Where tslink keeps node state, sockets, logs, status files and certificates, and what each file holds.
category: reference
tags: [files, data-directory, logs, status, state]
---

# Files and paths

tslink keeps every file it writes in its data directory, `/var/lib/docker-plugins/tailscale` on the host, which the
plugin sees as `/data`. Several files are per endpoint: one container's connection to one tslink network.

## Data directory

```text
/var/lib/docker-plugins/tailscale/
├── by-hostname/<hostname>/          state directory of a container outside a stack
├── by-stack/<stack>/<hostname>/     state directory of a container in a stack
├── sock/<endpoint>.sock             tailscaled socket of each endpoint
├── status/<endpoint>.json           status file of each endpoint
├── status/<endpoint>.ready          readiness latch of each endpoint
├── certs/                           HTTPS certificates, if shared.source is not set
├── oauth-client.secret              cluster credential (optional)
└── plugin.log                       plugin log
```

`<endpoint>` is the first 12 characters of the Docker endpoint ID. The sockets live in `sock/` rather than in the
state directories because a Unix socket path can't be longer than 108 bytes. `<hostname>` and `<stack>` start with a
letter or digit, and contain only letters, digits, `.`, `_` and `-`.

The plugin creates the data directory, with mode 0755, as it starts. Don't delete it while the plugin is installed.

## State directory

Each node has one state directory, and only one endpoint at a time can use it.

| File | Content |
| --- | --- |
| `tailscaled.state` | The identity of the node, with its private keys |
| `tailscaled.log` | The log of tailscaled |
| `tailscaled.sock` | A link to the socket in `sock/` |
| `ephemeral` | A marker that the node is ephemeral |

> [!WARNING]
> Whoever can read `tailscaled.state` can act as the node. Keep the data directory readable by root only.

## Logs

tslink rotates each log at a fixed size, and keeps one rotated file, so the logs can't fill the disk.

| Log | Rotated at | Maximum on disk |
| --- | --- | --- |
| `<state directory>/tailscaled.log` | 10 MB | 20 MB for each endpoint |
| `plugin.log` | 50 MB | 100 MB |

## Status files

tslink writes `status/<endpoint>.json` while the endpoint exists.

| Field | Type | Content |
| --- | --- | --- |
| `endpoint` | string | The endpoint ID |
| `hostname` | string | The hostname of the node |
| `stack` | string | The stack's name. Absent outside a stack. |
| `state` | string | `running`, `retrying` or `failed` |
| `error` | string | The last error. Absent without one. |
| `attempts` | number | The number of start attempts |
| `updated` | string | The time of the last change, in RFC 3339 format |
| `warnings` | array | Conditions worth a look. Absent without any. |

| State | Meaning |
| --- | --- |
| `running` | tailscaled is running. |
| `retrying` | The start failed, and tslink tries again. |
| `failed` | The start failed, and tslink won't try again, for example because the stack isn't allowed a tag. |

Each item in `warnings` has the fields `key`, `message` and `since`.

| Key | Condition |
| --- | --- |
| `dns-upstreams` | tailscaled found no DNS server of the container or the host, and uses a fallback. |
| `renewal-blocked/<domain>` | Let's Encrypt refuses to renew the certificate of `<domain>`. |
| `cert-expiry/<domain>` | The certificate of `<domain>` expires in less than 14 days. |
| `health` | tslink can't start the readiness endpoint on the port of `tslink.health`. |
| `node-name` | The control server named the node `<hostname>-N`, because another node holds the name. See [slow registrations](../explanation/limitations.md#slow-registrations). |
| `service/<service>` | Configuring the Service failed. tslink retries, and clears the warning once it succeeds. |
| `ports/<service>` | The container doesn't serve every port of the Service. The message names the ports. |
| `control/<service>` | The control server hasn't fetched the node's Service list after tslink advertised the Service again 3 times. |

## Shared directory

With the plugin setting [`shared.source`](plugin-settings.md#sharedsource), tslink keeps HTTPS certificates and their
ACME account key in `certs/` in that directory; otherwise in `certs/` in the data directory.
