# Deploy on Swarm

## Goal

Deploy a stack whose containers join the tailnet as nodes with the tag of the stack.

## Prerequisites

- tslink is installed on each Swarm node. See [Provision a host](provision-a-node.md).
- You can edit the tailnet policy and create OAuth clients in the Tailscale admin console.
- The stack name starts with a letter or a digit, and contains only letters, digits, `.`, `_` and `-`.

Select one of two credential models:

| Model | Credential | Use it when |
| --- | --- | --- |
| A: cluster credential | One OAuth client for all stacks, in a file on each host | Each stack needs one tag, `tag:<stack>` |
| B: OAuth client for each stack | One OAuth client for each stack, in the network of the stack | A stack needs other tags, or you want no credential on the hosts |

Both models can be used in one cluster. A network with `tslink.authkey` uses its own credential. For the security
properties of the two models, see [Security model](../explanation/security-model.md).

## Steps

### Option A: cluster credential

1. In the tailnet policy, make the OAuth client the owner of the tag of the stack. In this example, the OAuth client
   has the tag `tag:tslink`, and the stack is `billing`:

   ```json
   "tagOwners": {
     "tag:tslink": ["autogroup:admin"],
     "tag:billing": ["tag:tslink"]
   }
   ```

2. In the admin console, create an OAuth client with the scope to write auth keys, and with the tag `tag:tslink`.

3. On each host, write the OAuth client secret to the cluster credential file. Do not append parameters:

   ```bash
   install -m 0600 /dev/stdin /var/lib/docker-plugins/tailscale/oauth-client.secret <<<"$SECRET"
   ```

4. In the stack file, give the network only its tag:

   ```yaml
   networks:
     tailnet:
       driver: tslink:latest
       driver_opts:
         tslink.tags: tag:billing
   ```

5. Deploy the stack:

   ```bash
   docker stack deploy -c billing.yml billing
   ```

### Option B: OAuth client for each stack

1. In the admin console, create an OAuth client for the stack. Let it apply only the tags of the stack.

2. Keep the OAuth client secret in the secret store of your deployment tool, for example as
   `TSLINK_OAUTH_SECRET`.

3. In the stack file, give the network the secret and the tags:

   ```yaml
   networks:
     tailnet:
       driver: tslink:latest
       driver_opts:
         tslink.authkey: ${TSLINK_OAUTH_SECRET}?ephemeral=true&preauthorized=true
         tslink.tags: tag:svc-billing
   ```

4. Deploy the stack from your deployment tool, with the secret in the environment:

   ```bash
   docker stack deploy -c billing.yml billing
   ```

### Rotate the credential

- **Option A:** Replace `oauth-client.secret` on each host. tslink reads the file each time a node registers. You do
  not recreate the networks.
- **Option B:** Docker cannot change the options of a network. Remove the stack, and deploy it again with the new
  secret. The stack is unavailable for some seconds. To prevent this, deploy the stack under a new stack name with
  the new secret, and remove the old stack when the new stack serves.

OAuth client secrets do not expire. If you revoke a secret, the running nodes stay online. New containers cannot
register until the network has a valid secret.

## Verify

1. Make sure that the tasks run:

   ```bash
   docker stack ps billing
   ```

2. In the admin console, make sure that the nodes of the stack have the tags of the network. In Option A, the tag is
   `tag:billing`.

3. On a host of the stack, make sure that each status file of the stack shows `running`. See
   [Monitor tslink](monitor.md).

## Next steps

- [Expose a service](expose-a-service.md)
- [Update without downtime](zero-downtime-updates.md)
- [Credentials](../reference/credentials.md)
