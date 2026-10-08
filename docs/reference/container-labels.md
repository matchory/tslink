# Container labels

Container labels configure one container on a tslink network. tslink reads the labels of the container: `--label`
on `docker run`, or `labels` of a service in a Compose or stack file. tslink does not read the labels of a Swarm
service (`deploy.labels`).

| Label | Value | Default |
| --- | --- | --- |
| [`tslink.hostname`](#tslinkhostname) | A hostname | From the container name |
| [`tslink.tags`](#tslinktags) | Comma-separated ACL tags | None |
| [`tslink.serve.<port>`](#tslinkserveport) | A serve rule | None |
| [`tslink.service`](#tslinkservice) | `svc:<name>` | None |
| [`tslink.direct`](#tslinkdirect) | `true` or `false` | `true` without `tslink.service`, `false` with it |
| [`tslink.health`](#tslinkhealth) | A port, 1 to 65535 | None |

## tslink.hostname

The hostname of the node in the tailnet, for example `my-api`.

| Condition | Effect |
| --- | --- |
| The label is not set | tslink uses the container name. It replaces each character that is not an ASCII letter or a digit with `-`, uses the first 63 characters, and removes `-` at the start and the end. |
| Two containers on one host use the same hostname | The second container waits without a node until the first container leaves. |
| A container starts with the hostname of an earlier container | It gets the identity of the earlier container: the same node and the same tailnet addresses. This is not true if the earlier node was ephemeral, or if the network has a different `tslink.authkey`: tslink deleted the state, and the node is new. |

## tslink.tags

The ACL tags of the node, separated by commas, for example `tag:server,tag:prod`. If the network has the option
`tslink.tags`, tslink ignores this label.

## tslink.serve.\<port\>

Makes a port of the container available in the tailnet through Tailscale Serve. `<port>` is the port in the tailnet.
The value has this syntax:

```text
<protocol>[:<target>][/<path>][?<option>[&<option>]]
```

| Part | Value |
| --- | --- |
| `<protocol>` | `http`, `https`, `tcp`, `tls-terminated-tcp` or `tun` |
| `<target>` | The port in the container. The default is `<port>`. |
| `<path>` | For `http` and `https`: the path to serve, for example `/api` |
| `<option>` | See [Serve options](#serve-options) |

| Example | Effect |
| --- | --- |
| `tslink.serve.443=https:8080` | HTTPS on port 443, to port 8080 in the container |
| `tslink.serve.80=http:3000/api` | HTTP on port 80 at `/api`, to port 3000 |
| `tslink.serve.5432=tcp` | TCP on port 5432, to port 5432 |
| `tslink.serve.0=tun` | All traffic to the Service, see [Layer 3 forwarding](#layer-3-forwarding) |

tslink ignores a rule with an incorrect option, and logs a warning. It also ignores a rule with an unknown protocol,
and logs this only at the debug level.

> [!NOTE]
> Tailscale gets a Let's Encrypt certificate for each name that it serves HTTP on, also for plain HTTP. See
> [HTTPS certificates](../explanation/https-certificates.md).

### Layer 3 forwarding

With `tun`, Tailscale forwards all traffic to the addresses of the container's Service to its Tailscale interface,
whatever the port (`tailscale serve --tun`). The application must receive this traffic there, for example through
firewall rules in the container: tslink adds none.

- `tun` needs [`tslink.service`](#tslinkservice). Without one, tslink ignores the rule and logs this only at the debug
  level.
- `<port>` must be a number, but tslink ignores it, the target and the path.
- Serve options are not allowed: tslink ignores a `tun` rule with an option and logs a warning.

### Serve options

| Option | Protocols | Effect |
| --- | --- | --- |
| `proxy-protocol=1` or `proxy-protocol=2` | `tcp`, `tls-terminated-tcp` | Tailscale sends a PROXY protocol header, version 1 or 2, with the tailnet address of the caller. The application must accept the header. |
| `accept-app-caps=<cap>[,<cap>]` | `http`, `https` | Tailscale sends the grants of the caller for these [app capabilities](https://tailscale.com/kb/1537/grants-app-capabilities) as JSON in the `Tailscale-App-Capabilities` header. Each `<cap>` has the form `<domain>/<name>`. |

Example: `tslink.serve.5432=tcp:5432?proxy-protocol=2`.

### Caller identity

| Protocol | What the application receives |
| --- | --- |
| `http`, `https` | The tailnet address of the caller in `X-Forwarded-For` |
| `tcp`, `tls-terminated-tcp` | Connections from `127.0.0.1`. With `proxy-protocol`, the address of the caller in the PROXY header. |

A tagged caller gets no `Tailscale-User-*` headers. To authorize a tagged caller, use `accept-app-caps`. Serve
replaces a `Tailscale-App-Capabilities` header that the caller sends.

## tslink.service

Makes the container a backend of a Tailscale Service, for example `svc:my-api`. The container must also have at least
one `tslink.serve.<port>` label. The Service must exist in the tailnet, and the credential of the network must have
tags.

## tslink.direct

Also serves the `tslink.serve.<port>` rules on the hostname of the container.

| Value | Effect |
| --- | --- |
| `false`, `0` or `no` | Serves only on the Service |
| Other values | Serves on the Service and on the hostname |
| Not set | `true` without `tslink.service`, `false` with it |

## tslink.health

Starts the readiness endpoint on this port of the loopback interface of the container. tslink ignores a value that is
not a port from 1 to 65535, and logs a warning. See [Readiness endpoint](readiness-endpoint.md).
