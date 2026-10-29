---
title: Network options
description: Options of `docker network create`.
category: reference
tags: [network]
---

# Network options

Network options configure all containers on a tslink network. Set them with `--opt` on `docker network create`, or
with `driver_opts` in a Compose or stack file. You cannot change an option after you create the network.
`docker network create` fails for an option that starts with `tslink.` and is not in this table, so a misspelt
option does not leave the network with another credential or other tags than you meant. tslink takes no
`--driver-opt` options on `docker network connect`: set a container's options as [labels](container-labels.md).

| Option | Value | Default |
| --- | --- | --- |
| [`tslink.authkey`](#tslinkauthkey) | A credential | Required, except in the conditions below |
| [`tslink.tags`](#tslinktags) | Comma-separated ACL tags | None |
| [`tslink.ephemeral`](#tslinkephemeral) | `true` or `false` | From the credential |
| [`com.docker.network.driver.mtu`](#comdockernetworkdrivermtu) | 576 to 65535 | 1500 |

## tslink.authkey

The credential that the nodes of the network register with: an auth key (`tskey-auth-…`) or an OAuth client secret
(`tskey-client-…`).

If the option is not set, tslink uses the cluster credential or `TS_AUTHKEY`. See
[Precedence](credentials.md#precedence).

| Condition | Effect |
| --- | --- |
| No credential is available | `docker network create` fails. |
| The credential is an auth key with parameters appended (`?…`) | `docker network create` fails. Only OAuth client secrets take parameters. |
| The credential is an OAuth client secret with an unknown parameter, or a value of `ephemeral` or `preauthorized` that is not a boolean (empty included) | `docker network create` fails. |

## tslink.tags

The ACL tags of all nodes on the network, separated by commas, for example `tag:web,tag:prod`.

| Condition | Effect |
| --- | --- |
| The option is set | It overrides the `tslink.tags` label of each container. |
| The network uses the cluster credential | The option is required. |
| The credential is an OAuth client secret | The option or the `tslink.tags` label of the container is required. Without tags, the node does not start. |
| The network uses the cluster credential | Each tag must agree with the stack name. See [Tag rules](credentials.md#tag-rules). |

## tslink.ephemeral

Tells tslink if the nodes of the network are ephemeral. Use it for credentials that do not tell tslink, such as auth
keys and headscale's keys. For the effect, see [Ephemeral nodes](credentials.md#ephemeral-nodes).

| Condition | Effect |
| --- | --- |
| The value is not exactly `true` or `false` | `docker network create` fails. |
| The credential is an OAuth client secret without an `ephemeral` parameter | tslink appends `?ephemeral=<value>` to it. |
| The OAuth client secret has an `ephemeral` parameter with a different value | `docker network create` fails. |
| The network uses the cluster credential, and the value is `false` | `docker network create` fails. |

> [!WARNING]
> Set `true` only with a credential that creates ephemeral nodes. With `true`, tslink deletes the state of a node
> when its container stops, and the node loses its identity.

## com.docker.network.driver.mtu

The MTU of the interface that tslink adds to the container. `docker network create` fails for a value that is not a
number from 576 to 65535.

## Removed options

| Option | Status |
| --- | --- |
| `tslink.loginserver` | `docker network create` fails. On a network that an earlier version created with it, `docker run` or the task fails with an error that names `TSLINK_LOGIN_SERVER`, and a container that was running when the plugin restarted gets the status `failed`, which names it. Recreate the network without it, and set the control server with the plugin setting [`TSLINK_LOGIN_SERVER`](plugin-settings.md#tslink_login_server). |

## Stack networks

A network that `docker stack deploy` creates has the label `com.docker.stack.namespace`. tslink starts Tailscale on
such a network only for the containers of the same stack. It does not start Tailscale for containers of other stacks,
or for containers outside a stack.

The state directories of stack containers are in `by-stack/<stack>/<hostname>/`. See
[Files and paths](files-and-paths.md#data-directory).
