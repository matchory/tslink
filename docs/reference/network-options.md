# Network options

Network options configure all containers on a tslink network. Set them with `--opt` on `docker network create`, or
with `driver_opts` in a Compose or stack file. You cannot change an option after you create the network.

| Option | Value | Default |
| --- | --- | --- |
| [`tslink.authkey`](#tslinkauthkey) | A credential | Required, except in the conditions below |
| [`tslink.tags`](#tslinktags) | Comma-separated ACL tags | None |
| [`tslink.ephemeral`](#tslinkephemeral) | `true` or `false` | From the credential |
| [`tslink.loginserver`](#tslinkloginserver) | An `http` or `https` URL | Tailscale's control server |
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

## tslink.loginserver

The URL of a control server other than Tailscale's, for example [headscale](https://github.com/juanfont/headscale).
tailscaled logs in to this server. The URL must use `http` or `https` and must have a hostname.

## com.docker.network.driver.mtu

The MTU of the interface that tslink adds to the container. tslink ignores a value that is not a number from 576 to
65535, and uses 1500.

## Stack networks

A network that `docker stack deploy` creates has the label `com.docker.stack.namespace`. tslink starts Tailscale on
such a network only for the containers of the same stack. It does not start Tailscale for containers of other stacks,
or for containers outside a stack.

The state directories of stack containers are in `by-stack/<stack>/<hostname>/`. See
[Files and paths](files-and-paths.md#data-directory).
