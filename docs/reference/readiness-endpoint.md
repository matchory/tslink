---
title: Readiness endpoint
description: The endpoint for Docker healthchecks.
category: reference
tags: [readiness]
---

# Readiness endpoint

The readiness endpoint tells a Docker healthcheck when the node of a container is ready. The label
`tslink.health=<port>` starts it on `127.0.0.1:<port>` in the container. Only the processes of the container can
reach it.

## Request

| Method and path | Response |
| --- | --- |
| `GET /ready` | A status code and a JSON body |
| `HEAD /ready` | The status code only |
| Other methods | `405 Method Not Allowed` |

## Response

The status code is `200` if the state is `ready`, and `503` for all other states. The body is a JSON object:

```json
{"state": "certificate-pending", "services": {"svc:a": "ready", "svc:b": "certificate-pending"}}
```

| Field | Value |
| --- | --- |
| `state` | One of the [states](#states): the least ready of the container's Services. It [latches](#latching) at `ready`. |
| `services` | The current state of each Service, also after `state` latched: a Service that lost its approval shows here while `state` stays `ready`. Once latched, the answer does not wait for tailscaled: it shows the states from the previous check, a few seconds old at most when the container asks regularly. The field is absent without a Service. |
| `service` | The name of the Service when the container has exactly one. The field is absent otherwise. |

A container with several Services is ready only when all are. One Service whose certificate is stuck or that waits
for approval keeps the container unready, so a healthcheck on `/ready` fails the whole task, its working Services
included. This is deliberate: an update must not replace working tasks with tasks that serve less. Deploy Services
that should fail independently as separate containers.

tslink keeps a result for 2 seconds. A request in this period gets the same result.

## States

| State | Meaning |
| --- | --- |
| `starting` | tailscaled is not running yet. |
| `failed` | tailscaled cannot start, and tslink does not try again. |
| `logged-out` | The node must log in. |
| `serving-pending` | The `tslink.serve.<port>` rules are not applied yet. |
| `certificate-pending` | The Service waits for its HTTPS certificate. |
| `ports-uncovered` | The Service defines ports that the container does not serve. The control server sends no traffic to such a backend. The status file lists the ports. |
| `awaiting-approval` | The control server has not approved the container as a backend of the Service. |
| `awaiting-control` | tslink advertised the Service, and the control server has not fetched the Service list of the node since. Until it does, it may send no traffic to the container. |
| `drained` | The backend is drained, or the container is leaving the network. |
| `ready` | All conditions for ready are true. |

## Conditions for ready

The state is `ready` when all these conditions are true:

1. The node is running.
2. tailscaled has applied the `tslink.serve.<port>` rules.
3. For each Service: the container serves every port that the Service defines, if tslink knows these ports.
4. For each Service: the control server has approved the container as a backend of the Service.
5. For each Service: the control server has fetched the Service list of the node since tslink last advertised the
   Service.

tslink knows the ports of a Service from the capabilities that the control server gives the node. Otherwise, if the
credential is an OAuth client with the scope `services:read`, tslink reads them from the Tailscale API. If neither
source has them, or the Service does not validate its ports (`do-not-validate`), tslink does not check them. See
[Credentials](credentials.md#optional-api-scopes).

## Latching

When the state of a container is `ready` once, it stays `ready` until one of these events:

- tslink drains the backend of the container.
- The container leaves the network.

tslink records the latch in `status/<endpoint>.ready` in the data directory, so the latch stays after a plugin
restart. Thus the endpoint controls when a container becomes healthy, but it is not a liveness check. A container
does not become unhealthy when it loses its connection to the control server, or during a plugin restart.

During a plugin restart, the endpoint does not answer for some seconds. For a healthcheck that allows for this, see
[Expose a service](../guides/expose-a-service.md).
