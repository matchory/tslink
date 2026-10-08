# Update without downtime

## Goal

Update a Tailscale Service, restart Docker and upgrade tslink while the callers of the Service get no errors.

## Prerequisites

- The Service has more than one backend. See [Expose a service](expose-a-service.md).
- Root access on each host.

## Steps

1. Give the application time to stop. When Docker sends the stop signal, tslink drains the backend of the container,
   and callers move to other backends. Callers need some time to find out about the drain. Thus the application must
   continue to serve for some seconds after `SIGTERM`, then finish its requests and exit. Set the time that Docker
   waits with `stop_grace_period`:

   ```yaml
   services:
     backend:
       stop_grace_period: 15s
   ```

   The default of Docker is 10 seconds.

2. Use the readiness healthcheck with `order: start-first`. Then Swarm stops an old task only when the new task is a
   backend. See step 3 of [Expose a service](expose-a-service.md).

3. Install the drain drop-in on each host. See [Install the drain drop-in](#install-the-drain-drop-in).

4. Before planned maintenance on a Swarm node, drain the node:

   ```bash
   docker node update --availability drain <node>
   ```

   Swarm moves the tasks of the node to other nodes.

5. To upgrade tslink, follow [Upgrade](install-and-upgrade.md#upgrade). It drains the node first, because tailnet
   traffic stops for some seconds while each tailscaled restarts.

### Install the drain drop-in

Docker sends no events when it stops. Thus tslink cannot drain the backends of a host whose Docker stops, for example
at a restart or a reboot. The drop-in drains all backends on the host before Docker gets `SIGTERM`.

1. From a clone of the repository, install the script and the drop-in:

   ```bash
   install -m 0755 deploy/systemd/tslink-drain /usr/local/sbin/tslink-drain
   install -d /etc/systemd/system/docker.service.d
   install -m 0644 deploy/systemd/tslink-drain.conf /etc/systemd/system/docker.service.d/
   systemctl daemon-reload
   ```

   The script needs `sh` and `timeout` (coreutils). The drop-in takes effect at the next stop of Docker. You do not
   restart Docker.

2. If necessary, set environment variables for the script with `Environment=` in the drop-in:

   | Variable | Default | Content |
   | --- | --- | --- |
   | `TSLINK_DATA` | `/var/lib/docker-plugins/tailscale` | The data directory |
   | `TSLINK_TAILSCALE` | The `tailscale` CLI in the plugin, under `/var/lib/docker/plugins` | The `tailscale` CLI. Set it if Docker uses a different data root. |
   | `TSLINK_DRAIN_WAIT` | `3` | The seconds to wait after the drain |

The drop-in does not help when dockerd crashes.

## Verify

1. Make sure that systemd uses the drop-in:

   ```bash
   systemctl cat docker | grep tslink-drain
   ```

   The output contains `ExecStop=/usr/local/sbin/tslink-drain`.

2. Restart Docker on a host while a client sends requests to the Service. The client gets one failed request or
   none:

   ```bash
   systemctl restart docker
   ```

## Next steps

- [Monitor tslink](monitor.md)
- [Architecture: draining](../explanation/architecture.md#draining)
