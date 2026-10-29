---
title: Network options
description: Every option of a tslink network, set with docker network create or driver_opts, with its values and conditions.
category: reference
tags: [network, options, credentials, tags, mtu]
---

# Network options

Network options configure every container on a tslink network: the credential they register with, their tags, and
the MTU. Set them with `--opt` on `docker network create`, or under `driver_opts` in a Compose or stack file.

Docker can't change a network's options once it exists, so to change one, recreate the network.
`docker network create` refuses an option that starts with `tslink.` but isn't listed here, so a typo can't leave a
network with a credential or tags you didn't mean. tslink takes no `--driver-opt` on `docker network connect`; a
container's own settings are [labels](container-labels.md).

| Option | Value | Default |
| --- | --- | --- |
| [`tslink.authkey`](#tslinkauthkey) | A credential | Required, unless another credential applies |
| [`tslink.tags`](#tslinktags) | Comma-separated ACL tags | None |
| [`tslink.ephemeral`](#tslinkephemeral) | `true` or `false` | From the credential |
| [`com.docker.network.driver.mtu`](#comdockernetworkdrivermtu) | 576 to 65535 | 1500 |

## tslink.authkey

The credential the network's nodes register with: an auth key (`tskey-auth-…`) or an OAuth client secret
(`tskey-client-…`).

| Value | Default |
| --- | --- |
| A credential | The cluster credential or `TS_AUTHKEY`, see [precedence](credentials.md#precedence) |

`docker network create` fails when:

- no credential is available at all;
- an auth key has parameters appended (`?…`): only OAuth client secrets take parameters;
- an OAuth client secret has an unknown parameter, or an `ephemeral` or `preauthorized` value that isn't a boolean,
  an empty value included.

## tslink.tags

The ACL tags of every node on the network, separated by commas, such as `tag:web,tag:prod`.

| Value | Default |
| --- | --- |
| Comma-separated tags | None |

- When it's set, it overrides each container's [`tslink.tags`](container-labels.md#tslinktags) label.
- With the cluster credential, it's required, and every tag has to agree with the stack's name; see
  [tag rules](credentials.md#tag-rules).
- With an OAuth client secret, either this option or the container's `tslink.tags` label is required. Without tags,
  the node doesn't start.

## tslink.ephemeral

Tells tslink whether the network's nodes are ephemeral, for credentials that don't say so themselves, such as auth
keys and headscale's keys. [Ephemeral nodes](credentials.md#ephemeral-nodes) explains what changes.

| Value | Default |
| --- | --- |
| `true` or `false` | From the credential |

- Any value other than exactly `true` or `false` fails `docker network create`.
- On an OAuth client secret without an `ephemeral` parameter, tslink appends `?ephemeral=<value>`.
- An OAuth client secret whose `ephemeral` parameter says otherwise fails `docker network create`.
- With the cluster credential, `false` fails `docker network create`.

> [!WARNING]
> Set `true` only with a credential that really creates ephemeral nodes. With `true`, tslink deletes a node's state
> when its container stops, and the node loses its identity.

## com.docker.network.driver.mtu

The MTU of the interface tslink adds to the container. Match it to Docker's MTU on hosts whose network has an MTU
below 1500; see [Provision a host](../guides/provision-a-node.md).

| Value | Default |
| --- | --- |
| A number from 576 to 65535 | 1500 |

Any other value fails `docker network create`.

## Removed options

`tslink.loginserver` is gone: set the control server for the whole plugin with
[`TSLINK_LOGIN_SERVER`](plugin-settings.md#tslink_login_server) instead.

- `docker network create` with the option fails.
- On a network an earlier version created with it, `docker run` or the task fails with an error that names
  `TSLINK_LOGIN_SERVER`. A container that was already running when the plugin restarted gets the status `failed`,
  with the same pointer.

Recreate such a network without the option.

## Stack networks

`docker stack deploy` labels the networks it creates with `com.docker.stack.namespace`. On such a network, tslink
starts Tailscale only for containers of the same stack, never for another stack's containers or for containers
outside any stack, so one stack can't borrow another's network and identity.

Stack containers keep their state directories in `by-stack/<stack>/<hostname>/`; see
[files and paths](files-and-paths.md#data-directory).
