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
  have caught a regression), `3` for SKIPPED (an environment property the probe assumes does not
  hold here, or it is a probe of another testbed), or `4` for INFO (it passed, and its control
  violates an environment property, so it shows what an operator gives up; or it only records a
  behaviour). Any other exit is reported as ERROR.

Its first comment lines document what it tests:

```bash
#!/bin/bash
# Guards: G2
# Testbed: t1
# Attacker: A1, a process in a container on a tslink network, as root inside it.
# Assumes: E1, E2, E5
# Attack: what the attacker does, and what counts as getting through.
# Control: how the control run disables the defence, and how its teardown restores it.
```

`G<n>` is the guarantee of [SECURITY.md](../../../SECURITY.md) the probe tests, which
`internal/secmodel` counts; a probe of no guarantee has no `Guards:` line. `Testbed:` is `t1`, the
single host, or `tn`, test/cluster's Swarm in a Tailscale tailnet. `Attacker:` names the attacker
class (A1 container, A2 peer, A3 stack author, A5 control) and describes the attacker (matching the
guarantee's own "Attackers:" line is fine), and `Assumes:` lists the environment properties it
assumes. A probe that needs Tailscale Services says `Needs: services`.

For a probe with a control run, [`../lib.sh`](../lib.sh) does the PASS/FAIL/BROKEN bookkeeping;
source it, call `probe_group`, `probe` and `run_probes` as `test/integration/run.sh` does, and end
with `probe_exit`.

## Testbeds

A `t1` probe sources [`../t1.sh`](../t1.sh), whose `t1_up` installs the plugin built from the
working tree, headscale and the host's own tailscaled, and removes them when the probe exits.

A `tn` probe sources [`../tn.sh`](../tn.sh) and runs on the operator's machine, reaching the bed's
nodes over SSH with test/cluster's helpers: run it directly, as `test/security/redteam/<probe>.sh`.
`redteam.sh` runs only the single host's probes: a `tn` probe it copies there reports SKIPPED.

## Where probes live

A probe for a finding that is not fixed yet does not belong in this public repository: keep it in a
private directory and pass it to `redteam.sh --probes DIR`. A probe moves into this directory once
its fix has shipped, so the red team keeps checking it on every run.
