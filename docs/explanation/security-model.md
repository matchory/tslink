# Security model

This page tells how tslink keeps one stack from using the identity, tags or credential of another stack. The
guarantees, the attackers they defend against and the environment properties they rely on are in
[SECURITY.md](../../SECURITY.md). This page gives the reasons for the design.

## Trust boundaries

Access to the Docker API is equal to root access on the host. A person with this access can read each credential and
each node key on the host. tslink does not defend against this person.

On a Swarm, a person who can deploy a stack under a name acts as that stack. A person with root access on a host acts
as each stack that has a container on the host. tslink keeps all other actors in their stack.

## Tags come from the network

The option `tslink.tags` of a network overrides the label `tslink.tags` of the container. Thus a container cannot
select its own tags. With an OAuth client for each stack, the control server refuses tags that the OAuth client does
not own. Thus a stack that declares the tags of another stack cannot register.

## Containers must be in the stack of the network

`docker stack deploy` gives each network of a stack the label `com.docker.stack.namespace`. tslink starts tailscaled
only for containers with the same label. Thus stack B cannot use the network of stack A as an external network, and
cannot get the identity of stack A.

`docker stack deploy` sets the label after it applies the labels of the stack file. Thus a stack file cannot claim the
name of another stack. A person with access to the Docker API can set the label, but this person is already root on
the host.

## State directories are in the directory of the stack

The state directory of a stack container is `by-stack/<stack>/<hostname>`. Thus a container cannot use or delete the
state of another stack when it selects a hostname.

## Where credentials are visible

| Place | Visible to |
| --- | --- |
| Network options (`docker network inspect`) | Persons with access to the Docker API |
| Plugin settings (`docker plugin inspect`) | Persons with access to the Docker API |
| The Raft store of the Swarm managers | Persons with root access on a manager |
| `oauth-client.secret` | Root on each host |
| The application container, logs, status files and the output of `tslink diag` | Never. See `G5` in [SECURITY.md](../../SECURITY.md). |

## Cluster credential and OAuth clients for each stack

With an OAuth client for each stack, the control server stops a stack from using other tags. With the cluster
credential, one OAuth client owns the tags of all stacks, so the control server cannot stop this. tslink stops it:
a stack can use only `tag:<stack>`. See [Tag rules](../reference/credentials.md#tag-rules).

The boundary stays the same: a person who can deploy a stack under a name acts as that stack. The difference is the
breadth of root access on a host. With the cluster credential, root on any host can read a credential for the tags of
all stacks. With an OAuth client for each stack, root on a host can read only the credentials of the stacks that run
there.

## Rotation and revocation

A credential is used only to register nodes. When you revoke it, the running nodes stay online. They also stay
online after a plugin restart or a reboot, because a node that is still logged in starts without its credential.
New containers cannot register until the network has a valid credential.

To rotate the secret of an OAuth client for a stack, you recreate the network. In the cluster test, the stack was
unavailable for 14 seconds, from `docker stack rm` until the new stack answered on the address of its Service.

## Alternatives that tslink does not use

| Alternative | Why tslink does not use it |
| --- | --- |
| A policy file on each host that maps stacks to credentials | It makes cluster deployments depend on host provisioning. The cluster credential file contains no stack name, so it does not. |
| Docker secrets | Swarm gives a secret only to the tasks that mount it. tslink would read the secret from the application container. A compromised application would then have a credential that does not expire, and that can create keys for its tags on any host. |
| Plugin settings (`docker plugin set`) | They apply to one host, and a change requires that you disable the plugin. |

For workload identity federation, see [Limitations](limitations.md#workload-identity-federation).
