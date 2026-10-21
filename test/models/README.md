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
backends (`pkg/tailscale/drain.go`): a drained backend is never advertised
again, and a node leaves only once control has fetched its drained Service
list. Its header lists the actors, the abstractions and the mutations;
labels (`Drain_marked`, `Change_locked`, ...) are the Go test hook points
(`drain:marked`, `change:locked`, ...).

`drain.cfg` runs the core actors (backend starts and retries, the stop
signal, Leave, restarts) at two Services and one tailscaled restart. The
confirmer (`confirmAdvertisedLater`, `readvertise`), `pruneServices` and the
certificate lease holder are behind the constants `Confirmer`, `Prune` and
`Lease`, each enabled in its own `drain-pass-<path>.cfg` with one Service:
the paths together, or any of them with a restart and liveness, exceed half
an hour. Each pass cfg's header gives its bounds, why, and its cost; the
`-restart` ones check the safety properties with a restart, the others
liveness too. They use `VIEW CountView`, which keeps of each fetch count
read only whether a fetch was counted since (the model's header says why
that is exact).

Properties:

| Property | | Result |
| --- | --- | --- |
| S1 `DrainedStaysDrained` | once a drain is recorded (external included), no Service is advertised again | violated: I4 |
| S2 `DrainedFlagHoldsBack` | no change that took the lock once drained was set adds a Service | holds |
| S2b `NoDaemonAdvertisesDrained` | a daemon started once drained advertises nothing | violated: I1 |
| S3 `LeaveWaitsForDrain` | Leave returns before its timeout only after control fetched the drained list | violated: I2, I5, I6, I7 |
| S4 `NoWaitForNothing` | no wait if no Service was ever advertised | holds |
| S5 `PrunedWaitedFor` | once a prune removed a Service, Leave drains and returns early only after control fetched the list since | violated: I11, I12; `S5Recorded` (a prune whose edit returned, Leave not waiting) holds |
| S6 `NoHiddenDrain` | a drain is recorded external only if no edit of tslink's removed a Service of the container since control's last fetch | holds for the core actors; violated: I3, I10, I11 |
| S7 `AckEndsWait` | a counted fetch of the drained list ends the wait | holds |
| S8 `NoLeaveWhileAdvertising` | Leave stops tailscaled only once no running daemon advertises a Service, or after a drain took effect and Leave waited for its fetch or timeout | violated: I8, I9 |
| L1 `LeaveTerminates` | DrainAndWait returns, also if control never fetches | holds |
| L2 `DrainGetsLock` | a drain gets the gate's lock | holds |
| L3 `ConfirmerStops` | a confirmer re-advertises at most `advertiseNudges` times, and stops | holds |

S2 as formalised covers the drained check a change makes once it holds the
lock. A change already past that check when the drain comes sends its edit
anyway; the cancel stops the call, not the request, and that window is
judged by S1 (I4).

S3 is split by how DrainAndWait returned (`S3Acked`, `S3Ctx`,
`S3NoRecord`, `S3External`, `S3NoDaemon`, `S3Unadvertised`), S5 by the
prune and the return (`S5Recorded`, `S5Unrecorded`, `S5Acked`), S6 by the
removal a drain hid (`S6Gap`, `S6Lease`) and S8 by whether Leave ran
DrainAndWait (`S8Drained`), so that each known violation's cfg finds its own
trace. S5 leaves to S3 and S8 the returns they judge for the drain itself
(I5, I6, I8, I9), and S6 the removal by the other drainer's edit (I5). Known violations, each with its trace
in its cfg:

| Id | Interleaving |
| --- | --- |
| I1 | a drain while tailscaled is down: the next tailscaled advertises from its state |
| I2 | a fetch made before the drain's edit, counted after the count was read, passes for its acknowledgement |
| I3 | a drain in a re-advertisement's gap finds nothing advertised and is recorded external: no wait for the removal |
| I4 | a cancelled advertisement applied after the drain advertises the Services again |
| I5 | a drain edit that timed out still takes effect: no drain record, no wait |
| I6 | tailscaled exits between the drain and control's fetch: the wait ends with it |
| I7 | a cancelled advertisement applied before the drain: the backend counts as never advertised, no wait |
| I8 | Leave while a restarted tailscaled is not yet published: the drain finds no daemon, and the endpoint stops one that advertises |
| I9 | Leave during the first start: the start is aborted with no drain while tailscaled advertises Services from its state or added during the start |
| I10 | the lease holder's unadvertised configuration removes a Service the state advertised; a drain then is recorded external: no wait |
| I11 | a prune edit that timed out still takes effect: no prune record, an external drain, no wait |
| I12 | a fetch made before the prune's edit, counted after its count was read, passes for control's fetch of the removal |
