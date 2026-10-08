# Files and paths

tslink keeps all its files in the data directory, `/var/lib/docker-plugins/tailscale` on the host. Inside the
plugin, the data directory is `/data`. An endpoint is the connection of one container to one tslink network.

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

`<endpoint>` is the first 12 characters of the Docker endpoint ID. `<hostname>` and `<stack>` must start with a
letter or a digit, and contain only letters, digits, `.`, `_` and `-`.

The data directory must exist before you install the plugin. Do not delete it while the plugin is installed.

## State directory

Each node has one state directory. One endpoint at a time can use a state directory.

| File | Content |
| --- | --- |
| `tailscaled.state` | The identity of the node, with its private keys |
| `tailscaled.log` | The log of tailscaled |
| `tailscaled.sock` | A link to the socket in `sock/` |
| `ephemeral` | A marker: the node is ephemeral |

> [!WARNING]
> A person who can read `tailscaled.state` can use the identity of the node. Restrict access to the data directory to
> root.

## Sockets

The socket of each endpoint is in `sock/`, not in the state directory, because a Unix socket path can have only 108
bytes.

## Logs

| Log | Size of one file | Copies |
| --- | --- | --- |
| `<state directory>/tailscaled.log` | 10 MB | The current file and one rotated file |
| `plugin.log` | 50 MB | The current file and one rotated file |

Thus each endpoint uses up to 20 MB for logs, and the plugin log uses up to 100 MB.

## Status files

tslink writes `status/<endpoint>.json` while the endpoint exists.

| Field | Type | Content |
| --- | --- | --- |
| `endpoint` | string | The endpoint ID |
| `hostname` | string | The hostname of the node |
| `stack` | string | The stack name. The field is absent outside a stack. |
| `state` | string | `running`, `retrying` or `failed` |
| `error` | string | The last error. The field is absent without an error. |
| `attempts` | number | The number of start attempts |
| `updated` | string | The time of the last change, in RFC 3339 format |
| `warnings` | array | Conditions to examine. The field is absent without warnings. |

| State | Meaning |
| --- | --- |
| `running` | tailscaled is running. |
| `retrying` | The start failed. tslink tries again. |
| `failed` | The start failed. tslink does not try again, for example because a tag is not permitted. |

Each item in `warnings` has the fields `key`, `message` and `since`.

| Key | Condition |
| --- | --- |
| `dns-upstreams` | tailscaled found no DNS server of the container or the host, and uses a fallback. |
| `renewal-blocked/<domain>` | Let's Encrypt refuses the renewal of the certificate of `<domain>`. |
| `cert-expiry/<domain>` | The certificate of `<domain>` expires in less than 14 days. |
| `health` | tslink cannot start the readiness endpoint on the port of `tslink.health`. |

## Shared directory

If the plugin setting `shared.source` is set, tslink keeps the HTTPS certificates and their ACME account key in
`certs/` in that directory. Otherwise it keeps them in `certs/` in the data directory.
