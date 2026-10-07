# Swarm spike, 2026-10-06

Can tslink give Docker Swarm tasks their own tailnet identity, as an alternative
to a per-service Tailscale sidecar? Run on a single-node swarm (OrbStack, Docker
29.4.0, arm64) against Matchory's production tailnet, with throwaway tags
`tag:svc-poc` (granted to the callee on port 80), `tag:svc-poc-nogrant` (no
grants) and `tag:svc-poc-callee`. All devices deleted and keys revoked after.

## Result

It works, with three changes to tslink. A Swarm task joined to a tslink network
gets its own tailscaled in its own network namespace; the callee
(`traefik/whoami`) saw the caller's own tailnet address, and `tailscale whois`
named it `tag:svc-poc`. Each tailscaled cost about 48 MB RSS.

## Docker side

- Swarm accepts managed (v2) network plugins; only v1 plugins are refused
  (`daemon/libnetwork/cnmallocator/provider.go`).
- The remote driver API is intact in 29.4.0
  (`daemon/libnetwork/drivers/remote`).
- For a `Scope: local` driver the manager stores only the driver name
  (`cnmallocator.Allocate`, the `isNodeLocal` branch), so the node's
  `CreateNetwork` receives an empty option map and the auth key is lost. Fix on
  this branch: declare `Scope: global` and return the options from
  `AllocateNetwork`, which Swarm stores as driver state and hands to every node.
  Side effect: the manager now allocates the network's IPAM pool; tslink ignores
  it and uses its own `/30`s.
- A task on an overlay network and a tslink network keeps Docker's default
  route via `docker_gwbridge`; tslink's veth adds only its `/30`.

## Required changes

1. **Swarm option propagation**, above. Committed nowhere yet (see below).
2. **Fail closed for tailnet addresses.** A task with no route to a tailnet peer
   fell through tslink's default route to the host, and left through the
   host's own tailscaled: the callee saw the Mac running the test, with admin
   rights, instead of a refused connection. On a fleet node it would leave as
   the node's tag. Adding `unreachable 100.64.0.0/10` and
   `unreachable fd7a:115c:a1e0::/48` to the task's main table fixed it:
   tailscaled's table 52 is consulted first, so granted peers still route via
   `tailscale0` and everything else is refused. Verified by hand with
   `nsenter`; tslink should install these routes at `Join`.
3. **Credentials and tags chosen by the platform, not by labels.** Today the
   auth key travels as a network option (readable with `docker network
   inspect`) and tags come from container labels, so any container on a
   network can claim any tag the key may apply. The credential should be
   selected by the task's stack, from plugin-side configuration.

## DNS

- A task on a tslink network alone cannot resolve public names through
  Docker's embedded resolver (SERVFAIL), probably because tslink reports no
  gateway (`DisableGatewayService`). Direct queries to the upstream and to
  `100.100.100.100` work.
- On overlay + tslink with `--dns 100.100.100.100`: MagicDNS short names, public
  names and Swarm service names on the overlay all resolve. The embedded
  resolver forwards from inside the task's namespace, so quad-100 is reachable
  through table 52.

## Also observed

- tailscaled starts after the container, driven by Docker events, so the
  application can make its first connections before the identity exists.
- Device names come from `tslink.hostname`, otherwise the Swarm task name.
- Stopping a task does not log the device out, and node state stays on the
  host under `/var/lib/docker-plugins/tailscale/by-hostname/`. With ephemeral
  keys the devices eventually expire; we deleted them by API.
- The plugin's event filter matches the driver name against the upstream image
  name (`ghcr.io/aaomidi/tslink`), so a locally renamed plugin never starts
  tailscaled.

## Not tested

- A tag-scoped OAuth client secret as the key (tslink runs `tailscale up
  --authkey`, which should accept one). Creating the client was declined by a
  permission check during the spike.
- CPU and latency under load; caller and callee shared one machine, so numbers
  would not compare with cross-node measurements.
- Multi-node swarms, rolling updates, reschedules, and Tailscale Services
  hosted from a task.
