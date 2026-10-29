---
title: Container labels
description: Every label a container on a tslink network can set, with its values, defaults and conditions.
category: reference
tags: [labels, serve, services, hostname, tags]
---

# Container labels

Labels configure one container on a tslink network: its node's name and tags, what it serves, and which Tailscale
Services it backs. Set them with `--label` on `docker run`, or under `labels` of a service in a Compose or stack file.

tslink reads the container's labels only. It doesn't see a Swarm service's `deploy.labels`. A label that starts with
`tslink.` but isn't listed here is ignored, with a warning that names the container.

| Label | Value | Default |
| --- | --- | --- |
| [`tslink.hostname`](#tslinkhostname) | A hostname | From the container name |
| [`tslink.tags`](#tslinktags) | Comma-separated ACL tags | None |
| [`tslink.serve.<port>`](#tslinkserveport) | A serve rule | None |
| [`tslink.service`](#tslinkservice) | `svc:<name>` | None |
| [`tslink.service.<name>.serve.<port>`](#tslinkservicenameserveport) | A serve rule for the Service `svc:<name>` | None |
| [`tslink.direct`](#tslinkdirect) | `true` or `false` | `true` without a Service, `false` with one |
| [`tslink.health`](#tslinkhealth) | A port, 1 to 65535 | None |

## tslink.hostname

The name of the container's node in your tailnet, such as `my-api`.

| Value | Default |
| --- | --- |
| A hostname | The container's name, cleaned up |

Without the label, tslink uses the container's name: it replaces every character that isn't an ASCII letter or digit
with `-`, keeps the first 63 characters, and trims `-` from both ends.

tslink keeps a node's state by hostname, so a container that starts with the hostname of an earlier one comes back as
the same node, with the same tailnet addresses. Two exceptions start a new node instead: the earlier node was
ephemeral, or the network has a different `tslink.authkey`. In both cases tslink has deleted the old state.

If two containers on one host use the same hostname, the second one waits, without a node, until the first leaves.

## tslink.tags

The node's ACL tags, separated by commas, such as `tag:server,tag:prod`.

| Value | Default |
| --- | --- |
| Comma-separated tags | None |

If the network sets the option [`tslink.tags`](network-options.md#tslinktags), tslink ignores this label: the
network's tags take precedence, so a container can't pick its own.

## tslink.serve.\<port\>

Serves a port of the container in your tailnet through Tailscale Serve. `<port>` is the port in the tailnet.

| Value | Default |
| --- | --- |
| A serve rule | None |

A serve rule has this form:

```text
<protocol>[:<target>][/<path>][?<option>[&<option>]]
```

| Part | Value |
| --- | --- |
| `<protocol>` | `http`, `https`, `tcp`, `tls-terminated-tcp` or `tun` |
| `<target>` | The port in the container. Defaults to `<port>`. |
| `<path>` | For `http` and `https`: the path to serve, such as `/api` |
| `<option>` | One of the [serve options](#serve-options) |

| Example | What it does |
| --- | --- |
| `tslink.serve.443=https:8080` | HTTPS on port 443, to port 8080 in the container |
| `tslink.serve.80=http:3000/api` | HTTP on port 80 at `/api`, to port 3000 |
| `tslink.serve.5432=tcp` | TCP on port 5432, to port 5432 |
| `tslink.serve.0=tun` | All traffic to the Service, see [layer 3 forwarding](#layer-3-forwarding) |

A rule with an unknown protocol or an invalid option is ignored, with a warning.

> [!NOTE]
> Tailscale gets a Let's Encrypt certificate for every name it serves HTTP on, plain HTTP included. See
> [HTTPS certificates](../explanation/https-certificates.md).

### Layer 3 forwarding

With `tun`, Tailscale forwards all traffic to the addresses of the container's Service to the container's Tailscale
interface, on any port, as `tailscale serve --tun` does. Your application has to receive it there, for example
through firewall rules in the container; tslink adds none.

- `tun` needs [`tslink.service`](#tslinkservice). Without one, tslink ignores the rule, and logs that only at the
  debug level (see [`TSLINK_LOG_LEVEL`](plugin-settings.md#tslink_log_level)).
- `<port>` must be a number, but tslink ignores it, along with the target and the path.
- Serve options aren't allowed: a `tun` rule with an option is ignored, with a warning.

### Serve options

| Option | Protocols | What it does |
| --- | --- | --- |
| `proxy-protocol=1` or `proxy-protocol=2` | `tcp`, `tls-terminated-tcp` | Tailscale sends a PROXY protocol header, version 1 or 2, with the caller's tailnet address. Your application has to accept the header. |
| `accept-app-caps=<cap>[,<cap>]` | `http`, `https` | Tailscale sends the caller's grants for these [app capabilities](https://tailscale.com/kb/1537/grants-app-capabilities) as JSON in the `Tailscale-App-Capabilities` header. Each `<cap>` has the form `<domain>/<name>`. |

For example: `tslink.serve.5432=tcp:5432?proxy-protocol=2`.

### Caller identity

| Protocol | What your application receives |
| --- | --- |
| `http`, `https` | The caller's tailnet address in `X-Forwarded-For` |
| `tcp`, `tls-terminated-tcp` | Connections from `127.0.0.1`. With `proxy-protocol`, the caller's address in the PROXY header. |

A tagged caller gets no `Tailscale-User-*` headers; use `accept-app-caps` to authorize one. On requests that come
through Serve, Serve replaces any `Tailscale-App-Capabilities` header the caller sends. A caller that reaches your
application another way, such as over another Docker network, can set it freely, so trust the header only if the
application listens on `127.0.0.1`.

## tslink.service

Makes the container a backend of a Tailscale Service, such as `svc:my-api`.

| Value | Default |
| --- | --- |
| `svc:<name>` | None |

The name after `svc:` must be a lowercase DNS label: letters, digits and hyphens, at most 63 characters, with no
hyphen at either end. With any other name, the container's Tailscale doesn't start.

The container also needs at least one [`tslink.serve.<port>`](#tslinkserveport) label, the Service has to exist in
your tailnet, and the network's credential has to give the node tags. To back several Services from one container,
use [`tslink.service.<name>.serve.<port>`](#tslinkservicenameserveport) instead.

## tslink.service.\<name\>.serve.\<port\>

Makes the container a backend of the Tailscale Service `svc:<name>`, and serves `<port>` on it. Use one label per
Service and port.

| Value | Default |
| --- | --- |
| A serve rule, as for [`tslink.serve.<port>`](#tslinkserveport) | None |

```yaml
labels:
  tslink.service.ai-gateway.serve.443: https:8080
  tslink.service.ai-admin.serve.443: https:8081
  tslink.service.ai-metrics.serve.9090: tcp
```

- `<name>` is the Service's name without `svc:`, in lowercase.
- tslink advertises each Service as soon as its own configuration is applied and, for HTTPS, its certificate exists.
  A Service that's still waiting doesn't hold back the others. The container's
  [readiness](readiness-endpoint.md) waits for all of them.
- When the container stops, all its Services are drained together.
- A container can't mix this form with `tslink.service` and `tslink.serve.<port>`, or combine it with
  `tslink.direct=true`. tslink refuses such a container, and its status file names the reason and the label.
- Every node of a cluster has to run tslink v0.3.5 or later before a stack uses these labels: older versions ignore
  them, and report the container ready while it serves nothing. Don't downgrade a node while such containers run on
  it, because an older version doesn't drain their Services.

## tslink.direct

Serves the container's `tslink.serve.<port>` rules on its own hostname as well as on its Service.

| Value | Default |
| --- | --- |
| `true` or `false` | `true` without a Service, `false` with one |

`false`, `0` and `no` serve on the Service only. Any other value serves on both.

It's off by default with a Service because serving on the hostname too gives every replica a certificate of its own,
which uses up Let's Encrypt's [rate limits](../explanation/https-certificates.md#rate-limits).

## tslink.health

Starts the [readiness endpoint](readiness-endpoint.md) on this port of the container's loopback interface, for use
in a Docker healthcheck.

| Value | Default |
| --- | --- |
| A port, 1 to 65535 | None |

A value that isn't a port from 1 to 65535 is ignored, with a warning.
