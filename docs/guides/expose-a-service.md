# Expose a service

## Goal

Make an HTTPS service in a container available in the tailnet: first on the hostname of the container, then as a
Tailscale Service with more than one backend.

## Prerequisites

- A tslink network. See [Getting started](../getting-started.md) or [Deploy on Swarm](deploy-on-swarm.md).
- HTTPS is enabled in the tailnet (admin console, DNS settings).
- For a Tailscale Service: the credential of the network applies tags.

## Steps

1. Serve the container on its own hostname. Give it a hostname and a serve rule:

   ```bash
   docker run -d --network my-tailnet \
     --label tslink.hostname=web \
     --label tslink.serve.443=https:8080 \
     my-web-app
   ```

   Port 443 in the tailnet now goes to port 8080 in the container. For the syntax, see
   [`tslink.serve.<port>`](../reference/container-labels.md#tslinkserveport).

   > [!WARNING]
   > Tailscale gets a Let's Encrypt certificate for each name that it serves HTTP on, and Let's Encrypt limits the
   > number of new certificates. Do not give replicated containers names of their own. See
   > [HTTPS certificates](../explanation/https-certificates.md).

2. To serve more than one container under one name, create a Tailscale Service. In the admin console, open
   [Services](https://login.tailscale.com/admin/services) and create the Service, for example `svc:my-app`.

3. Make the containers backends of the Service, with a readiness healthcheck. With the healthcheck, Swarm replaces a
   task only when the new task is a backend of the Service:

   ```yaml
   services:
     backend:
       image: nginx:alpine
       networks:
         - tailnet
       labels:
         tslink.service: svc:my-app
         tslink.serve.443: https:80
         tslink.health: "9002"
       healthcheck:
         test: ["CMD", "wget", "-q", "-O-", "http://127.0.0.1:9002/ready"]
         interval: 10s
         retries: 3
       deploy:
         update_config:
           order: start-first
   ```

   Put the labels in `labels`, not in `deploy.labels`. tslink reads only the labels of the container.

   The readiness endpoint does not answer for some seconds during a plugin restart. Keep `interval` × `retries`
   above this period. In this example, it is 30 seconds. For the states, see
   [Readiness endpoint](../reference/readiness-endpoint.md).

4. If the application needs the identity of the caller, add a serve option. For example, `?proxy-protocol=2` sends
   the address of the caller to a TCP service. See [Serve options](../reference/container-labels.md#serve-options).

## Verify

1. From a device in the tailnet, connect to the hostname of step 1:

   ```bash
   curl https://web.<tailnet>.ts.net
   ```

2. In a backend container, read the readiness endpoint:

   ```bash
   docker exec <container> wget -q -O- http://127.0.0.1:9002/ready
   ```

   The output is `{"state":"ready","service":"svc:my-app"}`.

3. From a device in the tailnet, connect to the Service. The admin console shows the address of the Service.

## Next steps

- [Update without downtime](zero-downtime-updates.md)
- [Container labels](../reference/container-labels.md)
- [HTTPS certificates](../explanation/https-certificates.md)
