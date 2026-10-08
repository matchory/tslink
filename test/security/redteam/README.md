# Red-team probes

Probes that [`../redteam.sh`](../redteam.sh) runs against the security testbed, in addition to the
full end-to-end test ([`test/integration/run.sh`](../../integration/run.sh)).

## The probe contract

A probe is an executable `*.sh`. `redteam.sh` copies it to the testbed and runs it there as root,
after the end-to-end test has finished and cleaned up, with the working tree at `/root/tslink`. It
must:

- set up whatever it needs and clean up after itself, so probes can run one after another on the
  same testbed;
- use a fresh payload (auth key, tag, container name, ...) on every attempt, rather than reusing
  one that a previous run or probe may have left behind;
- exit `0` for PASS (the attack was blocked), `1` for FAIL (it got through), `2` for BROKEN (its
  control run - the same attack with the defence disabled - did not get through, so it could not
  have caught a regression), or `3` for SKIPPED (an environment property the probe assumes does not
  hold here). Any other exit is reported as ERROR.

Its first comment lines document what it tests:

```bash
#!/bin/bash
# Guards: G2
# Attacker: A process in a container on a tslink network, as root inside it.
# Assumes: E1, E2, E5
```

`G<n>` is the guarantee of [SECURITY.md](../../../SECURITY.md) the probe tests, `Attacker:` describes
the attacker (matching the guarantee's own "Attackers:" line is fine), and `Assumes:` lists the
environment properties it assumes.

For a probe with a control run, [`../lib.sh`](../lib.sh) does the PASS/FAIL/BROKEN bookkeeping;
source it and call `probe_group`, `probe` and `run_probes` as `test/integration/run.sh` does.

## Where probes live

A probe for a finding that is not fixed yet does not belong in this public repository: keep it in a
private directory and pass it to `redteam.sh --probes DIR`. A probe moves into this directory once
its fix has shipped, so the red team keeps checking it on every run.
