---
title: Monitor tslink
description: Get an alert when a container's tailscaled isn't running, from tslink's status files through Prometheus.
category: guide
tags: [monitoring, prometheus, alerts, status]
---

# Monitor tslink

Docker reports a container as healthy whether or not its tailscaled runs: the plugin's work happens outside the
container's own processes. To find out when a node is down, watch tslink's status files. This guide exports them to
Prometheus and alerts on them.

## Before you start

- tslink is installed on the host.
- `jq` is installed on the host.
- For the Prometheus export, the node exporter runs with its textfile collector in
  `/var/lib/node_exporter/textfile`.

## Set it up

1. **Read the status files.** tslink writes one file per endpoint:

   ```bash
   jq -r '[.hostname, .stack // "-", .state, (.error // "")] | @tsv' \
     /var/lib/docker-plugins/tailscale/status/*.json
   ```

   Each one's state is `running`, `retrying` or `failed`, and its `warnings` list conditions worth a look.
   [Status files](../reference/files-and-paths.md#status-files) describes every field, and
   [`tslink diag`](../reference/diag.md) shows the same and more in one command.

2. **Export the states to Prometheus.** Put this script in `/usr/local/sbin/tslink-metrics`:

   ```bash
   #!/bin/sh
   out=/var/lib/node_exporter/textfile/tslink.prom
   set -- /var/lib/docker-plugins/tailscale/status/*.json
   [ -e "$1" ] || set --
   jq -r '"tslink_endpoint_running{hostname=\"\(.hostname)\",stack=\"\(.stack // "")\"} " +
     (if .state == "running" then "1" else "0" end)' "$@" </dev/null >"$out.tmp" && mv "$out.tmp" "$out"
   ```

   Run it every minute, for example from `/etc/cron.d/tslink-metrics`:

   ```text
   * * * * * root /usr/local/sbin/tslink-metrics
   ```

3. **Alert on an endpoint that hasn't run for 5 minutes:**

   ```yaml
   groups:
     - name: tslink
       rules:
         - alert: TslinkEndpointNotRunning
           expr: max_over_time(tslink_endpoint_running[5m]) == 0
           labels:
             severity: warning
           annotations:
             summary: "tailscaled of {{ $labels.hostname }} is not running"
   ```

4. **Watch for offline ephemeral nodes** per tag in the admin console. A power loss or a dockerd crash leaves
   ephemeral nodes behind, offline, until the control server removes them.

## Check that it works

The metrics file has one line per endpoint, with `1` for each running one:

```bash
cat /var/lib/node_exporter/textfile/tslink.prom
```

In Prometheus, `tslink_endpoint_running` returns one series per endpoint.

## Next steps

- [Troubleshoot](troubleshoot.md): find the cause when an endpoint isn't running.
- [Files and paths](../reference/files-and-paths.md): where tslink keeps its state, logs and status files.
