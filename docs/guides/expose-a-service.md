---
title: Expose a service
description: Serve a container over HTTPS on its own name, then put several containers behind one Tailscale Service.
category: guide
tags: [serve, services, https, healthcheck]
---

# Expose a service

By the end of this guide, a web app in a container answers HTTPS in your tailnet on a name of its own. Then you'll put
several replicas behind one [Tailscale Service](https://tailscale.com/docs/features/tailscale-services), so callers
use one stable name, and tslink takes a replica out of rotation before it stops.

## Before you start

- You have a tslink network. [Getting started](../getting-started.md) creates one on a single host, and
  [Deploy on Swarm](deploy-on-swarm.md) one for a stack.
- HTTPS is enabled for your tailnet, in the DNS settings of the admin console.
- For a Tailscale Service, the network's credential gives its nodes tags. Services are hosted by tagged nodes only.

## Serve a container on its own name

Give the container a hostname, and a serve rule from a tailnet port to a container port:

```bash
docker run -d --network my-tailnet \
  --label tslink.hostname=web \
  --label tslink.serve.443=https:8080 \
  my-web-app
```

HTTPS on port 443 of `web.<tailnet>.ts.net` now reaches port 8080 in the container. Tailscale terminates TLS with a
certificate for the name, so your app can speak plain HTTP. The rule also takes HTTP, TCP and paths; the
[`tslink.serve.<port>`](../reference/container-labels.md#tslinkserveport) reference has the syntax.

> [!WARNING]
> Tailscale gets a Let's Encrypt certificate for every name it serves HTTP on, and Let's Encrypt limits how many new
> certificates you can get per week. Don't give each replica a name of its own: put replicas behind a Service, which
> shares one certificate. [HTTPS certificates](../explanation/https-certificates.md) explains the limits.

## Put several containers behind a Service

### Create the Service

In the admin console, open [Services](https://login.tailscale.com/admin/services) and create one, for example
`svc:my-app`, with the ports your containers serve. Approve the tag of your network's nodes for it.

Define only ports that every backend serves. Tailscale's control server routes traffic to a backend only if it
serves every port of the Service, and tslink reports a backend that doesn't as `ports-uncovered`.

### Make the containers its backends

Label the containers with the Service and what they serve, and give them a readiness healthcheck:

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

`tslink.health` starts tslink's [readiness endpoint](../reference/readiness-endpoint.md) on port 9002 of the
container's loopback interface. It answers 200 only once the backend is advertised and approved, so with
`order: start-first`, Swarm replaces an old task only after the new one actually takes traffic.

Put the labels under `labels`, not `deploy.labels`: tslink reads the container's labels, and `deploy.labels` only
label the Swarm service.

While the plugin restarts, the readiness endpoint doesn't answer for a few seconds. Keep `interval` × `retries`
longer than that, so a plugin restart doesn't mark healthy containers unhealthy. Here, it's 30 seconds.

### Pass the caller's identity on

If your app needs to know who's calling, add a [serve option](../reference/container-labels.md#serve-options). For
a TCP service, `?proxy-protocol=2` sends the caller's tailnet address in a PROXY protocol header. For HTTP,
`?accept-app-caps=…` passes the caller's grants for your app capabilities.

### Back several Services from one container

A container can back more than one Service. Instead of `tslink.service` and `tslink.serve.<port>`, use one label per
Service and port:

```yaml
services:
  backend:
    image: my-ai-app
    networks:
      - tailnet
    labels:
      tslink.service.ai-gateway.serve.443: https:8080
      tslink.service.ai-admin.serve.443: https:8081
      tslink.health: "9002"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O-", "http://127.0.0.1:9002/ready"]
      interval: 10s
      retries: 3
    deploy:
      update_config:
        order: start-first
```

Each Service has to exist in the admin console and approve the node's tag, as before. tslink advertises each one as
soon as it's configured, and the container is ready once all of them are. See
[`tslink.service.<name>.serve.<port>`](../reference/container-labels.md#tslinkservicenameserveport).

## Check that it works

From a device in your tailnet, open the container's own name:

```bash
curl https://web.<tailnet>.ts.net
```

Ask a backend whether it's ready:

```bash
docker exec <container> wget -q -O- http://127.0.0.1:9002/ready
```

A ready backend answers:

```json
{"state":"ready","service":"svc:my-app","services":{"svc:my-app":"ready"}}
```

Then connect to the Service from your device. Its name and address are on its page in the admin console.

## Next steps

- [Update without downtime](zero-downtime-updates.md): replace backends, restart Docker and upgrade tslink without
  dropping requests.
- [Container labels](../reference/container-labels.md): every label and serve option.
- [HTTPS certificates](../explanation/https-certificates.md): how certificates are issued and shared.
