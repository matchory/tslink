# Security testbed

A single Hetzner Cloud server that runs [`test/integration/run.sh`](../../integration/run.sh)
against a [headscale](https://github.com/juanfont/headscale) control server of its own, so no
Tailscale account is needed. It is the host the red team runs against:
[`../redteam.sh`](../redteam.sh) syncs a working tree to it, runs the end-to-end test, then the
probes of [`../redteam/`](../redteam/) and any private probe directory.

## Prerequisites

- `hcloud`, the Hetzner Cloud CLI.
- `~/.tslink-test/hcloud.token`: an API token for a Hetzner Cloud project used only for tests.
- `~/.ssh/id_ed25519`, the key injected into the server.

## Usage

```bash
./provision.sh    # create the testbed, write $TSLINK_SEC_DIR/ssh_config
../redteam.sh      # sync a tree, run the end-to-end test and the probes
./teardown.sh      # delete the testbed
```

A cpx22 bills for as long as it exists: tear it down with `./teardown.sh` once you are done.

`TSLINK_SEC_DIR` (default `~/.tslink-test/security`) holds the testbed's SSH state: `ssh_config`,
`known_hosts` and the firewall rules `provision.sh` applied. `./s CMD...` runs `CMD` as root on the
testbed; `./sync.sh [TREE]` copies a working tree there.
