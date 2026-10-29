---
title: HTTPS certificates
description: How containers and Services get their Let's Encrypt certificates, and how to stay within Let's Encrypt's limits.
category: explanation
tags: [https, certificates, lets-encrypt, services]
---

# HTTPS certificates

When a container serves HTTPS, Tailscale gets a Let's Encrypt certificate for its name. That's convenient, but Let's
Encrypt limits how many certificates you can get, and a busy Swarm reaches those limits quickly unless certificates
are shared. This page explains how tslink shares them, and what's left for you to plan for.

## Why each served name gets a certificate

Tailscale gets a Let's Encrypt certificate for every name it serves HTTP on. Since Tailscale 1.100, that includes
plain HTTP, which is a Tailscale bug: [tailscale/tailscale#21693](https://github.com/tailscale/tailscale/issues/21693).
The fix, [tailscale/tailscale#21736](https://github.com/tailscale/tailscale/pull/21736), isn't in Tailscale 1.104.4
or earlier. `tslink version` shows which Tailscale your plugin bundles.

## Rate limits

| Let's Encrypt's limit | Value |
| --- | --- |
| New certificates per tailnet | 50 a week |
| New certificates per name | 5 a week |

## Names of Services

A Tailscale Service's backends share the Service's certificate, and tslink gets it once, so only a Service's first
deployment counts against the limits.

With a [`shared.source`](../reference/plugin-settings.md#sharedsource) every host shares, tslink gets the certificate
once per stack. Each stack keeps its certificates in a directory of its own, `certs/by-stack/<stack>/`, so a stack
that names another stack's Service can't hold up that Service's certificate; containers outside a stack share
`certs/`. Without a shared directory, each host gets the certificate once.

So a Service that moves to another stack gets a new certificate, as in a blue/green deployment with two stacks, or
when you rename a stack. Each such move counts against the limit of 5 per name a week.

After an upgrade from a version without these directories, a stack takes over a valid certificate for its Service
from `certs/` instead of getting a new one.

tslink coordinates the backends only for a Service that serves HTTPS. If a Service serves only plain HTTP, backends
that start at the same time can each get a certificate.

## Names of containers

A container served on a name of its own ([`tslink.direct`](../reference/container-labels.md#tslinkdirect)) gets a new
certificate with every new container, so a replicated service with names of its own uses up the limits fast. That's
why `tslink.direct` is off by default for a container with a Service.

## Renewal

Each backend renews the shared certificate itself, 30 days before it expires. While one backend's renewal is pending,
Let's Encrypt refuses the others' with `alreadyReplaced`, until that renewal completes or expires, after 7 days at
most.

A refused renewal doesn't count against any limit, and the current certificate stays in use, which with 90-day
certificates leaves plenty of time. tslink still tells you about it:

| Condition | Plugin log | Status file |
| --- | --- | --- |
| Let's Encrypt refuses a renewal | A warning, once an hour per name | `renewal-blocked/<domain>` |
| A certificate expires in less than 14 days | A warning, once a day | `cert-expiry/<domain>` |

## Failed validations

Let's Encrypt validates every new certificate with a DNS-01 challenge: tailscaled asks Tailscale's control server to
publish a TXT record for the name, and Let's Encrypt then asks the `ts.net` name servers for it. In the cluster test,
2 of 3 validations failed because some `ts.net` name servers didn't have the record yet, and the order ended with
`status: invalid`. tslink can't prevent that.

A failed validation yields no certificate, so tslink asks again 5 seconds after each failure, until it gets one.
Failures don't count against the [rate limits](#rate-limits) above, but against another one:

| Let's Encrypt's limit | Value |
| --- | --- |
| Failed validations per name and ACME account | 5 an hour, then 1 more every 12 minutes |

Every tailscaled that uses one certificate directory uses one ACME account. After the fifth failure, Let's Encrypt
refuses new orders for the name with `rateLimited` until the limit refills.

Until the certificate exists, tslink doesn't advertise a Service's HTTPS backends. Backends on a host without the
certificate get no traffic while validations fail, and if no host has it, as for a new Service, the Service isn't
reachable at all.

## Recommendations

1. Serve replicated services through Tailscale Services, not on the containers' own names.
2. Set `shared.source` to a volume every host shares.
3. Add new Services well below 50 a week, counting everyone else in the tailnet who uses Tailscale HTTPS
   certificates.
4. Keep the certificate directory when you reinstall the plugin or replace a host: every new certificate needs a
   validation, and a validation can fail.
