# TLA+ models

TLA+ models of tslink's concurrent code, checked with TLC. A model verifies
the design as the model describes it, not the Go code: every step names the
Go function and step it stands for, and a change to a modelled function
updates the model in the same pull request.

## Running

```bash
make check-models                      # every model, its mutations and known violations
CHECK_MODELS_SKIP_GO=1 make check-models   # model work only: skips the Go test check
make check-models-selftest             # the runner itself, on fixture/
```

`scripts/check-models.sh` runs TLC from a container image and a jar pinned by
digest, so it needs Docker. For each `<module>.tla` it checks that the
committed PlusCal translation is current, then runs TLC on:

| File | Must |
| --- | --- |
| `<module>.cfg` | pass |
| `<module>-pass-<name>.cfg` | pass: the model at other bounds, or with other paths enabled |
| `<module>-mut-<name>.cfg` | violate the property of its `\* Expect:` line: a mutation of the code that the property must catch |
| `<module>-known-<id>.cfg` | violate its `\* Expect:` property: a violation the code still has (`\* Known: <id>`), which a Go test calling `knownViolation(t, "<id>")` reproduces |

Every invariant and property of `<module>.cfg` and of its `-pass-` cfgs
must be the `Expect` of a mutation: a property no mutation breaks is not
reported as checked. A temporal `Expect` works only in a cfg that lists that
one `PROPERTY` (TLC reports a violated `[]` conjunct of it as an invariant
by its line, which counts too). A fix
of a known violation updates the model and moves its property back into
`<module>.cfg`; a known violation that starts passing without one fails the
check.

After editing the PlusCal of a model, translate it in place
(`java -cp tla2tools.jar pcal.trans -nocfg <module>.tla`, with the jar the
script names) and commit the translation.

## `drain`: the drain gate

`drain.tla` (`\* Guards: G7`) models the drain gate of a container's Service
backends (`pkg/tailscale/drain.go`): a drained backend is not advertised
again, and a node leaves only once control has fetched its Service list since
tslink's last removal of a Service took effect. Its header lists the actors,
the abstractions and the mutations; labels (`Drain_marked`, `Change_locked`,
...) are the Go test hook points (`drain:marked`, `change:locked`, ...).

`drain.cfg` runs the core actors (backend starts and retries, the stop
signal, Leave, restarts) at two Services and one tailscaled restart. The
confirmer (`confirmAdvertisedLater`, `readvertise`), `pruneServices` and the
certificate lease holder are behind the constants `Confirmer`, `Prune` and
`Lease`, each enabled in its own `drain-pass-<path>.cfg` with one Service:
the paths together, or any of them with a restart and liveness, exceed half
an hour. Each cfg's header gives its bounds, why, and its cost; the
`-restart` ones check the safety properties with a restart, the others
liveness too. They use `VIEW CountView`, which keeps of each fetch count
read only whether a fetch was counted since (the model's header says why
that is exact).

Properties:

| Property | |
| --- | --- |
| S1 `ReadvertisedDrainedAgain` | an edit advertising a Service of a running daemon once drained, as a cancelled call carried out late, is undone: a drain again is requested, runs, or its edit is pending |
| S2 `DrainedFlagHoldsBack` | no change that took the lock once drained was set adds a Service |
| S2b `NoDaemonAdvertisesDrained` | a daemon started once drained is published without the Services, unless removing them failed or is in flight |
| S3 `LeaveWaitsForDrain` | Leave returns before its timeout only after control fetched the Service list since every removal of tslink's that took effect (the drain's, a prune's, readvertise's, the lease holder's) |
| S4 `NoWaitForNothing` | no wait for a fetch if no Service was ever in the prefs |
| S7 `AckEndsWait` | a counted fetch of the drained list ends the wait |
| S8 `NoLeaveWhileAdvertising` | Leave stops tailscaled only once it advertises no Service, or after its wait timed out |
| L1 `LeaveTerminates` | DrainAndWait returns, also if control never fetches |
| L2 `DrainGetsLock` | a drain gets the gate's lock |
| L3 `ConfirmerStops` | a confirmer re-advertises at most `advertiseNudges` times, and stops |

All hold. S2 as formalised covers the drained check a change makes once it
holds the lock. A change already past that check when the drain comes sends
its edit anyway; the cancel stops the call, not the request, which S1 and
S8 judge.

The model found twelve violations in v0.3.5 (I1 to I12, design
`2026-10-20-sec4-drain-gate-model-design.md`), fixed by tslink judging its
edits by tailscaled's log rather than by what their calls return. Each has a
Go test in `pkg/tailscale/drain_test.go` (I9 in `supervisor_test.go` and
`pkg/core/run_test.go`) that fails on v0.3.5, and a mutation that brings it
back:

| Id | Interleaving | Mutation |
| --- | --- | --- |
| I1 | a drain while tailscaled is down: the next tailscaled advertises from its state | `no-drain-at-start` |
| I2 | a fetch made before the drain's edit, counted after the count was read, passes for its acknowledgement | `count-before-edit` |
| I3 | a drain in a re-advertisement's gap finds nothing advertised: no wait for the removal | `drain-only-record` |
| I4 | a cancelled advertisement applied after the drain advertises the Services again | `no-redrain` |
| I5 | a drain edit that timed out still takes effect: no record, no wait | `record-on-return` |
| I6 | tailscaled exits between the drain and control's fetch: the wait ends with it | `no-follow-restart` |
| I7 | a cancelled advertisement applied before the drain: the backend counts as never advertised, no wait | (gone with `gate.advertised`) |
| I8 | Leave while a restarted tailscaled is not yet published: the drain finds no daemon, and the endpoint stops one that advertises | `no-follow-restart` |
| I9 | Leave during the first start: the start is aborted with no drain while tailscaled advertises | `abort-first-start` |
| I10 | the lease holder's unadvertised configuration removes a Service; a drain then finds nothing: no wait | `drain-only-record` |
| I11 | a prune edit that timed out still takes effect: no record, no wait | `drain-only-record`, `record-on-return` |
| I12 | a fetch made before the prune's edit, counted after its count was read, passes for control's fetch | `count-before-edit` |

Checking the fixes found three more, fixed and tested the same way: Leave
right after tailscaled crashed, before the supervisor replaced it, stopped
the restarted one (`TestLeaveAfterCrashWaitsForNextDaemon`); a tailscaled
restarted before the drain and published after it was never drained
(`TestLeaveDrainsDaemonPublishedAfterItsDrain`); a drain that cancelled an
advertisement and found nothing advertised returned before that call could
land (`TestCancelledAdvertisementSettledBeforeLeaving`, mutation
`no-settle`).
