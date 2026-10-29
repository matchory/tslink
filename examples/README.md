# Examples

Two Compose files to try tslink with. Both need the plugin installed with the alias `tslink`, as
[Install and upgrade tslink](../docs/guides/install-and-upgrade.md) describes.

## docker-compose.yml

One web server, served over HTTPS on a name of its own with Tailscale Serve:

```bash
export TS_AUTHKEY=tskey-auth-…
docker compose up -d
```

Then open `https://web-server.<tailnet>.ts.net` from a device in your tailnet.

## docker-compose-services.yml

Several backends behind one [Tailscale Service](https://tailscale.com/docs/features/tailscale-services).

1. In the [admin console](https://login.tailscale.com/admin/services), create a Service named `my-app`.
2. Use an auth key with tags: only tagged nodes can be a Service's backends.
3. Start the backends:

   ```bash
   export TS_AUTHKEY=tskey-auth-…
   docker compose -f docker-compose-services.yml up -d
   ```

4. Scale to three backends:

   ```bash
   docker compose -f docker-compose-services.yml up -d --scale backend=3
   ```

Open the Service at the address the admin console shows for it. [Expose a
service](../docs/guides/expose-a-service.md) goes further, with healthchecks and Swarm.
