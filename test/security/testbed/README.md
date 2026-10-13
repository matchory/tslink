# Security testbed

A disposable Linux host that runs [`test/integration/run.sh`](../../integration/run.sh)
against a [headscale](https://github.com/juanfont/headscale) control server of its own, so no
Tailscale account is needed. It is the host the red team runs against:
[`../redteam.sh`](../redteam.sh) syncs a working tree to it, runs the end-to-end test, then the
probes of [`../redteam/`](../redteam/) and any private probe directory.

## Prerequisites

- A disposable Linux host with Docker, `curl`, `jq`, `iptables` and `python3` (as in
  [`cloud-init.yaml`](cloud-init.yaml)), reached as root over SSH: `$TSLINK_SEC_DIR/ssh_config`
  with a `Host testbed` entry. The end-to-end test replaces the host's `tslink` plugin.

## Usage

```bash
../redteam.sh      # sync a tree, run the end-to-end test and the probes
```

## Optional: provisioning on Hetzner Cloud

`provision.sh` creates a testbed on Hetzner Cloud and writes `$TSLINK_SEC_DIR/ssh_config`;
`teardown.sh` deletes it. They need `hcloud`, the Hetzner Cloud CLI, an API token for a project used
only for tests in `~/.tslink-test/hcloud.token`, and `~/.ssh/id_ed25519`, the key injected into the
server. The server bills for as long as it exists: tear it down once you are done.

`TSLINK_SEC_DIR` (default `~/.tslink-test/security`) holds the testbed's SSH state: `ssh_config`,
`known_hosts` and the firewall rules `provision.sh` applied. `./s CMD...` runs `CMD` as root on the
testbed; `./sync.sh [TREE]` copies a working tree there.
