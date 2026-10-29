---
title: Update without downtime
description: Roll out new versions of a Tailscale Service, restart Docker and upgrade tslink without callers seeing errors.
category: guide
tags: [updates, draining, services, swarm, maintenance]
---

# Update without downtime

When a Service backend stops, tslink drains it first, so callers move to the other backends before it goes away.
This guide sets up everything around that, so you can roll out a new version, restart Docker or upgrade tslink while
callers of the Service see no errors.

## Before you start

- The Service has more than one backend. See [Expose a service](expose-a-service.md).
- You have root access on each host.

## Set it up

1. **Give your application time to stop.** When Docker sends the stop signal, tslink drains the container's backend,
   and callers start moving to the other backends. They need a few seconds to notice, so your application has to
   keep serving for a few seconds after `SIGTERM`, then finish its requests and exit. Docker's default of 10 seconds
   is tight; set the time it waits with `stop_grace_period`:

   ```yaml
   services:
     backend:
       stop_grace_period: 15s
   ```

2. **Replace tasks only once the new one takes traffic.** Use the readiness healthcheck with `order: start-first`, as
   in [Expose a service](expose-a-service.md#make-the-containers-its-backends), so Swarm stops an old task only after
   the new one is a backend.

3. **Install the drain drop-in** on every host, as the next section describes.

4. **Before planned maintenance on a Swarm node, drain it**, and Swarm moves its tasks to other nodes:

   ```bash
   docker node update --availability drain <node>
   ```

5. **Upgrade tslink one host at a time**, as [Upgrade](install-and-upgrade.md#upgrade) describes. It drains the node
   first, because tailnet traffic stops for a few seconds while each tailscaled restarts.

## Install the drain drop-in

Docker sends no events when it stops itself, so tslink can't drain the backends of a host whose Docker restarts or
shuts down. This systemd drop-in drains every backend on the host before Docker gets `SIGTERM`.

1. From a clone of the repository, install the script and the drop-in:

   ```bash
   install -m 0755 deploy/systemd/tslink-drain /usr/local/sbin/tslink-drain
   install -d /etc/systemd/system/docker.service.d
   install -m 0644 deploy/systemd/tslink-drain.conf /etc/systemd/system/docker.service.d/
   systemctl daemon-reload
   ```

   The script needs `sh` and `timeout` (from coreutils). The drop-in takes effect the next time Docker stops; you
   don't need to restart Docker now.

2. If your setup differs from the defaults, set the script's environment with `Environment=` in the drop-in:

   | Variable | Default | What it is |
   | --- | --- | --- |
   | `TSLINK_DATA` | `/var/lib/docker-plugins/tailscale` | The data directory |
   | `TSLINK_TAILSCALE` | The plugin's `tailscale` CLI, under `/var/lib/docker/plugins` | The `tailscale` CLI. Set it if Docker uses a different data root. |
   | `TSLINK_DRAIN_WAIT` | `3` | Seconds to wait after the drain |

The drop-in can't help when dockerd crashes: nothing runs before it's gone.

## Check that it works

systemd uses the drop-in:

```bash
systemctl cat docker | grep tslink-drain
```

The output contains `ExecStop=/usr/local/sbin/tslink-drain`. Then restart Docker on one host while a client sends
requests to the Service:

```bash
systemctl restart docker
```

The client sees one failed request at most.

## Next steps

- [Monitor tslink](monitor.md): get an alert when a node isn't running.
- [Draining](../explanation/architecture.md#draining): how tslink knows a drain has reached the control server.
