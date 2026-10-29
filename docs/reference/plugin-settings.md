---
title: Plugin settings
description: Every setting of the tslink plugin, set with docker plugin set, with its values, default and effect.
category: reference
tags: [plugin, settings, isolation, control-server, ports]
---

# Plugin settings

Plugin settings configure tslink for every network on one host. Set them with `docker plugin set` while the plugin is
disabled, as [change a setting](../guides/install-and-upgrade.md#change-a-setting) shows.

| Setting | Values | Default |
| --- | --- | --- |
| [`TS_AUTHKEY`](#ts_authkey) | A credential | None |
| [`shared.source`](#sharedsource) | A directory on the host | `/var/lib`, which sets none |
| [`TSLINK_ISOLATE_HOST_TAILNET`](#tslink_isolate_host_tailnet) | `true` or `false` | `true` |
| [`TSLINK_TAG_SCOPE`](#tslink_tag_scope) | `exact` or `prefix` | `exact` |
| [`TSLINK_LOGIN_SERVER`](#tslink_login_server) | An `https` URL, or `http` with an IP address | Tailscale's control server |
| [`TSLINK_WIREGUARD_PORTS`](#tslink_wireguard_ports) | A port range `first-last` | `61000-65535` |
| [`TSLINK_HOST_ENDPOINTS`](#tslink_host_endpoints) | `auto`, `off`, or up to 3 IPv4 addresses | `auto` |
| [`TSLINK_LOG_LEVEL`](#tslink_log_level) | `debug`, `info`, `warn` or `error` | `info` |
| [`TS_DEBUG_ACME_DIRECTORY_URL`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A URL | Let's Encrypt |
| [`SSL_CERT_FILE`](#ts_debug_acme_directory_url-and-ssl_cert_file) | A file in the plugin | The system's root certificates |

## TS_AUTHKEY

The credential for networks without their own `tslink.authkey`, on hosts without a cluster credential.

| Value | Default |
| --- | --- |
| A credential | None |

[Precedence](credentials.md#precedence) says which credential wins when several apply.

## shared.source

The source of the plugin's `shared` mount, where tslink keeps HTTPS certificates and their ACME account key, in
`certs/`. Point it at a volume every host shares, and tslink gets each Service's certificate once for the cluster
instead of once per host.

| Value | Default |
| --- | --- |
| A directory on the host | `/var/lib`, which sets no shared directory |

With the default, tslink keeps certificates in `certs/` in the data directory, and doesn't keep the mount.

To move to another directory, copy the `certs/` directory itself, with its contents, into the new one, not just the
files in it: tslink looks only in `certs/`, and gets a certificate again if it isn't there.

To see which source the plugin uses, read `.Config.Mounts` in `docker plugin inspect tslink`: `docker plugin set`
changes the source there, and the plugin runs with it, while `.Settings.Mounts` can still show the one from the
install. `docker plugin upgrade` resets the source to the default, so set it again after an upgrade, before you
enable the plugin.

> [!WARNING]
> The directory holds every Service's private key. Make it available to the hosts of the cluster only.

## TSLINK_ISOLATE_HOST_TAILNET

Keeps containers from reaching the tailnet through the host's own tailscaled. It covers every container on the host,
on tslink networks and on any other network.

| Value | Default |
| --- | --- |
| `true` or `false` | `true` |

- `true`: containers can't reach the tailnet through the host. DNS queries to `100.100.100.100` still pass.
- `false`: containers can reach the tailnet as the host. Use it only if containers on the host have to use the host's
  tailscaled.
- Any other value: tslink keeps `true`, and logs a warning.

The firewall rules stay in place when the plugin stops. [Isolation](../explanation/architecture.md#isolation)
explains how they work, and `G1` in [SECURITY.md](../../SECURITY.md) is the guarantee they back.

## TSLINK_TAG_SCOPE

Which tags a stack can use with the cluster credential.

| Value | Default |
| --- | --- |
| `exact` or `prefix` | `exact` |

[Tag rules](credentials.md#tag-rules) lists the tags each value allows. For any other value, tslink uses `exact`, and
logs a warning.

> [!WARNING]
> With `prefix`, a stack can use the base tag of a stack with a longer name: stack `a` can use `tag:a-b`, the base tag
> of stack `a-b`. If a stack needs more than one tag, give its network its own `tslink.authkey` instead.

## TSLINK_LOGIN_SERVER

The control server every node of the plugin logs in to, such as a [headscale](https://github.com/juanfont/headscale)
server.

| Value | Default |
| --- | --- |
| An `https` URL with a hostname or IP address, or an `http` URL with an IP address | Tailscale's control server |

`http://10.0.0.5:8080` and `http://[fd00::5]:8080` are fine; the URL can't contain a username or password. With any
other value, `http` with a hostname such as `localhost` included, the plugin doesn't start.

> [!NOTE]
> tailscaled resolves the control server's name with the container's DNS servers, which a stack can set. Over
> `https`, the server's certificate proves who it is. Over `http`, nothing does, so a stack could point the name at a
> server of its own and receive its nodes' keys. That's why `http` needs an IP address.

The setting applies to every network on the host and every credential, the cluster credential included. A network
can't choose a control server of its own.

## TSLINK_WIREGUARD_PORTS

The UDP ports on the host that containers' tailscaled use for their WireGuard traffic to other hosts. Each container
gets a port of its own from the range, for every destination, so peers on other hosts can connect to it directly
instead of through a DERP relay.

| Value | Default |
| --- | --- |
| A port range `first-last`, with one port per container on the host | `61000-65535` |

Use ports from 1024 to 65535, outside the kernel's ephemeral port range (`net.ipv4.ip_local_port_range`, by default
`32768 60999`), and not used by any service on the host. The range can't contain `41641`, the port tailscaled uses
inside containers. For any other value, tslink uses `61000-65535` and logs a warning. A container keeps its port until
it leaves the network.

The ports only rewrite the source port of traffic leaving the host. They don't accept connections: the host drops new
UDP packets to the range until something behind it has sent a reply, so a host service listening in the range gets
no unsolicited UDP.

## TSLINK_HOST_ENDPOINTS

The host addresses that containers' tailscaled advertise, each with the container's external port from
[`TSLINK_WIREGUARD_PORTS`](#tslink_wireguard_ports), so nodes on hosts that share a network connect directly over it
instead of through a DERP relay.

| Value | Default |
| --- | --- |
| `auto`, `off`, or up to 3 comma-separated IPv4 addresses | `auto` |

- `auto`: the private IPv4 addresses of the host's NICs, bonds and VLANs that are up, in interface order, at most 3.
  Bridges (Docker's, libvirt's), veths, tun, WireGuard and VXLAN devices never count.
- `off`: nothing is advertised.
- Addresses: advertised as given.
- Any other value: tslink keeps `auto`, and logs a warning.

With `auto` or explicit addresses, every peer that gets the node's endpoints learns the host's private addresses, as
it would from the host's own tailscaled.

If the shared network sits on a bridge, such as a `br0` holding the NIC, `auto` doesn't find it: give the addresses
explicitly.

tailscaled reads the addresses when it starts. tslink restarts a container's tailscaled when its address or port
changes, so a change takes effect within a minute.

## TSLINK_LOG_LEVEL

The lowest level the plugin logs, to `plugin.log` and the Docker daemon's log.

| Value | Default |
| --- | --- |
| `debug`, `info`, `warn` or `error` | `info` |

For any other value, tslink uses `info`, and logs a warning. `debug` shows what the plugin does with every event and
endpoint, which helps when you investigate a problem, and logs a lot more.

## TS_DEBUG_ACME_DIRECTORY_URL and SSL_CERT_FILE

For tests only. `TS_DEBUG_ACME_DIRECTORY_URL` points certificate requests at a different ACME directory, such as
Let's Encrypt's staging environment, and `SSL_CERT_FILE` sets a root certificate bundle that trusts it.

| Value | Default |
| --- | --- |
| A URL; a file in the plugin | Let's Encrypt; the system's root certificates |

## Removed settings

tslink runs only the Tailscale binaries in its own image (see [binaries](../explanation/architecture.md#binaries)),
so the settings that chose another version are gone:

- `TS_VERSION`: tslink ignores any value other than `bundled`, and logs a warning.
- `TS_PATH`: tslink ignores it, and logs a warning.

## Internal settings

Leave `TS_DATA_DIR`, `TS_SHARED_DIR` and `DOCKER_HOST` alone. The plugin's configuration sets them, and
`docker plugin set` can't change them.
