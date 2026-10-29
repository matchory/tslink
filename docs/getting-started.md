---
title: Getting started
description: Install tslink on one host, put an nginx container on your tailnet as a node of its own, and open it over HTTPS.
category: tutorial
tags: [installation, quickstart, https]
---

# Getting started

You'll install the plugin, run nginx as a node of its own in your tailnet, and open it from your laptop at
`https://web.<tailnet>.ts.net`, with a valid certificate.

## What you need

- A Linux host with Docker, or a Mac with OrbStack or Docker Desktop.
- A tailnet with HTTPS enabled, in the DNS settings of the admin console.
- An auth key that's ephemeral, reusable and pre-approved, from
  [Settings → Keys](https://login.tailscale.com/admin/settings/keys) in the admin console. Ephemeral means the node
  goes away when its container does, so trying things out leaves nothing behind.
- A second device in the same tailnet to test from, such as your laptop.

## 1. Install the plugin

Install the plugin from the GitHub container registry. On an ARM host or an Apple silicon Mac, replace `amd64` with
`arm64`:

```bash
docker plugin install --alias tslink --grant-all-permissions \
  ghcr.io/matchory/tslink:latest-amd64
```

`--grant-all-permissions` accepts what the plugin asks for: the host's network, `CAP_NET_ADMIN` and
`CAP_SYS_ADMIN`, the TUN device and the Docker socket. It needs them to create network interfaces and firewall rules
on the host, to enter containers' network namespaces, and to read their labels. Check that Docker enabled it:

```bash
docker plugin ls
```

You should see `tslink:latest` with `ENABLED` set to `true`.

## 2. Create a network

A tslink network is where the credential lives: every container you attach to it joins your tailnet with it.

```bash
docker network create --driver tslink:latest \
  --opt tslink.authkey=tskey-auth-… \
  --opt tslink.ephemeral=true \
  my-tailnet
```

`tslink.ephemeral=true` tells tslink that the key creates ephemeral nodes, so it logs the node out as soon as the
container stops instead of leaving it offline in your tailnet.

## 3. Run a container

Start nginx on the network. The two labels name the node `web`, and serve nginx's port 80 as HTTPS on port 443:

```bash
docker run -d --name web --network my-tailnet \
  --label tslink.hostname=web \
  --label tslink.serve.443=https:80 \
  nginx:alpine
```

After a few seconds, `web` appears in the Machines list of the admin console: a node of its own, with its own tailnet
address, not the host's.

## 4. Open it from your tailnet

On your second device, replace `<tailnet>` with your tailnet's name, such as `tail1234`, and open the page:

```bash
curl https://web.<tailnet>.ts.net
```

You get nginx's welcome page. The first request can take a few seconds, while Tailscale gets a certificate for the
name from Let's Encrypt.

## 5. Clean up

Remove the container and the network:

```bash
docker rm -f web
docker network rm my-tailnet
```

tslink logs the node out, and it disappears from the admin console.

## What just happened

When the container started, tslink ran a tailscaled inside the container's network namespace, so the container
itself became a node in your tailnet, with its own name and address. Tailscale Serve, inside that same node,
answered HTTPS on port 443 and forwarded each request to nginx on port 80. The host's own Tailscale, if it has one,
wasn't involved. [Architecture](explanation/architecture.md) explains the details.

## Next steps

- [Expose a service](guides/expose-a-service.md): put several containers behind one Tailscale Service.
- [Deploy on Swarm](guides/deploy-on-swarm.md): run stacks, each with its own tags.
- [Provision a host](guides/provision-a-node.md): get a host ready for production.
- [Container labels](reference/container-labels.md): everything a container can configure.
