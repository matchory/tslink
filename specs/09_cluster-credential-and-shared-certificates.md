# Spec: Cluster Credential and Shared HTTPS Certificates

> **Status:** Implemented
> **Last Updated:** 2026-10-07
> **See Also:** [docs/credentials.md](../docs/credentials.md) (the per-stack model this extends),
> [02_tailscale-services.md](02_tailscale-services.md)

## Overview

Today each stack carries its own OAuth client secret in its network options, which puts a non-expiring secret into
stack files, CI and `docker network inspect`, and means minting a client per stack. This spec adds one OAuth client
for the whole cluster, read by the plugin from a file, so stacks name only their tags.

Ephemeral nodes also lose their HTTPS certificates with their state, so every new task of a Tailscale Service with an
HTTPS endpoint issues a new certificate for the same name. Let's Encrypt allows 5 of those per week, so a rolling
update of a six-replica service is enough to lock the name out for about 34 hours. This spec moves certificates into a
directory that outlives the node, optionally shared between all hosts, and serializes the first issuance.

## Goals

- Deploy a new stack without a new OAuth client, without a secret in the stack file, and without touching node
  provisioning
- Rotate the cluster credential by replacing one file, without recreating networks
- Issue the certificate for a Tailscale Service name once, not once per task
- Advertise a replica as a Service backend only when it can serve HTTPS
- Keep the per-stack model working unchanged

## Non-Goals

- Persistent (non-ephemeral) nodes with the cluster credential: it registers ephemeral nodes only
- Certificate sharing for per-task names (`tslink.direct`): every task has a unique name, so there is nothing to
  share. Service replicas therefore no longer serve on their own names unless asked (see
  [Tailscale behavior](#tailscale-behavior-worked-around))
- Coordinating tailscaled's certificate renewals (see [Renewal](#renewal))
- Making HTTPS the default: it stays opt-in per endpoint (`tslink.serve.<port>: https:<target>`); serving plain HTTP
  inside the tailnet is a valid choice

## Configuration

Plugin settings, set once per node by provisioning, and the same for every stack:

| Setting | Default | Description |
| --- | --- | --- |
| file `/var/lib/docker-plugins/tailscale/oauth-client.secret` | absent | The cluster's OAuth client secret (`tskey-client-…`, nothing appended), in the plugin's existing data mount |
| mount `shared` source | `/var/lib/docker-plugins/tailscale` | Holds `certs/`. Defaults to the data directory, so certificates are kept per host; point it at a shared volume (GlusterFS, NFS) to share them across hosts: `docker plugin set tslink shared.source=/mnt/shared/tslink` |
| `TS_DEBUG_ACME_DIRECTORY_URL` | empty | Passed through to tailscaled. Test hook: point it at Let's Encrypt staging |
| `SSL_CERT_FILE` | empty | Root certificates for tslink and tailscaled. Test hook: the system bundle plus the test CA's roots, which tailscaled needs to accept a cached certificate |

Network options, per stack, with the cluster credential:

```yaml
networks:
  tailnet:
    driver: tslink:latest
    driver_opts:
      tslink.tags: tag:billing
```

### Tailnet policy

The OAuth client gets the `auth_keys` scope and one tag, say `tag:tslink`, which owns every tag a stack may use:

```json
"tagOwners": {
  "tag:tslink":  ["autogroup:admin"],
  "tag:billing": ["tag:tslink"],
  "tag:shop":    ["tag:tslink"]
}
```

Adding a stack's tag is a policy change, not a node change. Tailscale Services with HTTPS need their `ports` to include
`tcp:443` and a grant on it.

## Behavior

### Credential resolution

A node registers with the first credential that exists, in this order:

1. The network's `tslink.authkey` (the per-stack model, unchanged)
2. The cluster credential file
3. The plugin's `TS_AUTHKEY` setting

tslink reads the cluster credential file each time a node registers, not at network creation, and appends
`?ephemeral=true&preauthorized=true`. Replacing the file therefore takes effect for the next registration. Running
nodes stay online, as they do when a per-stack secret is revoked.

A network using the cluster credential must set `tslink.tags`. Without tags, network creation fails on the node, and
the task shows the error; otherwise registration would fail later, in the background. Networks with their own secret
keep today's behavior, where tags may also come from container labels.

A node whose state survives, such as after a plugin restart, is not re-registered when the cluster credential changes:
that is a rotation, and a logged-in node needs no key. An explicit key that changes still wipes the state, as before.

### Tag scope

Per-stack clients confine each stack to its own tags: Tailscale rejects tags the stack's client does not own. The
cluster client owns every tag, so on its own it would let any stack claim any other stack's tags.

With the cluster credential, a network's tags must therefore be `tag:<stack>` or start with `tag:<stack>-`, where
`<stack>` is the network's `com.docker.stack.namespace`. A network outside a stack cannot use the cluster credential.
Docker does not pass a network's labels to the driver when it creates the network, so the check runs when a task
starts; a task that fails it never starts tailscaled, as with a task from the wrong stack.
This puts the boundary back where [credentials.md](../docs/credentials.md) has it: whoever can deploy a stack under a
name acts as that stack, and `docker stack deploy` sets the namespace label itself. Tags a stack needs beyond its
prefix require a per-stack secret.

Rejected:

- **No check:** anyone who can deploy a stack could take any tag the cluster client owns
- **An allow-list on each node** (stack → tags): couples deployments to provisioning

### Certificate directory

Every tailscaled keeps its certificates in `<state dir>/certs`; tslink links that to `certs/` in the plugin's `shared`
mount before tailscaled starts. A non-ephemeral node's existing `certs` directory is left alone. Certificates are files
named after their domain, so all services and all hosts share one directory.

tailscaled creates an ACME account key on first use. Two tailscaleds starting at once would each create one, and only
the last one written would stay, leaving certificates issued under an account whose key is gone. Renewals then could
not claim to replace the previous certificate, so they would lose their rate-limit exemption. tslink therefore creates
`acme-account.key.pem` itself, with exclusive create, before the first tailscaled uses the directory.

### First issuance

tslink applies a replica's serve configuration after tailscaled is up. For a replica with an `https` endpoint for a
Service, whose certificate is for `<service name>.<MagicDNS suffix>`:

1. If the certificate directory holds a valid, unexpired certificate for that name, tslink applies the serve
   configuration. tailscaled finds the certificate and issues nothing.
2. Otherwise tailscaled's start completes without the Service, and the replica configures it in the background. It
   tries to take the lease `<domain>.lease` in the certificate directory, created exclusively, naming the endpoint,
   and touched every 15 seconds while held.
   - The holder applies its serve configuration, then asks tailscaled for the certificate (`tailscale cert`) until it
     exists, and releases the lease. Serving the Service alone does not reliably start issuance: tailscaled fetches
     certificates for served names right away only if the name is already among the node's certificate domains,
     but control adds a Service's domain seconds after the Service is configured, and tailscaled's next attempt is
     an hour later.
   - Every other replica waits, checking every 5 seconds for a valid certificate, then applies its configuration.
3. A lease not touched for 2 minutes (tailscaled's issuance timeout) is stale; a waiting replica removes it and takes
   it.

A replica is advertised as a Service backend only once its serve configuration is applied, so traffic never reaches a
replica still waiting for its certificate. With `start-first` updates, the old replicas keep serving meanwhile. Only
the first deployment of a Service waits.

The lease lives only as long as an issuance, so no replica holds a lasting role.

The lease is not the only protection. A replica whose serve configuration survived in its node state, as after a plugin
restart, starts fetching before tslink gets to the lease. All tailscaleds share one ACME account, and Let's Encrypt
hands concurrent orders of one account for the same name the same order, so such a replica joins the holder's order
rather than issuing a second certificate (observed in validation).

### Tailscale behavior worked around

- **tailscaled's DNS.** tailscaled runs in the container's network namespace but would read the plugin's
  `resolv.conf`, which is the host's. Where that names a resolver on the host's loopback, such as systemd-resolved's
  `127.0.0.53`, every lookup tailscaled makes itself fails, so it could never reach an ACME server. Tailscale's own
  servers are unaffected, since it resolves them with built-in fallbacks. tslink starts tailscaled in a mount
  namespace of its own with a `resolv.conf` naming Docker's embedded resolver, `127.0.0.11`, which every container
  on a tslink network has.
- **Certificates for plain HTTP.** Since 1.100, tailscaled fetches a certificate for every name in its serve
  configuration, including names served only over plain HTTP. Its refresh loop does not check whether a web entry is
  HTTPS. With per-task names, every new task would draw on the tailnet's 50 new certificates a week. tslink
  therefore no longer serves Service replicas on their own names by default (`tslink.direct` defaults to `false`
  with `tslink.service`), and warns when a task serves HTTP on its own name. HTTP-only Services still get one
  certificate per Service name, issued once, until Tailscale fixes the loop
  ([tailscale/tailscale#21693](https://github.com/tailscale/tailscale/issues/21693)).

### Renewal

Every tailscaled runs as it does today and renews on its own. tslink has no hook into that decision. Each replica picks
a random time in Let's Encrypt's renewal window, renews once that time passes, and only forgets that time after its own
renewal. N replicas therefore renew N times per certificate lifetime, roughly every 60 days.

Each renewal replaces whatever certificate is on disk at that moment. Under a shared ACME account, Let's Encrypt
exempts such renewals from all rate limits. The cost is N certificates per cycle in the public Certificate
Transparency logs. Validation must confirm this (see [Validation](#validation)).

tailscaled writes the new key before the new certificate. A replica on another host that reads between the two sees a
mismatched pair, treats it as expired, and issues a new certificate that is not a renewal. The window is milliseconds;
this spec accepts it and counts on validation to show how often it happens.

While one replica's renewal order is pending, Let's Encrypt refuses the other replicas' orders for the same certificate
with `alreadyReplaced`, until that order completes or expires: up to 7 days. tailscaled retries on every request that
needs the certificate and keeps serving the current one. Refused orders count against no rate limit, and Let's Encrypt
renews 90-day certificates about 30 days before expiry, so the block is benign: an order abandoned by a stopped replica
expires with at least 23 days to spare. tslink does not intervene, but makes it visible:

- tailscaled's output is scanned for a refused renewal, a line with `cert("<domain>"):` and
  `urn:ietf:params:acme:error:alreadyReplaced` (`async renewal failed: getCertPem: 409 …`). The plugin log warns once
  an hour per domain, and the endpoint's status file lists it under `warnings` until tailscaled logs `got cert` for
  the domain.
- Every replica with an HTTPS Service checks its certificate hourly. With less than 14 days left, the plugin log warns
  once a day per domain, and the status file lists it until the certificate is renewed.

`tslink diag` lists the warnings of every endpoint on the node.

## Error Handling

| Condition | User Sees | Recovery |
| --- | --- | --- |
| Cluster credential, `tslink.tags` missing | Network creation fails: `tslink.tags is required with the cluster credential` | Add `tslink.tags` |
| Cluster credential, tag outside the stack's prefix | Task's tailscaled does not start; the status names the tag and the allowed prefix | Rename the tag, or use a per-stack secret |
| Cluster credential, network not in a stack | Task's tailscaled does not start | Deploy as a stack, or set `tslink.authkey` |
| Credential file unreadable or malformed at registration | Registration retries with backoff; the plugin log and status show the error | Fix the file; the next retry picks it up |
| Issuance fails (e.g. rate limited) | Holder logs the ACME error; all replicas stay unadvertised and keep waiting | Wait for the limit to refill, or remove the HTTPS endpoint |
| Certificate directory unwritable | Endpoint start fails with the path | Fix the mount |
| Renewal refused with `alreadyReplaced` | Plugin log warns hourly per domain; `warnings` in the status file, `tslink diag` | None needed: renewals resume once the pending order completes or expires (at most 7 days) |
| Certificate has less than 14 days left | Plugin log warns daily per domain; `warnings` in the status file, `tslink diag` | Look in the replicas' `tailscaled.log` for why renewals fail |

## Security Considerations

- **One credential, every tag.** Root on any node can read the cluster secret and mint keys for every tag the cluster
  client owns. With per-stack secrets, root on a node can already read the network options of the stacks running there;
  the difference is breadth. Rotation is cheap: replace the file.
- **The secret never enters a stack file, CI, network options or an application container.** It is not shown by `docker
  plugin inspect` either, since it is a file rather than a setting.
- **Tag scope** is limited to the stack's prefix (see [Tag scope](#tag-scope)).
- **The certificate directory holds the private keys of every Service's certificate and the ACME account key.** Anything
  that mounts the shared volume can read them: restrict the volume to the plugin's nodes.

## Validation

On the test bed ([test/cluster](../test/cluster/README.md)), against Let's Encrypt staging through
`TS_DEBUG_ACME_DIRECTORY_URL`, with an NFS export from `mgr` mounted on all five nodes as the shared volume. Results
from 2026-10-07 follow each item.

1. **Cluster credential:** mint a client that owns the `tag:tslink-test-*` tags and deploy `callee` and `caller` with
   only `tslink.tags`, as stacks named `tslink-test-callee` and `tslink-test-caller` to satisfy the tag scope; run
   `regress.sh`. Rotate the file and check that the next registrations use the new secret. **Passed:** devices
   registered with the right tags, ephemeral, and logged out cleanly; every caller reached the Service. After the file
   was replaced and the old client revoked, running nodes stayed connected, new tasks registered, and a plugin restart
   kept every node's identity.
2. **Tag scope:** a stack declaring another stack's tag fails at network creation. **Passed**, at task start rather than
   network creation (Docker does not pass the network's labels to the driver): no device registered, and the status file
   names the tag. A stack network without tags fails at creation; a network outside a stack is refused at task start.
3. **First issuance:** a shared certificate directory on all nodes (NFS from `mgr` if GlusterFS is not available),
   Service with `https` on 443, three replicas. Expect exactly one issuance, and no replica advertised before its
   certificate. **Passed** after the fixes in [Tailscale behavior](#tailscale-behavior-worked-around): one certificate,
   issued 32 seconds after the holder took the lease; the two waiting replicas configured themselves a second later.
   With a grant on `tcp:443`, HTTPS through the Service answered from callers on all five nodes, with the shared
   certificate for the Service's name, verified against the test CA's root alone.
4. **Rolling updates:** ten `start-first` updates; expect no new issuance. **Passed** with three updates (nine new
   replicas): no new certificate.
5. **Renewal**, against [Pebble](https://github.com/letsencrypt/pebble) instead of staging, since staging certificates
   live 90 days: certificates valid for 10 minutes, three replicas, and each replica's tailscaled asked for the
   certificate every 5 seconds, as TLS handshakes would; nothing else triggers a renewal check within the hour.
   Pebble, version 2.10.1, needed changes to stand in for Let's Encrypt:
   - a `Location` header on finalize responses: Pebble always finalizes asynchronously, and tailscaled's ACME client
     polls the order named there (Boulder usually returns a valid order at once);
   - certificates marked as replaced when the replacing order is created, as Boulder does, rather than after
     finalization, and unmarked when that order fails;
   - an ARI window of 5 to 7 minutes after issuance for each certificate, like Let's Encrypt's narrow windows: Pebble's
     own is 48 hours wide, which covers the whole lifetime of a short certificate;
   - no reuse of authorizations, and DNS lookups against `ts.net`'s authoritative servers: a public resolver caches the
     challenge TXT set for 300 seconds and hides new values.

   **Passed**, over two runs and nine windows:
   - Every renewal order named the certificate on disk at the time, so each certificate replaced the previous one.
     Orders without `replaces` came only from first issuance and from an expired certificate (below). No torn read was
     seen, but over eight renewals that says little about a millisecond window.
   - Each replica renews once per window: it keeps its renewal time until its own renewal succeeds.
   - Overlapping renewals: while one replica's order was pending, the others were refused with `alreadyReplaced` and
     retried on every request, always as renewals. Without the Boulder change, Pebble accepted all three overlapping
     orders and issued three renewals.
   - Replicas restarted during a window found the certificate and configured the Service without issuing. A replica
     stopped while its renewal order was pending left the order open, and it blocked every other replica's renewal
     until the certificate expired; then each replica issued a certificate without `replaces`. Let's Encrypt's orders
     expire after 7 days, well before its certificates do, so there a stopped renewal only delays the next one.
   - After a plugin restart, replicas whose serve configuration survived ordered at once, and Pebble issued each its
     own certificate; Let's Encrypt gives them one order (see [First issuance](#first-issuance)).
   - Pebble does not check CAA, so it issued for `ts.net` names.
   - Two thirds of the DNS-01 validations failed: the TXT records Tailscale's control plane publishes reached some of
     dnsimple's servers late. tailscaled retries them as renewals, but Let's Encrypt counts failed validations
     against a limit per name and account.
   - Pebble has no rate limits, so this validates tailscaled's and tslink's behavior, not Let's Encrypt's exemption.
6. **Lease takeover:** kill the holder's tailscaled during issuance; another replica takes the stale lease. **Passed:**
   a plugin reinstall killed the holder, and another replica took the lease once it went stale.
7. **Locking on the shared volume:** exclusive create of the lease is atomic on GlusterFS (and on NFS, if used).
   **Passed on NFS** (20 rounds of five nodes racing to hard-link the same name: one winner each); GlusterFS untested.

## Future Enhancements

1. **Workload identity federation** — replaces the cluster secret; see [credentials.md](../docs/credentials.md)
2. **A single renewer** — if validation shows N renewals per cycle are a problem, run all but one replica with
   `TS_CERT_SHARE_MODE=ro`; this brings back a lasting role and its handover problem

---

## Implementation Notes

Key files:

- `pkg/core/network.go` — credential precedence (`NewNetwork`), reading the file (`Credential`), `CheckTagScope`
- `pkg/docker/driver.go` — `CreateNetwork` and endpoint recovery use `NewNetwork`
- `pkg/core/endpoint.go` — tag scope check at task start; passes the certificate directory to tailscaled
- `pkg/tailscale/daemon.go` — `DaemonConfig.AuthKey` is a function, so a re-login reads the current file
- `pkg/tailscale/certs.go` — certs link, ACME account key, lease, requesting the certificate, and configuring the
  Service once certified
- `pkg/tailscale/daemon.go` — tailscaled's own mount namespace and `resolv.conf`
- `pkg/tailscale/warnings.go` — blocked renewals and certificates close to expiry; `pkg/core/status.go` writes them
  to the status file, `pkg/diag` lists them
- `pkg/core/gc.go` — collects the certificate directory, including tailscaled's `<file>.tmp<digits>` files
- `pkg/docker/events.go` — `tslink.direct` defaults to `false` with `tslink.service`
- `docker/config.json` — `shared` mount, `TS_SHARED_DIR`, `TS_DEBUG_ACME_DIRECTORY_URL`
- `docs/credentials.md` — record the cluster credential as the default and the per-stack model as the alternative
