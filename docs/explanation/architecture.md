# Architecture

This page tells how tslink gives each container its own node in the tailnet, and why it works as it does.

## Overview

tslink is a Docker network plugin. For each container on a tslink network, the plugin starts a tailscaled in the
network namespace of the container. The container is then a node in the tailnet, with its own identity and tailnet
addresses.

```text
┌────────────────────────────────────────────────────────────┐
│ Host                                                       │
│                                                            │
│  ┌──────────────────────────────────────────┐              │
│  │ Container (tailnet address 100.x.y.z)    │              │
│  │                                          │              │
│  │ App ─► tailscale0 ─► tailscaled ─► veth ─┼─► NAT ───────┼─► Tailnet
│  │                                          │              │
│  │ App ─► eth0 ─────────────────────────────┼─► Docker ────┼─► Internet
│  └──────────────────────────────────────────┘              │
│                                                            │
│  ┌──────────────────────────────┐                          │
│  │ Plugin                       │                          │
│  │ • Runs tailscaled            │                          │
│  │ • Creates the veth pair      │                          │
│  │ • Routes and NATs the veth   │                          │
│  └──────────────────────────────┘                          │
└────────────────────────────────────────────────────────────┘
```

The container has two paths out:

- **The tailnet.** Traffic to tailnet addresses goes through `tailscale0` to the tailscaled of the container.
  tailscaled sends its encrypted traffic through a veth pair that tslink adds to the container.
- **All other traffic.** It goes through `eth0` and the usual gateway of Docker. On a Swarm, this is
  `docker_gwbridge`.

## Start-up

Docker requires the plugin to answer the request to connect a container quickly. A tailscaled login can take more
than 60 seconds. Thus tslink starts Tailscale in two parts:

1. When Docker connects the container, tslink creates the veth pair and answers at once.
2. tslink watches the events of Docker. When the container has started, tslink reads its name and labels, and starts
   tailscaled in the background.

During the start, the container has no node. Connections to tailnet addresses fail at once. See
[Start-up delay](limitations.md#start-up-delay).

## Identity

tslink keeps the state of a node by hostname, not by container ID. When a container starts with the hostname of an
earlier container, it gets the earlier identity: the same node and the same tailnet addresses.

Containers in a stack have their state directories in the directory of the stack. See
[State directories](security-model.md#state-directories-are-in-the-directory-of-the-stack). One endpoint at a time can
use a state directory.

## Routing

Docker gives the container its usual gateway. Without a gateway, the DNS server of Docker does not resolve public
names.

The veth pair carries only the traffic of tailscaled. tailscaled marks its sockets, and a routing rule sends marked
traffic to a table with a default route through the veth pair. In the main routing table, the tailnet address ranges
are unreachable. Thus tailnet traffic leaves the container through its own tailscaled, or it does not leave.

Each veth pair gets a /30 subnet from `10.200.0.0/16`.

## Isolation

The container reaches the tailnet only as its own node, not as the host. Routes stop sockets, but not raw frames, and
the tailscaled of a host accepts forwarded traffic. Thus the host also enforces the isolation with firewall rules:

- **Host isolation.** Containers on the host cannot reach the tailnet through the tailscaled of the host. This applies
  to all containers, on tslink networks and on other networks. The plugin setting `TSLINK_ISOLATE_HOST_TAILNET`
  controls it. tslink installs the rules before it accepts requests, restores them when they change, and keeps them
  when the plugin stops.
- **Isolation between containers.** Containers cannot reach the veth addresses of other containers. Only the
  WireGuard traffic of tailscaled passes between them, on UDP port 41641. Thus two nodes on one host keep a direct
  connection, and do not use a DERP relay.

The guarantee and the conditions that it relies on are `G1` in [SECURITY.md](../../SECURITY.md).

## DNS

tailscaled resolves names, and forwards queries for names outside the tailnet. It uses the first of these sources
that gives a DNS server:

1. The DNS servers of the container (`dns` of the container or the service). tslink removes the servers of Tailscale
   (`100.100.100.100`, `fd7a:115c:a1e0::53`) and of Docker (`127.0.0.11`). It keeps servers on the loopback
   interface, such as `127.0.0.1`, because they run in the network namespace that tailscaled shares.
2. The DNS servers in `/etc/resolv.conf` of the host, without the servers on the loopback interface of the host.
3. The upstream servers of systemd-resolved, in `/run/systemd/resolve/resolv.conf`.

Docker uses the same sources. If no source gives a server, tailscaled uses the DNS server of Docker. If the DNS
server of the container is `100.100.100.100`, tailscaled uses the public servers of Google, the default of Docker. In
both cases, tslink writes the warning `dns-upstreams` in the plugin log and the status file.

tailscaled selects its servers when it starts, as Docker does when the container starts. tslink does not read the
`dns` setting in `/etc/docker/daemon.json`. It reads only the `dns` setting of the container or the service.

## Self-healing

tslink recovers from these failures without help:

| Failure | What tslink does |
| --- | --- |
| tailscaled does not start | It tries again, with longer intervals, until the container leaves. It does not try again after a permanent error, such as a tag that is not permitted. |
| tailscaled stops | It restarts tailscaled. After a series of crashes, it waits, then continues. |
| The control server does not know the node, for example because the device was deleted | It logs the node in again. |
| The plugin restarts | It finds the running containers again. A node that is still logged in starts without the credential. Thus a rotated or revoked credential does not stop it. |
| A container stops without a message to tslink, for example at a power loss | It logs out the ephemeral nodes that no container uses, and deletes their state. Thus a container with a fixed `tslink.hostname` gets its name back, and not `<hostname>-1`. |

## Draining

When a container is a backend of a Tailscale Service, tslink drains the backend before the container stops:

1. Docker sends the stop signal. tslink sees the event and drains the backend. Callers move to other backends.
2. If a container stops without the stop signal, because it crashed or completed, tslink drains the backend when the
   container leaves the network.
3. tslink waits until the control server has the drained list of backends: at least 1 second and at most 10 seconds.
4. Then tslink logs out an ephemeral node and deletes its state, or stops tailscaled.

If tslink stopped the node before the control server knew about the drain, callers would send traffic to the node
for some minutes. A drain is final: tslink does not make the container a backend again, also after a restart of
tailscaled.

When Docker stops, it sends no more events. The [drain drop-in](../guides/zero-downtime-updates.md#install-the-drain-drop-in)
drains the backends of the host before Docker stops.

## Throughput

These values were measured on a 4-vCPU cloud VM with Ubuntu 24.04 and Docker 29.8:

| Condition | Tailnet throughput |
| --- | --- |
| The host itself | 2.8 Gbit/s |
| A container, without UDP GRO forwarding | 1.2 Gbit/s, which is 40 % of the host |
| Without outbound UDP, through a DERP relay | 25 Mbit/s |

Thus [Provision a host](../guides/provision-a-node.md) enables UDP GRO forwarding.

## Binaries

The plugin image contains the Tailscale binaries that tslink runs. tslink downloads nothing at runtime. Each tslink
release pins one Tailscale version, and Dependabot updates it. tailscaled writes its output to `tailscaled.log` in the
state directory.
