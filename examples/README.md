# Examples

These Compose files need the plugin with the alias `tslink`. See
[Install and upgrade tslink](../docs/guides/install-and-upgrade.md).

## docker-compose.yml

One web server, served over HTTPS with Tailscale Serve.

```bash
export TS_AUTHKEY=tskey-auth-…
docker compose up -d
```

Open `https://web-server.<tailnet>.ts.net` from a device in the tailnet.

## docker-compose-services.yml

More than one backend for one [Tailscale Service](https://tailscale.com/kb/1438/services).

1. In the [admin console](https://login.tailscale.com/admin/services), create a Service with the name `my-app`.
2. Use an auth key with tags. A Service needs tagged backends.
3. Start the backends:

   ```bash
   export TS_AUTHKEY=tskey-auth-…
   docker compose -f docker-compose-services.yml up -d
   ```

4. To start three backends, scale the service:

   ```bash
   docker compose -f docker-compose-services.yml up -d --scale backend=3
   ```

Open the Service at the address that the admin console shows. For more, see
[Expose a service](../docs/guides/expose-a-service.md).
