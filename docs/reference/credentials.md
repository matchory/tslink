# Credentials

A credential lets a node register in the tailnet. tslink accepts an auth key or an OAuth client secret, from three
sources.

## Credential types

| Type | Format | Nodes are ephemeral | Needs `tslink.tags` |
| --- | --- | --- | --- |
| Auth key | `tskey-auth-…` | Not known to tslink. See [Ephemeral nodes](#ephemeral-nodes). | No |
| OAuth client secret | `tskey-client-…`, with optional parameters | Yes, unless `?ephemeral=false` is appended or the network has `tslink.ephemeral=false` | Yes |
| Cluster credential | An OAuth client secret in a file, without parameters | Yes, always | Yes |

Properties of an auth key that tslink uses:

| Property | Effect |
| --- | --- |
| Ephemeral | The control server removes the node some time after it goes offline. |
| Reusable | More than one node can register with the key. |
| Pre-approved | The nodes do not need approval in the admin console. |

Parameters such as `?ephemeral=false` and `?preauthorized=true` are permitted only on OAuth client secrets. A
network with an auth key that has parameters cannot be created.

## Precedence

tslink takes the credential of a network from the first source that is set:

1. The network option `tslink.authkey`.
2. The cluster credential file on the host.
3. The plugin setting `TS_AUTHKEY`.

If no source is set, `docker network create` fails.

## Ephemeral nodes

When the container of an ephemeral node stops, tslink logs the node out and deletes its state directory. The node is
then removed from the tailnet, and its hostname is free. When the container of another node stops, tslink keeps the
state directory, and the node stays in the tailnet, offline.

If the network has `tslink.ephemeral`, its value decides. Otherwise the credential decides:

| Credential | The node is ephemeral if |
| --- | --- |
| Cluster credential | Always. The network cannot have `tslink.ephemeral=false`. |
| OAuth client secret | The secret has no `ephemeral` parameter, or it has `?ephemeral=true` |
| Auth key | Never. An auth key does not tell tslink if it creates ephemeral nodes. |

## Cluster credential

The cluster credential is one OAuth client secret for all stacks of a Swarm.

| Property | Value |
| --- | --- |
| File | `oauth-client.secret` in the data directory, on each host |
| Content | The OAuth client secret (`tskey-client-…`), without parameters |
| When tslink reads it | Each time a node registers. To rotate the credential, replace the file. |
| Nodes | Ephemeral and pre-approved |
| Networks | Only networks of a stack can use it. They must have `tslink.tags`, and they cannot have `tslink.ephemeral=false`. |

## Tag rules

These rules apply to networks that use the cluster credential. `<stack>` is the name of the stack, from the label
`com.docker.stack.namespace` of the network.

| `TSLINK_TAG_SCOPE` | Permitted tags |
| --- | --- |
| `exact` (default) | `tag:<stack>` |
| `prefix` | `tag:<stack>` and `tag:<stack>-*` |

If a tag is not permitted, the container does not get a node, and tslink does not try again.

The OAuth client must own the tags of all stacks in the tailnet policy. For example, if the OAuth client has the tag
`tag:tslink`, make `tag:tslink` the owner of each stack tag:

```json
"tagOwners": {
  "tag:billing": ["tag:tslink"]
}
```

For the security reasons of these rules, see [Security model](../explanation/security-model.md).
