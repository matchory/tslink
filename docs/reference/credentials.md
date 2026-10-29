---
title: Credentials
description: The credentials tslink accepts, which one a network uses, when its nodes are ephemeral, and the tag rules.
category: reference
tags: [credentials, oauth, auth-key, ephemeral, tags]
---

# Credentials

A credential is what lets a node register in your tailnet. tslink accepts an auth key or an OAuth client secret,
from three sources, and uses it only to register: a node that's logged in doesn't need it again.

## Credential types

| Type | Format | Nodes are ephemeral | Needs `tslink.tags` |
| --- | --- | --- | --- |
| Auth key | `tskey-auth-…` | Unknown to tslink; see [ephemeral nodes](#ephemeral-nodes) | No |
| OAuth client secret | `tskey-client-…`, with optional parameters | Yes, unless `?ephemeral=false` is appended or the network has `tslink.ephemeral=false` | Yes |
| Cluster credential | An OAuth client secret in a file, without parameters | Always | Yes |

Three properties of an auth key matter to tslink:

- **Ephemeral:** the control server removes the node some time after it goes offline.
- **Reusable:** more than one node can register with the key, so every container on a network can share it.
- **Pre-approved:** the nodes don't wait for approval in the admin console.

Parameters such as `?ephemeral=false` and `?preauthorized=true` belong on OAuth client secrets only. A network whose
auth key has parameters can't be created.

## Precedence

A network takes its credential from the first of these that's set:

1. The network option [`tslink.authkey`](network-options.md#tslinkauthkey).
2. The cluster credential file on the host.
3. The plugin setting [`TS_AUTHKEY`](plugin-settings.md#ts_authkey).

If none is, `docker network create` fails. Whichever it is, the node registers with the control server of the plugin
setting [`TSLINK_LOGIN_SERVER`](plugin-settings.md#tslink_login_server).

## Ephemeral nodes

When an ephemeral node's container stops, tslink logs the node out and deletes its state directory, so the node
leaves your tailnet and its hostname is free again. Any other node keeps its state directory when its container
stops, and stays in your tailnet, offline, ready to come back as the same node.

A network's `tslink.ephemeral` option decides, if it's set. Otherwise the credential does:

| Credential | The node is ephemeral |
| --- | --- |
| Cluster credential | Always. Its networks can't set `tslink.ephemeral=false`. |
| OAuth client secret | Unless the secret has `?ephemeral=false` |
| Auth key | Never, as far as tslink knows: an auth key doesn't tell tslink whether it creates ephemeral nodes |

## Optional API scopes

tslink uses an OAuth client secret to create auth keys through Tailscale's API. If the OAuth client also has the
scope `services:read`, tslink uses it for one more job: it reads the ports of each of a container's Services, and
reports `ports-uncovered` if the container doesn't serve all of them (see
[conditions for ready](readiness-endpoint.md#conditions-for-ready)).

Without the scope, tslink logs once per Service that it can't read the ports, and checks them only if the node's
capabilities list them. An auth key gives no access to the API.

## Cluster credential

One OAuth client secret for every stack of a Swarm, in a file on each host.

| Property | Value |
| --- | --- |
| File | `oauth-client.secret` in the data directory, on each host |
| Content | The OAuth client secret (`tskey-client-…`), without parameters |
| When tslink reads it | Whenever a node registers, so you rotate it by replacing the file |
| Nodes | Ephemeral and pre-approved |
| Networks | Only stack networks can use it. They have to set `tslink.tags`, and can't set `tslink.ephemeral=false`. |

## Tag rules

These rules apply to networks that use the cluster credential. `<stack>` is the stack's name, from the network's
`com.docker.stack.namespace` label, which `docker stack deploy` sets and a stack file can't override.

| `TSLINK_TAG_SCOPE` | Tags a stack can use |
| --- | --- |
| `exact` (default) | `tag:<stack>` |
| `prefix` | `tag:<stack>` and `tag:<stack>-*` |

A container whose tag isn't allowed gets no node, and tslink doesn't retry.

In the tailnet policy, the OAuth client has to own every stack's tag. If the OAuth client has the tag `tag:tslink`,
make `tag:tslink` the owner of each stack's tag:

```json
"tagOwners": {
  "tag:billing": ["tag:tslink"]
}
```

The [security model](../explanation/security-model.md) explains why the rules are what they are.
