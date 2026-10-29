---
title: Security model
description: How tslink keeps one stack from using another's identity, tags or credential, whom you have to trust, and why.
category: explanation
tags: [security, stacks, credentials, tags, isolation]
---

# Security model

tslink's main promise on a shared Swarm is that one stack can't act as another: it can't take another stack's
identity, tags or credential. This page explains how the design keeps that promise, and where its limits are. The
guarantees themselves, the attackers they defend against and the environment properties they rely on are listed in
[SECURITY.md](../../SECURITY.md).

## Trust boundaries

Access to the Docker API is root on the host, and whoever has it can read every credential and node key there.
tslink doesn't try to defend against them.

On a Swarm, whoever can deploy a stack under a name acts as that stack, and root on a host acts as every stack with a
container on that host. Everyone else stays inside their own stack.

## Tags come from the network

A network's [`tslink.tags`](../reference/network-options.md#tslinktags) option overrides the containers'
`tslink.tags` labels, so a container can't choose its own tags. With an OAuth client per stack, the control server
also refuses any tag the OAuth client doesn't own, so a stack that claims another stack's tags can't even register.

## Containers must be in the network's stack

`docker stack deploy` labels every network of a stack with `com.docker.stack.namespace`, and tslink starts tailscaled
only for containers with the same label. So stack B can't attach to stack A's network as an external network, and
can't pick up stack A's identity that way.

`docker stack deploy` sets the label after it applies the stack file's labels, so a stack file can't claim another
stack's name. Someone with access to the Docker API can set the label, but they're root on the host already.

## State directories are in the directory of the stack

A stack container's state directory is `by-stack/<stack>/<hostname>`. Choosing a hostname therefore can't reach
another stack's state, to use it or to delete it.

## Where credentials are visible

| Place | Visible to |
| --- | --- |
| Network options (`docker network inspect`) | Anyone with access to the Docker API |
| Plugin settings (`docker plugin inspect`) | Anyone with access to the Docker API |
| The Swarm managers' Raft store | Root on a manager |
| `oauth-client.secret` | Root on each host |
| The application container, logs, status files and `tslink diag` output | No one: see `G5` in [SECURITY.md](../../SECURITY.md) |

## Cluster credential or an OAuth client per stack

With an OAuth client per stack, the control server keeps a stack to its own tags. With the cluster credential, one
OAuth client owns every stack's tags, so the control server can't; tslink does instead, by allowing a stack only
`tag:<stack>` (see [tag rules](../reference/credentials.md#tag-rules)).

The boundary between stacks is the same either way: whoever can deploy a stack under a name acts as that stack. What
differs is how far root on a single host reaches. With the cluster credential, root on any host can read a credential
for every stack's tags. With an OAuth client per stack, root on a host can read only the credentials of the stacks
running there.

## Rotation and revocation

A credential is only used to register nodes. When you revoke it, running nodes stay online, and they stay online
through a plugin restart or a reboot too, because a node that's still logged in starts without its credential. New
containers can't register until the network has a valid credential again.

Rotating a per-stack OAuth client's secret means recreating the network. In the cluster test, the stack was
unavailable for 14 seconds, from `docker stack rm` until the new stack answered on its Service's address.

## Alternatives tslink doesn't use

| Alternative | Why not |
| --- | --- |
| A policy file on each host that maps stacks to credentials | Cluster deployments would depend on host provisioning. The cluster credential file names no stack, so it doesn't. |
| Docker secrets | Swarm hands a secret only to the tasks that mount it, so tslink would have to read it from the application container. A compromised application would then hold a credential that doesn't expire, and that creates keys for its tags on any host. |
| Plugin settings (`docker plugin set`) | They apply to one host, and changing them means disabling the plugin. |

Workload identity federation would remove the secrets altogether;
[limitations](limitations.md#workload-identity-federation) sketches how it could work.
