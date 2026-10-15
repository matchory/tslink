# Plugin settings

Plugin settings configure tslink for all networks on one host. Set them with `docker plugin set` while the plugin
is disabled. See [Change a setting](../guides/install-and-upgrade.md#change-a-setting).

| Setting | Values | Default |
| --- | --- | --- |
| [`TS_AUTHKEY`](#ts_authkey) | A credential | None |
| [`shared.source`](#sharedsource) | A directory on the host | The data directory |
| [`TSLINK_ISOLATE_HOST_TAILNET`](#tslink_isolate_host_tailnet) | `true` or `false` | `true` |
| [`TSLINK_TAG_SCOPE`](#tslink_tag_scope) | `exact` or `prefix` | `exact` |
| [`TSLINK_LOGIN_SERVER`](#tslink_login_server) | An `http` or `https` URL | Tailscale's control server |
| [`TSLINK_WIREGUARD_PORTS`](#tslink_wireguard_ports) | A port range `first-last` | `61000-65535` |
| [`TSLINK_HOST_ENDPOINTS`](#tslink_host_endpoints) | `auto`, `off`, or up to 3 IPv4 addresses | `auto` |
| [`TSLINK_LOG_LEVEL`](#tslink_log_level) | `debug`, `info`, `warn` or `error` | `info` |
| [`TS_DEBUG_ACME_DIRECTORY_URL`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A URL | Let's Encrypt |
| [`SSL_CERT_FILE`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A file in the plugin | The system's root certificates |

## TS_AUTHKEY

The credential for networks without the option `tslink.authkey` on hosts without a cluster credential. See
[Precedence](credentials.md#precedence).

## shared.source

The source of the plugin's `shared` mount. tslink keeps HTTPS certificates and their ACME account key in `certs/` in
this directory. Set it to a volume that all hosts share. tslink then gets the certificate of a Service once for the
cluster, and not once for each host.

To move to another directory, copy `certs/` with its contents into the new one, not the files in it: tslink looks only
in `certs/`, and gets a certificate again if it is not there.

To check which source the plugin uses, read `.Config.Mounts` in `docker plugin inspect tslink`. `docker plugin set`
changes the source there, and the plugin runs with it. `.Settings.Mounts` can still show the source from the install.
`docker plugin upgrade` resets the source to the data directory: set it again after an upgrade, before you enable the
plugin.

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

The firewall rules stay when the plugin stops. For how they work, see
[Isolation](../explanation/architecture.md#isolation). For the guarantee that this setting supports, see `G1` in
[SECURITY.md](../../SECURITY.md).

## TSLINK_TAG_SCOPE

Sets which tags a stack can use with the cluster credential: `exact` or `prefix`. For the permitted tags, see
[Tag rules](credentials.md#tag-rules). For other values, tslink uses `exact` and logs a warning.

> [!WARNING]
> With `prefix`, a stack can use the base tag of a stack with a longer name. For example, stack `a` can use
> `tag:a-b`, the base tag of stack `a-b`. If a stack needs more than one tag, give its network its own
> `tslink.authkey`.

## TSLINK_LOGIN_SERVER

The URL of the control server that all nodes of the plugin log in to, for example a
[headscale](https://github.com/juanfont/headscale) server. Leave it empty for the control server of Tailscale. The URL
must use `http` or `https` and must have a hostname. For other values, the plugin does not start.

The setting applies to all networks on the host and all credentials, the cluster credential included. A network
cannot set a control server of its own.

## TSLINK_WIREGUARD_PORTS

Sets the UDP ports on the host that the tailscaled of containers use for their WireGuard traffic to other hosts. Each
container on the host gets its own port from the range, for all destinations. Thus peers on other hosts can connect
to it directly, without a DERP relay. The range needs one port for each container on the host.

Use ports from 1024 to 65535, outside the ephemeral port range of the kernel (`net.ipv4.ip_local_port_range`, by
default `32768 60999`), and not used by services on the host. The range cannot contain `41641`, the port of
tailscaled in the containers. For other values, tslink uses `61000-65535` and logs a warning. Containers keep their
port until they leave the network.

The ports only change the source port of traffic that leaves the host. They do not accept connections: the host
drops new UDP packets to the range before anything on it has sent a reply. Keep host services out of the range; one
listening there gets no unsolicited UDP.

## TSLINK_HOST_ENDPOINTS

Sets the host addresses containers' tailscaled advertise, each with the container's external port (see
[`TSLINK_WIREGUARD_PORTS`](#tslink_wireguard_ports)), so nodes on hosts that share a network go direct over it instead
of through a DERP relay.

| Value | Effect |
| --- | --- |
| `auto` | The private IPv4 addresses of the host's NICs, bonds and VLANs that are up, in interface order, at most 3. Bridges (Docker's, libvirt's), veths, tun, wireguard and vxlan devices are never included. |
| `off` | Nothing is advertised. |
| Up to 3 IPv4 addresses, comma-separated | Advertised as given. |
| Other values | tslink keeps `auto` and logs a warning. |

With `auto` or explicit addresses, the host's private addresses are sent to every peer that gets the node's
endpoints, the same as the host's own tailscaled does; `off` advertises none.

If the shared network sits on a bridge (for example `br0` holding the NIC), `auto` does not find it: give the
addresses explicitly.

tailscaled reads these addresses when it starts; tslink restarts a container's tailscaled when its address or port
changes, so a change is picked up within a minute.

## TSLINK_LOG_LEVEL

Sets the lowest level the plugin logs to `plugin.log` and the Docker daemon's log: `debug`, `info`, `warn` or
`error`. For other values, tslink uses `info` and logs a warning. Set `debug` to follow what the plugin does with each
event and endpoint, for example while investigating a problem; it logs much more.

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
