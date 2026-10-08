# Getting started

In this tutorial, you install tslink on one host. Then you connect an nginx container to your tailnet, and open it
over HTTPS from another device in the tailnet.

## What you need

- A Linux host with Docker, or a Mac with OrbStack or Docker Desktop.
- A Tailscale tailnet, with HTTPS enabled in the DNS settings of the admin console.
- An auth key that is ephemeral, reusable and pre-approved. Create it in the admin console under
  [Settings → Keys](https://login.tailscale.com/admin/settings/keys).
- A second device in the same tailnet, for example your laptop.

## 1. Install the plugin

1. Create the data directory:

   ```bash
   sudo install -d -m 0755 /var/lib/docker-plugins/tailscale
   ```

2. Install the plugin. On an ARM host or an Apple silicon Mac, replace `amd64` with `arm64`:

   ```bash
   docker plugin install --alias tslink --grant-all-permissions \
     ghcr.io/matchory/tslink:latest-amd64
   ```

3. Make sure that the plugin is enabled:

   ```bash
   docker plugin ls
   ```

   The output shows `tslink:latest` with `ENABLED` set to `true`.

## 2. Create a network

Create a tslink network with your auth key:

```bash
docker network create --driver tslink:latest \
  --opt tslink.authkey=tskey-auth-… \
  --opt tslink.ephemeral=true \
  my-tailnet
```

The option `tslink.ephemeral=true` tells tslink that the key creates ephemeral nodes. tslink then removes the node
when the container stops.

## 3. Run a container

Start nginx on the network. The labels give the node the name `web`, and serve port 80 of nginx as HTTPS on port 443:

```bash
docker run -d --name web --network my-tailnet \
  --label tslink.hostname=web \
  --label tslink.serve.443=https:80 \
  nginx:alpine
```

Some seconds later, the node `web` is in the Machines list of the admin console.

## 4. Open the container from the tailnet

On your second device, open the container. Replace `<tailnet>` with the name of your tailnet, for example
`tail1234`:

```bash
curl https://web.<tailnet>.ts.net
```

The output is the welcome page of nginx. The first request can take some seconds, because Tailscale gets a
certificate for the name.

## 5. Clean up

Stop the container, and remove the network:

```bash
docker rm -f web
docker network rm my-tailnet
```

tslink logs the node out. The node disappears from the admin console.

## What happened

tslink started a tailscaled in the network namespace of the container. Thus the container was a node in the tailnet
with its own name and address, separate from the host. Tailscale Serve received HTTPS on port 443 and forwarded it to
nginx. For details, see [Architecture](explanation/architecture.md).

## Next steps

- [Expose a service](guides/expose-a-service.md): use a Tailscale Service with more than one container.
- [Deploy on Swarm](guides/deploy-on-swarm.md): run stacks with their own tags.
- [Provision a host](guides/provision-a-node.md): prepare a production host.
- [Container labels](reference/container-labels.md): all labels.
