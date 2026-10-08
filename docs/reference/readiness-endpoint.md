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

| Status code | Meaning |
| --- | --- |
| `200` | The state is `ready`. |
| `503` | The state is not `ready`. |

The body is a JSON object:

```json
{"state": "awaiting-approval", "service": "svc:api"}
```

| Field | Value |
| --- | --- |
| `state` | One of the [states](#states) |
| `service` | The value of `tslink.service`. The field is absent without a Service. |

tslink keeps a result for 2 seconds. A request in this period gets the same result.

## States

| State | Meaning |
| --- | --- |
| `starting` | tailscaled is not running yet. |
| `failed` | tailscaled cannot start, and tslink does not try again. |
| `logged-out` | The node must log in. |
| `serving-pending` | The `tslink.serve.<port>` rules are not applied yet. |
| `certificate-pending` | The Service waits for its HTTPS certificate. |
| `awaiting-approval` | The control server has not approved the container as a backend of the Service. |
| `drained` | The backend is drained, or the container is leaving the network. |
| `ready` | All conditions for ready are true. |

## Conditions for ready

The state is `ready` when all these conditions are true:

1. The node is running.
2. tailscaled has applied the `tslink.serve.<port>` rules.
3. With `tslink.service`: the control server has approved the container as a backend of the Service.

## Latching

When the state of a container is `ready` once, it stays `ready` until one of these events:

- tslink drains the backend of the container.
- The container leaves the network.

tslink records the latch in `status/<endpoint>.ready` in the data directory, so the latch stays after a plugin
restart. Thus the endpoint controls when a container becomes healthy, but it is not a liveness check. A container
does not become unhealthy when it loses its connection to the control server, or during a plugin restart.

> [!NOTE]
> During a plugin restart, the endpoint does not answer for some seconds. Set `interval` × `retries` of the
> healthcheck to more than this period. For an example, see
> [Expose a service](../guides/expose-a-service.md).
