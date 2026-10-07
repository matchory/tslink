# Credentials and tags for Swarm services

How a Swarm task gets its tailnet identity without letting a task choose
someone else's. Decided 2026-10-07, after the
[Swarm spike](swarm-spike-2026-10-06.md).

## Decision

Each stack gets its own Tailscale OAuth client, allowed to apply only that
stack's tags. CI injects the client secret into the stack's network
definition at deploy time; the tags sit next to it:

```yaml
networks:
  tailnet:
    driver: ghcr.io/matchory/tslink:latest
    driver_opts:
      tslink.authkey: ${TSLINK_OAUTH_SECRET}?ephemeral=true&preauthorized=true
      tslink.tags: tag:svc-billing
```

- **Tags come from the network.** `tslink.tags` on the network overrides the
  container label, so a task cannot pick its own tags. Tailscale rejects tags
  the OAuth client does not own, so a stack that declares another stack's tags
  fails to join.
- **A task must belong to the network's stack.** A network created by
  `docker stack deploy` carries `com.docker.stack.namespace`. tslink starts
  tailscaled only for tasks with the same label, so stack B cannot join
  stack A's network as an external network and inherit A's identity.
  `docker stack deploy` sets that label after applying the compose file's own
  labels, so a stack file cannot claim another stack's name. Anyone with
  direct Docker API access can, but that access is already root on the node.
- **Node state is namespaced by stack** (`by-stack/<stack>/<hostname>`), so a
  task cannot reuse or wipe another stack's tailscaled state by choosing its
  hostname.
- **The secret never enters the application container.** It is visible to
  `docker network inspect`, i.e. to holders of the Docker API, and tslink does
  not log option values.

Rotating the secret means recreating the network, which takes the stack down
briefly. OAuth client secrets do not expire, so this should be rare.

### Rejected

- **Policy file on each node** mapping stacks to credentials: couples cluster
  deployments to node provisioning.
- **Docker secrets:** Swarm hands secret contents only to the tasks they are
  mounted into, so tslink would have to read the secret from the application's
  own container. A compromised application would then hold a non-expiring
  credential that can mint keys for its tags anywhere.
- **Plugin settings (`docker plugin set`):** per node, and changing them
  requires disabling the plugin.

## Postponed: workload identity federation

[Workload identity federation](https://tailscale.com/docs/features/workload-identity-federation)
would remove the per-stack secrets. Since v1.92.1, `tailscale up` accepts
`--client-id=<id>?ephemeral=true&preauthorized=true --id-token=file:<path>`
and exchanges a signed OIDC token for a node key. Each federated credential
matches the token's `sub` (wildcards allowed) and optional custom claims, and
fixes the tags. Custom issuers work if Tailscale can reach their discovery
document and JWKS over the public internet.

Swarm has no token issuer, and the Hetzner Cloud servers we run on have no
workload identity, so we would need our own:

- A token issuer service on the managers, holding one signing key for the
  cluster as a Docker secret mounted only into that service. It serves its
  discovery document and JWKS at a public HTTPS URL (behind the ingress, or as
  static files in object storage).
- tslink asks the issuer for a token for a given task, authenticating with the
  node certificate Swarm issues to every node (its CN is the node ID). The
  issuer checks with the manager that the task runs on that node, reads its
  stack, and signs a short-lived token with `sub` set to
  `swarm:<cluster>:<stack>`.
- The stack's network then carries only `tslink.clientid`, which is not a
  secret.

The trust boundary stays where it is today: whoever can deploy into a stack,
or is root on a node running its tasks, can act as that stack. Costs are a new
component to run, a public URL, and a dependency on the issuer being up when
tasks start. The network-level tags and stack check above carry over
unchanged; only the credential source would change.

Before building it, prove it end to end: a minimal issuer, one federated
credential, and one `tailscale up --id-token` from a task.
