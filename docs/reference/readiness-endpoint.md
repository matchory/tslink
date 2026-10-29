---
title: Readiness endpoint
description: The endpoint a Docker healthcheck asks whether a container's node and Services are ready to take traffic.
category: reference
tags: [readiness, healthcheck, services, swarm]
---

# Readiness endpoint

The readiness endpoint tells a Docker healthcheck when a container's node, and every Service it backs, is ready to
take traffic. The label [`tslink.health=<port>`](container-labels.md#tslinkhealth) starts it on `127.0.0.1:<port>`
inside the container, where only the container's own processes can reach it.

## Request

| Method and path | Response |
| --- | --- |
| `GET /ready` | A status code and a JSON body |
| `HEAD /ready` | The status code only |
| Any other method | `405 Method Not Allowed` |

## Response

The status code is `200` when the state is `ready`, and `503` for every other state. The body is a JSON object:

```json
{"state": "certificate-pending", "services": {"svc:a": "ready", "svc:b": "certificate-pending"}}
```

| Field | Value |
| --- | --- |
| `state` | One of the [states](#states): the least ready of the container's Services. It [latches](#latching) at `ready`. |
| `services` | The current state of each Service, even after `state` latched: a Service that lost its approval shows up here while `state` stays `ready`. Once latched, the answer doesn't wait for tailscaled; it shows the states from the previous check, a few seconds old at most when the container asks regularly. Absent without a Service. |
| `service` | The Service's name, when the container has exactly one. Absent otherwise. |

A container with several Services is ready only once all of them are. One Service whose certificate is stuck, or
that waits for approval, keeps the container unready, so a healthcheck on `/ready` fails the whole task, its working
Services included. That's deliberate: an update must not replace working tasks with ones that serve less. Deploy
Services that should fail independently as separate containers.

tslink keeps a result for 2 seconds; requests within that window get the same answer.

## States

| State | Meaning |
| --- | --- |
| `starting` | tailscaled isn't running yet. |
| `failed` | tailscaled can't start, and tslink won't retry. |
| `logged-out` | The node needs to log in. |
| `serving-pending` | The `tslink.serve.<port>` rules aren't applied yet. |
| `certificate-pending` | The Service is waiting for its HTTPS certificate. |
| `ports-uncovered` | The Service defines ports the container doesn't serve, so the control server sends it no traffic. The status file lists the ports. |
| `awaiting-approval` | The control server hasn't approved the container as a backend of the Service. |
| `awaiting-control` | tslink advertised the Service, and the control server hasn't fetched the node's Service list since. Until it does, it can send the container no traffic. |
| `drained` | The backend is drained, or the container is leaving the network. |
| `ready` | Every condition for ready holds. |

## Conditions for ready

The state is `ready` when all of these hold:

1. The node is running.
2. tailscaled has applied the `tslink.serve.<port>` rules.
3. For each Service: the container serves every port the Service defines, if tslink knows those ports.
4. For each Service: the control server has approved the container as its backend.
5. For each Service: the control server has fetched the node's Service list since tslink last advertised the
   Service.

tslink learns a Service's ports from the capabilities the control server gives the node, or else, when the credential
is an OAuth client with the scope `services:read`, from Tailscale's API. If neither has them, or the Service doesn't
validate its ports (`do-not-validate`), tslink doesn't check them. See
[optional API scopes](credentials.md#optional-api-scopes).

## Latching

Once a container's state has been `ready`, it stays `ready` until tslink drains the container's backend or the
container leaves the network.

tslink records the latch in `status/<endpoint>.ready` in the data directory, so it survives a plugin restart. So the
endpoint decides when a container first becomes healthy, but it isn't a liveness check: a container doesn't turn
unhealthy when it loses its connection to the control server, or while the plugin restarts.

While the plugin restarts, the endpoint doesn't answer for a few seconds. [Expose a
service](../guides/expose-a-service.md#make-the-containers-its-backends) shows a healthcheck that allows for that.
