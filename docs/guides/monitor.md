# Monitor tslink

## Goal

Get an alert when the tailscaled of a container is not running.

## Prerequisites

- tslink is installed on the host.
- `jq` is installed on the host.
- For steps 3 and 4: the Prometheus node exporter, with its textfile collector in
  `/var/lib/node_exporter/textfile`.

Docker reports a container as healthy when its tailscaled is not running. Thus monitor the status files of tslink.

## Steps

1. Read the status files. tslink writes one file for each endpoint:

   ```bash
   jq -r '[.hostname, .stack // "-", .state, (.error // "")] | @tsv' \
     /var/lib/docker-plugins/tailscale/status/*.json
   ```

   The state is `running`, `retrying` or `failed`. The field `warnings` lists conditions to examine. For all fields,
   see [Status files](../reference/files-and-paths.md#status-files).

2. For more detail, run `tslink diag`. See [tslink diag](../reference/diag.md).

3. Export the states to Prometheus. Put this script in `/usr/local/sbin/tslink-metrics`:

   ```bash
   #!/bin/sh
   out=/var/lib/node_exporter/textfile/tslink.prom
   set -- /var/lib/docker-plugins/tailscale/status/*.json
   [ -e "$1" ] || set --
   jq -r '"tslink_endpoint_running{hostname=\"\(.hostname)\",stack=\"\(.stack // "")\"} " +
     (if .state == "running" then "1" else "0" end)' "$@" </dev/null >"$out.tmp" && mv "$out.tmp" "$out"
   ```

   Run it each minute, for example with this line in `/etc/cron.d/tslink-metrics`:

   ```text
   * * * * * root /usr/local/sbin/tslink-metrics
   ```

4. Add an alert for an endpoint that is not running for more than 5 minutes:

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

5. In the admin console, monitor the number of offline ephemeral nodes for each tag. A power loss or a crash of
   dockerd leaves ephemeral nodes behind. The control server removes them after some time.

## Verify

1. Read the metrics file:

   ```bash
   cat /var/lib/node_exporter/textfile/tslink.prom
   ```

   The file has one line for each endpoint, with the value `1` for a running endpoint.

2. In Prometheus, query `tslink_endpoint_running`. The query returns one series for each endpoint.

## Next steps

- [Troubleshoot](troubleshoot.md)
- [Files and paths](../reference/files-and-paths.md)
