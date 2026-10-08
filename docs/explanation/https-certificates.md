# HTTPS certificates

This page tells how tslink gets HTTPS certificates for served names, and how to stay within the limits of Let's
Encrypt.

## Why each served name gets a certificate

Tailscale gets a Let's Encrypt certificate for each name that it serves HTTP on. Since Tailscale 1.100, this is also
true for plain HTTP. This is a Tailscale bug:
[tailscale/tailscale#21693](https://github.com/tailscale/tailscale/issues/21693).

## Rate limits

| Limit of Let's Encrypt | Value |
| --- | --- |
| New certificates for each tailnet | 50 in a week |
| New certificates for each name | 5 in a week |

## Names of Services

The backends of a Tailscale Service share the certificate of the Service. tslink gets this certificate once. Thus
only the first deployment of a Service counts against the limits.

If the plugin setting `shared.source` is a volume that all hosts share, tslink gets the certificate once for the
cluster. Otherwise, each host gets it once.

## Names of containers

A container with a name of its own (`tslink.direct`) gets a new certificate for each new container. A replicated
service with names of its own uses the limits quickly. Thus `tslink.direct` is `false` by default for a container with
`tslink.service`.

## Renewal

Each backend renews the shared certificate itself, 30 days before the certificate expires. While the renewal of one
backend is pending, Let's Encrypt refuses the renewals of the other backends with `alreadyReplaced`. This continues
until the renewal is complete, or until it expires after at most 7 days.

A refused renewal does not count against a limit, and the current certificate stays in use. With certificates that
are valid for 90 days, this causes no problem.

tslink shows these conditions:

| Condition | Plugin log | Status file |
| --- | --- | --- |
| Let's Encrypt refuses a renewal | A warning, once an hour for each name | `renewal-blocked/<domain>` |
| A certificate expires in less than 14 days | A warning, once a day | `cert-expiry/<domain>` |

## Recommendations

1. Serve replicated services through Tailscale Services, not on the names of the containers.
2. Set `shared.source` to a volume that all hosts share.
3. Add new Services to the tailnet at a rate well below 50 in a week. Include all other users of Tailscale HTTPS
   certificates in the tailnet in this count.
