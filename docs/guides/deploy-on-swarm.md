---
title: Deploy on Swarm
description: Deploy a stack whose containers join your tailnet as nodes with the stack's own tags and credential.
category: guide
tags: [swarm, stacks, credentials, oauth, tags]
---

# Deploy on Swarm

On a Swarm, each stack's containers join your tailnet as nodes with the stack's own tags, so your tailnet policy can
tell `billing` from `search`. This guide sets that up, with one credential for the whole cluster or one per stack.

## Before you start

- tslink is installed on every Swarm node. [Provision a host](provision-a-node.md) covers that.
- You can edit the tailnet policy and create OAuth clients in the Tailscale admin console.
- The stack's name starts with a letter or digit, and contains only letters, digits, `.`, `_` and `-`.

## Pick a credential model

| Model | Credential | Use it when |
| --- | --- | --- |
| A: cluster credential | One OAuth client for all stacks, in a file on each host | Each stack needs one tag, `tag:<stack>` |
| B: an OAuth client per stack | One OAuth client per stack, in the stack's network | A stack needs other tags, or root on one host mustn't get a credential for every stack's tags |

You can use both in one cluster: a network with its own `tslink.authkey` uses that. The
[security model](../explanation/security-model.md) compares what each model protects.

## Option A: one cluster credential

1. In the tailnet policy, make the OAuth client's tag the owner of each stack's tag. Here the OAuth client has the tag
   `tag:tslink`, and the stack is `billing`:

   ```json
   "tagOwners": {
     "tag:tslink": ["autogroup:admin"],
     "tag:billing": ["tag:tslink"]
   }
   ```

2. In the admin console, create an OAuth client that can write auth keys, with the tag `tag:tslink`.

3. On every host, write the OAuth client secret to the cluster credential file, as it is, with no parameters
   appended:

   ```bash
   install -d -m 0755 /var/lib/docker-plugins/tailscale
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

## Option B: an OAuth client per stack

1. In the admin console, create an OAuth client for the stack that can apply only the stack's tags.

2. Keep its secret in your deployment tool's secret store, for example as `TSLINK_OAUTH_SECRET`.

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

## Rotate the credential

- **Option A:** replace `oauth-client.secret` on every host. tslink reads the file whenever a node registers, so you
  don't recreate any network.
- **Option B:** Docker can't change a network's options, so remove the stack and deploy it again with the new secret.
  That makes the stack unavailable for a few seconds. To avoid it, deploy the stack under a new name with the new
  secret, and remove the old stack once the new one serves.

Running nodes stay up when you revoke a secret; [rotation and
revocation](../explanation/security-model.md#rotation-and-revocation) explains why, and what still stops working.

## Check that it works

The tasks run:

```bash
docker stack ps billing
```

In the admin console, the stack's nodes carry the network's tags: `tag:billing` with option A. And on a host of the
stack, each of the stack's status files shows `running`, as [Monitor tslink](monitor.md) describes.

## Next steps

- [Expose a service](expose-a-service.md): put the stack's replicas behind a Tailscale Service.
- [Update without downtime](zero-downtime-updates.md): roll out new versions without dropping requests.
- [Credentials](../reference/credentials.md): every credential type, and how tslink picks one.
