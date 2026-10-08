# Plugin settings

Plugin settings configure tslink for all networks on one host. Set them with `docker plugin set` while the plugin
is disabled. See [Change a setting](../guides/install-and-upgrade.md#change-a-setting).

| Setting | Values | Default |
| --- | --- | --- |
| [`TS_AUTHKEY`](#ts_authkey) | A credential | None |
| [`shared.source`](#sharedsource) | A directory on the host | The data directory |
| [`TSLINK_ISOLATE_HOST_TAILNET`](#tslink_isolate_host_tailnet) | `true` or `false` | `true` |
| [`TSLINK_TAG_SCOPE`](#tslink_tag_scope) | `exact` or `prefix` | `exact` |
| [`TS_DEBUG_ACME_DIRECTORY_URL`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A URL | Let's Encrypt |
| [`SSL_CERT_FILE`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A file in the plugin | The system's root certificates |

## TS_AUTHKEY

The credential for networks without the option `tslink.authkey` on hosts without a cluster credential. See
[Precedence](credentials.md#precedence).

## shared.source

The source of the plugin's `shared` mount. tslink keeps HTTPS certificates and their ACME account key in `certs/` in
this directory. Set it to a volume that all hosts share. tslink then gets the certificate of a Service once for the
cluster, and not once for each host.

> [!WARNING]
> The directory holds the private key of each Service. Restrict access to the hosts of the cluster.

## TSLINK_ISOLATE_HOST_TAILNET

Prevents containers from reaching the tailnet through the tailscaled of the host. It applies to all containers on the
host, on tslink networks and on other networks.

| Value | Effect |
| --- | --- |
| `true` | Containers cannot reach the tailnet through the host. DNS queries to `100.100.100.100` are permitted. |
| `false` | Containers can reach the tailnet as the host. Use this value only if containers on the host must use the tailscaled of the host. |
| Other values | tslink keeps the value `true` and logs a warning. |

The firewall rules stay when the plugin stops. For how they work, see [Isolation](../explanation/architecture.md#isolation).
For the guarantee that this setting supports, see `G1` in [SECURITY.md](../../SECURITY.md).

## TSLINK_TAG_SCOPE

Sets which tags a stack can use with the cluster credential: `exact` or `prefix`. For the permitted tags, see
[Tag rules](credentials.md#tag-rules). For other values, tslink uses `exact` and logs a warning.

> [!WARNING]
> With `prefix`, a stack can use the base tag of a stack with a longer name. For example, stack `a` can use
> `tag:a-b`, the base tag of stack `a-b`. If a stack needs more than one tag, give its network its own
> `tslink.authkey`.

## TS_DEBUG_ACME_DIRECTORY_URL and SSL_CERT_FILE

For tests only. `TS_DEBUG_ACME_DIRECTORY_URL` sets a different ACME directory, for example the staging environment
of Let's Encrypt. `SSL_CERT_FILE` sets a root certificate bundle that trusts it.

## Removed settings

| Setting | Status |
| --- | --- |
| `TS_VERSION` | tslink ignores every value other than `bundled` and logs a warning. |
| `TS_PATH` | tslink ignores the setting and logs a warning. |

tslink runs only the Tailscale binaries in the plugin image. See [Binaries](../explanation/architecture.md#binaries).

## Internal settings

Do not change `TS_DATA_DIR`, `TS_SHARED_DIR` and `DOCKER_HOST`. The plugin configuration sets them, and
`docker plugin set` cannot change them.
