---- MODULE drain ----
\* Guards: G7
\*
\* The drain gate of a container's Service backends (pkg/tailscale/drain.go):
\* a drained backend is never advertised again, and a node leaves only once
\* control has fetched its drained Service list. The model describes the code
\* as it is; it verifies the design, not the Go code.
\*
\* Actors (PlusCal processes), each step naming the Go function it models:
\*   sup          DaemonSupervisor.supervisionLoop, startDaemon, daemon.Start
\*                (noteSurvivingState, configureServing: each backend's
\*                serviceBackend.start in turn), waitForExit, stopDaemon
\*   retry        serviceBackend.retryLater, of every backend
\*   stop         the stop signal: DaemonSupervisor.Drain (or none), once
\*                the endpoint has its supervisor
\*   leave        DaemonSupervisor.DrainAndWait, then the endpoint's Stop;
\*                during the first start, aborting it without a drain
\*   tailscaled   applies a LocalAPI prefs edit, or refuses one adding a
\*                Service
\*   control      fetches the node's Service list after a change, or never
\*   counter      handleLine: tslink counts a fetch, later
\*   crash        tailscaled exits
\*   conf_<s>     confirmAdvertisedLater(s) and readvertise (Confirmer)
\*   lease_<s>    configureServiceAfterCert(s): configureServiceWithCert,
\*                configureServiceBackend(false), requestCert with its
\*                certDomainGrace advertise, the final advertise (Lease)
\*   issuer       tailscaled issues the lease holder's certificate, or not
\* The supervisor's start also runs pruneServices (Prune), with a Service
\* the container does not name in the node's state.
\* The procedures Configure (configureServiceWhenCertified,
\* configureServiceBackend, serviceGate.change, runAdvertise, addAdvertised;
\* advertise for the lease holder)
\* and DrainProc (DaemonSupervisor.Drain, markDrained, daemon.Drain,
\* editAdvertised) are the code both kinds of caller share.
\*
\* Labels name the point reached, <Function>_<step>: a process at
\* Drain_marked has marked the gate drained and is to take its lock. They
\* are the Go test hook points (drain:marked, ...).
\*
\* Abstractions (each also at the step it concerns):
\* - Time is abstract. A LocalAPI call's timeout, DrainAndWait's timeout and
\*   floor are events, not durations. The floor's sleep is part of Leave's
\*   last step. The deadline does not fire in the same instant as a fetch is
\*   counted (Go's select would pick either): Wait_block times out only if
\*   no fetch was counted since Wait_check read the count.
\* - A change's calls have no timeout here: tailscaled may hold an edit
\*   until the change's context is cancelled, so liveness cannot rest on
\*   their 10 s timeouts (DrainGetsLock). The drain's own edit, on the
\*   daemon's context, times out: a fair step.
\* - A cancelled or timed-out call returns, and its request stays pending:
\*   tailscaled may apply it later, after other edits (the design's
\*   conservative assumption). tailscaled's exit drops its pending requests
\*   (it may have applied them before).
\* - editServe is not modelled: it is taken to succeed at once. Failures
\*   that make the start retry are tailscaled refusing an add instead.
\* - GetPrefs and the EditPrefs built from it are one step. An edit applied
\*   between them in Go has the same outcome as one applied after the send,
\*   before ours: EditPrefs writes the whole list.
\* - Steps are merged where no other actor can tell them apart: the outer
\*   isDrained checks with change's lock, markDrained's store with its
\*   cancel, getDaemon with daemon.Drain's lock and count read (see each).
\* - Control may fetch once after any change of the advertised Services and
\*   after tailscaled starts, later, or never. tslink counts a fetch later,
\*   in the model only when an actor is to read the count: nothing else
\*   reads it, so an earlier count is the same.
\* - Restarts are tailscaled exits, at most MaxRestarts; a start that fails
\*   (checkServiceHost, socket) is the same as an exit during the start.
\*   The restart for new host endpoints (hostEndpointsChanged) is one too.
\* - retryLater: one process polls the backends whose start failed, in any
\*   order, while their tailscaled runs (a poll while it is down fails at
\*   once and changes nothing).
\* - Once Leave stopped the supervisor, no step is explored
\*   (ACTION_CONSTRAINT Running in each cfg): nothing the properties read
\*   changes after it.
\* - The confirmer, pruneServices and the lease holder are behind the
\*   constants Confirmer, Prune and Lease: drain.cfg runs the core actors,
\*   drain-pass-*.cfg each path (bounds and cost in each cfg's header).
\* - A fetch count is only compared with later counts of the same daemon:
\*   the pass cfgs with a path enabled use VIEW CountView, which keeps of
\*   each count read only whether a fetch was counted since (see there).
\* - markAdvertised and fetchMark are not modelled: they feed the status
\*   warning and readiness, which no property reads. readvertise's count
\*   read is therefore no reader of the count (Reads).
\* - A confirmer starts once the one before it of the same Service returned
\*   (two at once need the lease holder's final advertise to land in a
\*   readvertise's gap); a confirmer of a stopped daemon ends at its next
\*   step, which in Go fails on its cancelled context.
\* - pruneServices' lock and send are one step, and so are readvertise's
\*   answers and unlocks: only markDrained can come between, and its
\*   cancel finds the call returned or not sent, as before or after.
\* - pruneServices' EditPrefs times out (a fair step), unlike a
\*   configuration's: NeedsDrain waits for the lock it holds.
\* - The lease holder always gets the lease; a replica without it waits
\*   for the certificate and configures the Service as a retry does. The
\*   certificate is issued while the holder asks for it, or never.
\* - Not modelled: checkApprovalLater, the certificate expiry watcher,
\*   editServe failures (prune's included).
\*
\* Bounds: drain.cfg two Services, one restart, the core actors;
\* drain-pass-<path>.cfg one Service, as each cfg's header says.
\*
\* Mutations (drain-mut-<name>.cfg, each must break its property):
\*   no-drained-check  no isDrained/drained check on the configure path
\*   always-wait       DrainAndWait ignores external and advertised
\*   ignore-restart    DrainAndWait keeps drain.fetches after a restart
\*   wait-no-timeout   waitServicesFetch has no deadline
\*   no-cancel         markDrained cancels nothing
\*   drain-records-external-always  daemon.Drain records every drain
\*                     external
\*   prune-no-record   pruneServices records no removal in g.prune
\*   unbounded-nudges  the confirmer does not stop after advertiseNudges
EXTENDS Naturals, Sequences, TLC

CONSTANTS
    Services,       \* the container's Services
    MaxRestarts,    \* how often tailscaled may exit and restart
    MaxNudges,      \* advertiseNudges: the confirmer's re-advertisements
    Confirmer,      \* model confirmAdvertisedLater and readvertise
    Prune,          \* model pruneServices, and a stale Service in the state
    Lease,          \* the Services serve HTTPS and have no certificate yet:
                    \* the certificate lease holder configures them
    Mutation        \* "none", or the name of a mutation (drain-mut-*.cfg)

None == "none"
Stale == "x"                        \* a Service the container does not name
Changers == {"sup", "retry"}       \* run changes of the gate
Drainers == {"stop", "leave"}       \* call DrainProc
ConfOf(s) == "conf_" \o s           \* the confirmer of s
LeaseOf(s) == "lease_" \o s         \* the certificate lease holder of s
Confs == {ConfOf(s) : s \in Services}
Leases == {LeaseOf(s) : s \in Services}
SvcOf(p) == CHOOSE s \in Services : p \in {ConfOf(s), LeaseOf(s)}
Actors == Changers \cup Drainers \cup Confs \cup Leases
Gens == 1..(MaxRestarts + 1)        \* tailscaled processes, by start
NoReq == [from |-> None, gen |-> 0, val |-> {}, kind |-> None]
NoRec == [set |-> FALSE, gen |-> 0, fetches |-> 0, external |-> FALSE]
NoConf == [gen |-> 0, after |-> 0]

(*--algorithm drain
variables
    \* tailscaled and control
    stateExists \in BOOLEAN,        \* StateExists(StateDir)
    \* the node's persisted AdvertiseServices; from a previous run, if any,
    \* which may name a Service the container no longer names (Prune)
    prefs \in IF stateExists
              THEN {{}, Services} \cup
                   (IF Prune THEN {{Stale}, Services \cup {Stale}} ELSE {})
              ELSE {{}},
    daemonGen = 0,                  \* the latest tailscaled started
    daemonUp = FALSE,               \* it runs
    ctxDone = [g \in Gens |-> FALSE], \* daemon g's context is cancelled
    crashes = 0,
    pending = {},                   \* EditPrefs sent, not applied
    waitOn = [a \in Actors |-> NoReq], \* the request a's call waits for
    refused = [a \in Actors |-> FALSE], \* tailscaled refused a's edit
    dirty = FALSE,                  \* control has something to fetch
    fetchQ = [g \in Gens |-> <<>>], \* fetches made, not yet counted
    fetchCount = [g \in Gens |-> 0], \* d.fetches, per daemon from 0
    certIssued = [s \in Services |-> FALSE], \* s's certificate exists
    \* the supervisor
    supDaemon = 0,                  \* s.daemon; 0 is nil
    stopped = FALSE,                \* the endpoint stopped the supervisor
    retryGen = [s \in Services |-> 0], \* retryLater runs, for daemon g
    confReq = [s \in Services |-> NoConf], \* confirmAdvertisedLater(s) asked
    leaseReq = [s \in Services |-> 0], \* configureServiceAfterCert(s) asked
    \* the gate (serviceGate)
    drained = FALSE,
    advertised = FALSE,
    lock = None,                    \* g.mu's holder
    cancelReg = None,               \* g.cancel: whose change it cancels
    cctx = [a \in Actors |-> FALSE], \* a's change context is cancelled
    drainRec = NoRec,               \* g.drain
    pruneRec = NoRec,               \* g.prune
    unpruned = FALSE,               \* g.unpruned
    \* per actor
    gen = [a \in Actors |-> 0],     \* the daemon a works on (b.d, d)
    res = [a \in Actors |-> "ok"],  \* Configure's fn: ok, added, err
    out = [a \in Actors |-> "ok"],  \* what Configure returned to a lease
                                    \* holder: ok, err, drained
    mark = [a \in Actors |-> 0],    \* fetches read by runAdvertise
    cnt = [a \in Actors |-> 0],     \* fetches read by daemon.Drain, prune
    \* DrainAndWait
    leaveRec = NoRec, leaveD = 0, after = 0, seen = 0, leaveEnd = None,
    \* history, for the properties
    everInPrefs = (prefs # {}),     \* a Service was ever in the prefs
    lockedDrained = [a \in Actors |-> FALSE], \* a's change began drained
    addAfterDrained = FALSE,        \* such a change sent an add
    startedDrained = FALSE,         \* the running daemon started drained
    drainApplied = FALSE,           \* daemon.Drain's edit took effect
    \* control fetched the drained list since (none if advertised again)
    ackMade = FALSE,
    ackCounted = [g \in Gens |-> FALSE], \* tslink counted such a fetch
    ackTimedOut = FALSE,            \* Leave timed out with one counted
    published = FALSE,              \* the first daemon was published
    leftAdvertising = FALSE,        \* Leave stopped a daemon advertising
    pruneApplied = FALSE,           \* a prune's edit took effect
    pruneFetched = FALSE,           \* control fetched the list since
    pruneOK = FALSE,                \* a prune's edit returned: removed
    \* actors whose edit removed a Service of the container since control
    \* last fetched the list
    unfetched = {},
    \* the actors whose removal a drain recorded external hid, and where
    \* they were then
    hiddenAt = {};

define
    \* A LocalAPI call of a's daemon reaches tailscaled
    Live(a) == daemonUp /\ daemonGen = gen[a] /\ ~ctxDone[gen[a]]
    \* ... and a's change context is not cancelled
    CallOK(a) == Live(a) /\ ~cctx[a]
    \* a's EditPrefs request was applied, and its answer can arrive
    Applied(a) == waitOn[a] \notin pending /\ ~refused[a] /\ Live(a)
    \* A retry of s polls: its daemon runs, or stopped
    RetryDue(s) ==
        /\ retryGen[s] # 0
        /\ \/ ctxDone[retryGen[s]]
           \/ retryGen[s] = daemonGen /\ daemonUp
    Checks == Mutation # "no-drained-check"
    Cancels == Mutation # "no-cancel"
    Ack == drainApplied /\ prefs = {}
    \* NeedsDrain: the container names a Service, or a prune removed one or
    \* failed to. It takes g.mu to read g.prune: NeedsDrainRead is when that
    \* read can happen
    NeedsDrain == Services # {} \/ unpruned \/ pruneRec.set
    NeedsDrainRead == Services # {} \/ unpruned \/ lock = None
    \* An actor reads daemon g's fetch count in its next step. Counting a
    \* fetch changes nothing else, so the model counts only then: an
    \* earlier count is the same as one just before the read.
    Reads(g) ==
        \/ pc["leave"] \in {"Wait_check", "Wait_block"} /\ leaveD = g
        \/ \E p \in Drainers : pc[p] = "Drain_marked" /\ supDaemon = g
        \* runAdvertise's read, the confirmer's after
        \/ Confirmer /\ \E p \in Changers \cup Leases :
               pc[p] = "Change_locked" /\ gen[p] = g
        \* waitServicesFetch and the next after, of a confirmer
        \/ \E c \in Confs : pc[c] = "Confirm_wait" /\ gen[c] = g
        \* pruneServices' read
        \/ pc["sup"] = "Prune_begin" /\ gen["sup"] = g

    \* S1: once a drain is recorded (it took effect, or found nothing
    \* advertised), no Service is advertised again
    DrainedStaysDrained == drainRec.set => prefs = {}
    \* S2: no change that began once drained was set adds a Service
    DrainedFlagHoldsBack == ~addAfterDrained
    \* S2b: a daemon started once drained was set runs without the Services
    NoDaemonAdvertisesDrained == daemonUp /\ startedDrained => prefs = {}
    \* S3: Leave returns before its timeout only after control fetched the
    \* drained Service list, once a drain's edit took effect. leaveEnd is
    \* how DrainAndWait returned: acked (a fetch was counted), ctx (the
    \* daemon stopped), timeout, or without a wait: norecord (no drain took
    \* effect), external, nodaemon, unadvertised; aborted is a Leave during
    \* the first start, which runs no DrainAndWait, and nodrain one that
    \* NeedsDrain spared it.
    LeaveReturnedEarly(how) ==
        pc["leave"] = "Done" /\ leaveEnd \in how /\ drainApplied => ackMade
    LeaveWaitsForDrain ==
        LeaveReturnedEarly({"acked", "ctx", "norecord", "external",
                            "nodaemon", "unadvertised"})
    \* S3 for one way of returning, to tell the violations apart
    S3Acked == LeaveReturnedEarly({"acked"})
    S3Ctx == LeaveReturnedEarly({"ctx"})
    S3NoRecord == LeaveReturnedEarly({"norecord"})
    S3External == LeaveReturnedEarly({"external"})
    S3NoDaemon == LeaveReturnedEarly({"nodaemon"})
    S3Unadvertised == LeaveReturnedEarly({"unadvertised"})
    \* S8: Leave stops tailscaled only once no running daemon advertises a
    \* Service of the container, or after a drain took effect and Leave
    \* waited for its fetch or its timeout
    NoLeaveWhileAdvertising == ~leftAdvertising
    \* S8 for a Leave that ran DrainAndWait (not one aborting the first
    \* start), to tell the violations apart
    S8Drained == leftAdvertising => leaveEnd = "aborted"
    \* S4: no wait if no Service was ever in the prefs
    NoWaitForNothing ==
        pc["leave"] \in {"Wait_check", "Wait_block"} => everInPrefs
    \* S5: once a prune's edit took effect, Leave drains the node and
    \* returns before its timeout only after control fetched the list since.
    \* The returns S3 and S8 judge for the drain itself (ctx, norecord,
    \* nodaemon: I5, I6, I8; aborted: I9) are left to them.
    PrunedWaitedFor ==
        pc["leave"] = "Done" /\ pruneApplied
        /\ leaveEnd \in {"acked", "external", "unadvertised", "nodrain"}
        => pruneFetched
    \* S5 for a prune whose edit returned, so pruneServices noted it, and a
    \* Leave that did not wait (acked is S3's I2, or S5Acked)
    S5Recorded ==
        pc["leave"] = "Done" /\ pruneOK
        /\ leaveEnd \in {"external", "unadvertised", "nodrain"}
        => pruneFetched
    \* S5 for a prune edit that took effect without returning (no record)
    S5Unrecorded ==
        pc["leave"] = "Done" /\ pruneApplied /\ ~pruneOK
        /\ leaveEnd \in {"external", "unadvertised", "nodrain"}
        => pruneFetched
    \* S5 for a Leave acked on the prune's count (its drain found nothing)
    S5Acked ==
        pc["leave"] = "Done" /\ pruneApplied /\ leaveEnd = "acked"
        /\ drainRec.external => pruneFetched
    \* S6: a drain is recorded external only if no edit of tslink's removed a
    \* Service of the container since control last fetched the list. An
    \* edit of the other drainer is left out: one that took effect without
    \* a record is I5 (S3)
    NoHiddenDrain == hiddenAt = {}
    \* The removals a drain by p recording itself external hides: those
    \* control has not fetched, but for the other drainer's (I5's)
    Hidden(p) == {<<a, pc[a]>> : a \in unfetched \ (Drainers \ {p})}
    \* S6 for a removal by readvertise with the drain in its gap (I3), and
    \* for a removal by the lease holder (I10)
    S6Gap == \A h \in hiddenAt : h[2] # "Readvertise_gap"
    S6Lease == \A h \in hiddenAt : h[1] \notin Leases
    \* S7: a counted fetch after the drain ends the wait before its timeout
    AckEndsWait == ~ackTimedOut
    \* L1: DrainAndWait returns, also if control never fetches
    LeaveTerminates == <>(pc["leave"] = "Done")
    \* L2: a drain gets the gate's lock: the change holding it returns
    DrainGetsLock ==
        \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"
end define;

\* configureServiceWhenCertified and configureServiceBackend(true) for svc,
\* by the supervisor's start (serviceBackend.start, which asks for
\* retryLater if it fails) or by the retry; and advertise(svc), by the
\* certificate lease holder (configureServiceBackend's own drained checks
\* are the same as change's).
procedure Configure(svc)
begin
Change_lock:
    \* configureServiceWhenCertified, configureServiceBackend: isDrained();
    \* serviceGate.change: g.mu.Lock(), context.WithCancel(d.ctx),
    \* g.setCancel(cancel). (A drain before the lock is caught by the check
    \* after setCancel as well: the model checks once the lock is free.)
    await lock = None;
    if drained /\ Checks then
        if self = "retry" then
            retryGen[svc] := 0;
        elsif self \in Leases then
            out[self] := "drained";
        end if;
        return;
    else
        lock := self;
        cancelReg := self;
        lockedDrained[self] := drained;
    end if;
Change_locked:
    \* change: if g.drained.Load() { return errDrained }; then fn:
    \* configureServiceBackend: editServe (abstracted: instant, it succeeds),
    \* if b.d.isDrained() { return errDrained }; runAdvertise: fetches :=
    \* servicesFetches() (with the confirmer); addAdvertised: GetPrefs, then
    \* EditPrefs unless the Service is listed
    if drained /\ Checks then
        goto Change_fnReturned;
    elsif ~CallOK(self) then
        res[self] := "err";
        goto Change_fnReturned;
    elsif svc \in prefs then
        \* runAdvertise: not added, gate.advertised.Store(true)
        advertised := TRUE;
        goto Change_fnReturned;
    else
        with r = [from |-> self, gen |-> gen[self],
                  val |-> prefs \cup {svc}, kind |-> "add"] do
            pending := pending \cup {r};
            waitOn[self] := r;
        end with;
        if Confirmer then
            mark[self] := fetchCount[gen[self]];
        end if;
        addAfterDrained := addAfterDrained \/ lockedDrained[self];
    end if;
Advertise_sent:
    \* addAdvertised: EditPrefs returns, or fails (refused, cancelled,
    \* tailscaled gone); a request that did not return stays pending
    either
        \* hook: advertise:added (addAdvertised returned, added); fn returns
        \* nil
        await Applied(self);
        waitOn[self] := NoReq;
        res[self] := "added";
        goto Change_fnReturned;
    or
        await ~CallOK(self) \/ refused[self];
        waitOn[self] := NoReq;
        refused[self] := FALSE;
        res[self] := "err";
        goto Change_fnReturned;
    end either;
Change_fnReturned:
    \* change: fn returned; if it failed: if g.drained.Load() { return
    \* errDrained }, which ignoreDrained makes nil; setCancel(nil),
    \* g.mu.Unlock(). serviceBackend.start: retryLater on an error; a retry
    \* polls again, or ends
    \* On the add path first runAdvertise: gate.advertised.Store(true),
    \* markAdvertised (not modelled: nothing here reads fetchMark) and
    \* confirmAdvertisedLater(svc, fetches); Leave's read of advertised
    \* (after the lock) cannot tell it from before the unlock
    if res[self] = "added" then
        advertised := TRUE;
        if Confirmer then
            confReq[svc] := [gen |-> gen[self], after |-> mark[self]];
        end if;
    end if;
    lock := None;
    cancelReg := None;
    cctx[self] := FALSE;
    lockedDrained[self] := FALSE;
    if self \in Leases then
        out[self] := IF res[self] # "err" THEN "ok"
                     ELSE IF drained /\ Checks THEN "drained" ELSE "err";
    elsif res[self] = "err" /\ ~(drained /\ Checks) then
        if self = "sup" then
            retryGen[svc] := gen[self];
        end if;
    elsif self = "retry" then
        retryGen[svc] := 0;
    end if;
    res[self] := "ok";
    mark[self] := 0;
    return;
end procedure;

\* DaemonSupervisor.Drain: markDrained, then daemon.Drain on the running
\* daemon. Its callers only log its error.
procedure DrainProc()
begin
Drain_begin:
    \* markDrained: g.drained.Store(true); g.cancel(), the change in
    \* progress. (A change between the two steps either checks drained
    \* after them or sent its call before the cancel: one step is the same.)
    drained := TRUE;
    if cancelReg # None /\ Cancels then
        cctx[cancelReg] := TRUE;
    end if;
Drain_marked:
    \* s.getDaemon(): nil is "tailscaled is not running"; else daemon.Drain:
    \* markDrained again, g.mu.Lock(), if !g.drain.at.IsZero() { return nil },
    \* fetches, _ := d.servicesFetches(). Abstraction: getDaemon is read
    \* once the lock is free. In Go a daemon replaced while the drainer
    \* waits for the lock is drained as the stale d, which fails (its context
    \* is done) and records nothing; here the drainer drains the new daemon.
    \* The Go outcome is the one the model reaches when getDaemon finds nil
    \* during the restart (drained set, no record, nothing pending), and the
    \* new daemon started drained is judged by S2b either way, so no
    \* property sees the difference.
    if supDaemon = 0 then
        return;
    else
        await lock = None;
        lock := self;
        gen[self] := supDaemon;
        if cancelReg # None /\ Cancels then
            cctx[cancelReg] := TRUE;
        end if;
        if drainRec.set then
            goto Drain_release;
        else
            cnt[self] := fetchCount[supDaemon];
        end if;
    end if;
Drain_countRead:
    \* editAdvertised(d.ctx, keep nothing): GetPrefs, EditPrefs; nothing to
    \* remove is a drain outside tslink (external)
    if ~Live(self) then
        goto Drain_release;
    elsif prefs = {} then
        drainRec := [set |-> TRUE, gen |-> gen[self], fetches |-> 0,
                     external |-> TRUE];
        hiddenAt := hiddenAt \cup Hidden(self);
        goto Drain_release;
    else
        with r = [from |-> self, gen |-> gen[self], val |-> {},
                  kind |-> "drain"] do
            pending := pending \cup {r};
            waitOn[self] := r;
        end with;
    end if;
Drain_sent:
    \* EditPrefs returns: applied, tailscaled gone, or its 10 s timeout
    \* (the request stays pending)
    either
        await Applied(self);
        waitOn[self] := NoReq;
    or
        await ~Live(self);
        waitOn[self] := NoReq;
        goto Drain_release;
    or
        waitOn[self] := NoReq;
        goto Drain_release;
    end either;
Drain_edited:
    \* daemon.Drain: g.drain = drainRecord{daemon: d, fetches: fetches};
    \* g.mu.Unlock() (deferred)
    drainRec := [set |-> TRUE, gen |-> gen[self], fetches |-> cnt[self],
                 external |-> Mutation = "drain-records-external-always"];
    if Mutation = "drain-records-external-always" then
        hiddenAt := hiddenAt \cup Hidden(self);
    end if;
    if lock = self then
        lock := None;
    end if;
    cnt[self] := 0;
    gen[self] := 0;
    return;
Drain_release:
    if lock = self then
        lock := None;
    end if;
    cnt[self] := 0;
    gen[self] := 0;
    return;
end procedure;

\* supervisionLoop: start a daemon, wait for its exit, restart it
fair process Supervisor = "sup"
variables todo = {}, cur = None;
begin
Sup_loop:
    while ~stopped do
        \* startDaemon: newDaemon; daemon.Start: noteSurvivingState;
        \* startProcess: tailscaled runs on the node's state, and control
        \* may fetch its Service list. (An assignment's new value is what
        \* later reads of the same step see.)
        daemonGen := daemonGen + 1;
        gen["sup"] := daemonGen;
        if stateExists then
            advertised := TRUE;
        end if;
        daemonUp := TRUE;
        stateExists := TRUE;
        startedDrained := drained;
        dirty := TRUE;
        todo := Services;
        if ~Prune then
            goto Start_backends;
        end if;
Prune_begin:
        \* configureServing: pruneServices: change: g.mu.Lock(),
        \* setCancel, if g.drained { return errDrained }; fn: editServe
        \* (instant), fetches := servicesFetches(), editAdvertised(ctx,
        \* keep): GetPrefs, EditPrefs unless nothing is to be removed. Then
        \* g.unpruned.Store(err != nil && !errDrained). (The lock and the
        \* send are one step: a drain between them makes the change return
        \* errDrained before the send, as a drain before the lock does.)
        await lock = None;
        if drained then
            unpruned := FALSE;
            goto Start_backends;
        elsif ~CallOK("sup") then
            unpruned := TRUE;
            goto Start_backends;
        elsif Stale \notin prefs then
            unpruned := FALSE;
            goto Start_backends;
        else
            lock := "sup";
            cancelReg := "sup";
            cnt["sup"] := fetchCount[gen["sup"]];
            with r = [from |-> "sup", gen |-> gen["sup"],
                      val |-> prefs \ {Stale}, kind |-> "prune"] do
                pending := pending \cup {r};
                waitOn["sup"] := r;
            end with;
        end if;
Prune_sent:
        \* editAdvertised: EditPrefs returns: applied, failed (cancelled,
        \* tailscaled gone), or its 10 s timeout (the request stays
        \* pending). Unlike a configuration's calls, it times out here: a
        \* NeedsDrain waiting for the lock would wait forever otherwise
        either
            await Applied("sup");
            res["sup"] := "added";
        or
            await ~CallOK("sup");
            res["sup"] := "err";
        or
            res["sup"] := "err";
        end either;
        waitOn["sup"] := NoReq;
Prune_edited:
        \* editAdvertised returned; removed: g.prune = drainRecord{daemon:
        \* d, fetches: fetches}, g.advertised.Store(true); change: if fn
        \* failed and g.drained { errDrained }; setCancel(nil), Unlock;
        \* g.unpruned.Store(err != nil && !errDrained)
        if res["sup"] = "added" then
            pruneOK := TRUE;
            if Mutation # "prune-no-record" then
                pruneRec := [set |-> TRUE, gen |-> gen["sup"],
                             fetches |-> cnt["sup"], external |-> FALSE];
            end if;
            advertised := TRUE;
        end if;
        unpruned := res["sup"] = "err" /\ ~drained;
        lock := None;
        cancelReg := None;
        cctx["sup"] := FALSE;
        res["sup"] := "ok";
        cnt["sup"] := 0;
Start_backends:
        \* configureServing: serviceBackend.start for each backend in turn;
        \* then startDaemon: s.daemon = d
        while todo # {} do
            cur := CHOOSE s \in todo : TRUE;
            todo := todo \ {cur};
            if Lease /\ ~certIssued[cur] then
                \* configureServiceWhenCertified: no certificate, so
                \* configureServiceAfterCert in the background (none once
                \* drained)
                if ~drained then
                    leaseReq[cur] := gen["sup"];
                end if;
            else
                call Configure(cur);
            end if;
        end while;
        supDaemon := gen["sup"];
        \* The endpoint gets its supervisor only if the first start's
        \* daemon is alive: startSupervisor's WaitForIP fails for an exited
        \* daemon and stops the supervisor with no drain
        published := published \/ daemonUp;
        cur := None;
Daemon_started:
        \* waitForExit: tailscaled exited, or the supervisor stopped;
        \* stopDaemon: s.daemon = nil, d.Stop() cancels d.ctx
        await ~daemonUp \/ stopped;
        if ~stopped then
            supDaemon := 0;
            ctxDone[gen["sup"]] := TRUE;
        end if;
    end while;
end process;

\* retryLater: each backend whose start failed is configured again until
\* that succeeds, it is drained or its daemon stops. One process runs the
\* retries of all backends, in any order: a retry's steps after the lock
\* hold it, so concurrent retries are these orders.
fair process Retry = "retry"
begin
Retry_poll:
    while ~stopped do
        \* ctxtime.Poll, every certPollInterval. A poll while tailscaled is
        \* down fails at once and changes nothing: the model polls only
        \* while it runs
        await stopped \/ \E s \in Services : RetryDue(s);
        if ~stopped then
            with s \in {x \in Services : RetryDue(x)} do
                if ctxDone[retryGen[s]] then
                    retryGen[s] := 0;
                else
                    gen["retry"] := retryGen[s];
                    call Configure(s);
                end if;
            end with;
        end if;
    end while;
end process;

\* confirmAdvertisedLater(s, after), started by runAdvertise when it added
\* s; readvertise as two changes with the gap between them
fair process Confirm \in Confs
variables nudges = 0, cafter = 0;
begin
Confirm_idle:
    while ~stopped do
        \* b.d.wg.Go: a new confirmer starts once the one before returned
        \* (two at once need the lease holder's final advertise in a
        \* readvertise's gap; abstraction)
        await confReq[SvcOf(self)].gen # 0;
        gen[self] := confReq[SvcOf(self)].gen;
        cafter := confReq[SvcOf(self)].after;
        nudges := 0;
        confReq[SvcOf(self)] := NoConf;
Confirm_wait:
        \* waitServicesFetch(after, now + advertiseAckTimeout): a fetch, the
        \* daemon's end, or the deadline; then if d.ctx.Err() != nil ||
        \* isDrained() { return }; after advertiseNudges attempts warn and
        \* return; else after, _ = servicesFetches()
        either
            await fetchCount[gen[self]] > cafter \/ ctxDone[gen[self]];
            goto Confirm_idle;
        or
            await fetchCount[gen[self]] <= cafter;
            if ctxDone[gen[self]] \/ drained then
                goto Confirm_idle;
            elsif nudges >= MaxNudges /\ Mutation # "unbounded-nudges" then
                \* warnUnfetched
                goto Confirm_idle;
            else
                \* (bounded for the state space under unbounded-nudges)
                nudges := IF nudges > MaxNudges THEN nudges ELSE nudges + 1;
                cafter := fetchCount[gen[self]];
            end if;
        end either;
Readvertise_begin:
        \* readvertise: change: g.mu.Lock(), setCancel; if g.drained {
        \* return errDrained }; fn: editAdvertised(ctx, keep all but s):
        \* GetPrefs, EditPrefs unless s is not listed. An error ends the
        \* confirmer
        await lock = None;
        if drained /\ Checks then
            goto Confirm_idle;
        elsif ~CallOK(self) then
            goto Confirm_idle;
        elsif SvcOf(self) \notin prefs then
            goto Readvertise_gap;
        else
            lock := self;
            cancelReg := self;
            with r = [from |-> self, gen |-> gen[self],
                      val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] do
                pending := pending \cup {r};
                waitOn[self] := r;
            end with;
        end if;
Readvertise_removed:
        \* editAdvertised returns (hook readvertise:removed: s removed, the
        \* lock still held), or fails (cancelled, tailscaled gone); change:
        \* setCancel(nil), g.mu.Unlock(). An error ends the confirmer. (The
        \* answer and the unlock are one step: only markDrained can run
        \* between them, and it cancels a change whose call returned.)
        either
            await Applied(self);
            waitOn[self] := NoReq;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
        or
            await ~CallOK(self);
            waitOn[self] := NoReq;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
            goto Confirm_idle;
        end either;
Readvertise_gap:
        \* ctxtime.Sleep(d.ctx, advertiseNudgeGap): the daemon's end returns
        \* ctx.Err(); then change: g.mu.Lock(), setCancel, if g.drained {
        \* return errDrained }; fn: fetches (markAdvertised only),
        \* addAdvertised: GetPrefs, EditPrefs unless s is listed
        await lock = None \/ ctxDone[gen[self]];
        if ctxDone[gen[self]] then
            goto Confirm_idle;
        elsif drained /\ Checks then
            goto Confirm_idle;
        elsif ~CallOK(self) then
            goto Confirm_idle;
        elsif SvcOf(self) \in prefs then
            goto Confirm_wait;
        else
            lock := self;
            cancelReg := self;
            lockedDrained[self] := drained;
            with r = [from |-> self, gen |-> gen[self],
                      val |-> prefs \cup {SvcOf(self)}, kind |-> "add"] do
                pending := pending \cup {r};
                waitOn[self] := r;
            end with;
            addAfterDrained := addAfterDrained \/ drained;
        end if;
Readvertise_added:
        \* addAdvertised returns (hook readvertise:added: s added, the lock
        \* still held; markAdvertised(fetches)), or fails (refused,
        \* cancelled, tailscaled gone: the confirmer ends); change:
        \* setCancel(nil), g.mu.Unlock(); the next attempt waits. (One
        \* step, as at Readvertise_removed.)
        either
            await Applied(self);
            waitOn[self] := NoReq;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
            lockedDrained[self] := FALSE;
            goto Confirm_wait;
        or
            await ~CallOK(self) \/ refused[self];
            waitOn[self] := NoReq;
            refused[self] := FALSE;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
            lockedDrained[self] := FALSE;
            goto Confirm_idle;
        end either;
    end while;
end process;

\* configureServiceAfterCert(s): polls configureServiceWithCert until it
\* succeeds, the backend is drained or its daemon stops. This replica holds
\* the lease (one that does not waits for the certificate and configures the
\* Service as a retry would)
fair process LeaseHolder \in Leases
variables early = FALSE;    \* requestCert's advertised
begin
Lease_idle:
    while ~stopped do
        await leaseReq[SvcOf(self)] # 0;
        gen[self] := leaseReq[SvcOf(self)];
        leaseReq[SvcOf(self)] := 0;
Lease_poll:
        \* configureServiceWithCert: if isDrained { errDrained }; with a
        \* certificate configureService; else tryLease, and
        \* configureServiceBackend(false). Poll ends on the daemon's end
        early := FALSE;
        if ctxDone[gen[self]] \/ drained then
            goto Lease_idle;
        elsif certIssued[SvcOf(self)] then
            call Configure(SvcOf(self));
            goto Lease_result;
        end if;
Lease_unadvertise:
        \* configureServiceBackend(false): change: g.mu.Lock(), setCancel,
        \* if drained { errDrained }; fn: editServe (instant), isDrained,
        \* editAdvertised(ctx, keep all but s): GetPrefs, EditPrefs unless s
        \* is not listed
        await lock = None;
        if drained /\ Checks then
            goto Lease_idle;
        elsif ~CallOK(self) then
            goto Lease_poll;
        elsif SvcOf(self) \notin prefs then
            goto Lease_request;
        else
            lock := self;
            cancelReg := self;
            with r = [from |-> self, gen |-> gen[self],
                      val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] do
                pending := pending \cup {r};
                waitOn[self] := r;
            end with;
        end if;
Lease_unadvertiseSent:
        \* EditPrefs returns, or fails (cancelled, tailscaled gone); change
        \* returns: errDrained ends the poll, another error polls again
        either
            await Applied(self);
            waitOn[self] := NoReq;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
        or
            await ~CallOK(self);
            waitOn[self] := NoReq;
            lock := None;
            cancelReg := None;
            cctx[self] := FALSE;
            if drained then
                goto Lease_idle;
            else
                goto Lease_poll;
            end if;
        end either;
Lease_request:
        \* requestCert: until validCert: if isDrained { errDrained };
        \* CertPair; refused for certDomainGrace (and not advertised yet):
        \* advertise(s); ctxtime.Sleep(certPollInterval) ends with the daemon.
        \* With the certificate: advertise(s), the final one
        await certIssued[SvcOf(self)] \/ ~early \/ ctxDone[gen[self]]
              \/ drained;
        if ctxDone[gen[self]] \/ drained then
            goto Lease_idle;
        elsif certIssued[SvcOf(self)] then
            call Configure(SvcOf(self));
            goto Lease_result;
        else
            call Configure(SvcOf(self));
        end if;
Lease_advertisedEarly:
        \* requestCert: advertise returned; an error returns it
        \* (configureServiceWithCert fails: errDrained ends the poll,
        \* another error polls again)
        if out[self] = "ok" then
            early := TRUE;
            out[self] := "ok";
            goto Lease_request;
        elsif out[self] = "drained" then
            out[self] := "ok";
            goto Lease_idle;
        else
            out[self] := "ok";
            goto Lease_poll;
        end if;
Lease_result:
        \* configureServiceAfterCert: nil, errDrained or the daemon's end
        \* ends the poll; another error polls again
        if out[self] = "err" /\ ~ctxDone[gen[self]] then
            out[self] := "ok";
            goto Lease_poll;
        else
            out[self] := "ok";
            early := FALSE;
        end if;
    end while;
end process;

\* The stop signal: Endpoint.DrainService, which reaches the supervisor only
\* once startSupervisor returned (the first daemon was published), and
\* drains if NeedsDrain; or none
fair process StopSignal = "stop"
begin
Stop_signal:
    either
        await published /\ NeedsDrainRead;
        if NeedsDrain then
            call DrainProc();
        end if;
    or
        skip;
    end either;
end process;

\* The endpoint leaves: DrainAndWait if NeedsDrain, then the supervisor's
\* Stop
fair process Leave = "leave"
begin
Leave_begin:
    \* Endpoint.stopTailscale: with the supervisor, DrainAndWait if
    \* NeedsDrain; during the first start (no supervisor yet) RunTailscale
    \* aborts the start, with no drain. Go sets e.supervisor only after
    \* WaitForIP returns, so a Leave in that window aborts with no drain too
    \* (I9's class)
    await ~published \/ NeedsDrainRead;
    if ~published then
        leaveEnd := "aborted";
        goto Leave_done;
    elsif ~NeedsDrain then
        leaveEnd := "nodrain";
        goto Leave_done;
    else
        call DrainProc();
    end if;
Wait_lock:
    \* DrainAndWait: g.mu.Lock(); drain, prune := g.drain, g.prune;
    \* g.mu.Unlock(); a drain that removed nothing waits for the prune
    await lock = None;
    leaveRec := IF drainRec.external /\ pruneRec.set THEN pruneRec
                ELSE drainRec;
Wait_recordRead:
    \* d := s.getDaemon(); no wait if no drain took effect, the drain was
    \* external, there is no daemon, or the backend was never advertised;
    \* a restarted tailscaled counts its fetches from 0
    leaveD := supDaemon;
    if ~leaveRec.set then
        leaveEnd := "norecord";
        goto Leave_done;
    elsif leaveRec.external /\ Mutation # "always-wait" then
        leaveEnd := "external";
        goto Leave_done;
    elsif supDaemon = 0 then
        leaveEnd := "nodaemon";
        goto Leave_done;
    elsif ~advertised /\ Mutation # "always-wait" then
        leaveEnd := "unadvertised";
        goto Leave_done;
    else
        after := IF supDaemon # leaveRec.gen /\ Mutation # "ignore-restart"
                 THEN 0 ELSE leaveRec.fetches;
    end if;
Wait_check:
    \* waitServicesFetch: fetches, next := d.servicesFetches()
    if fetchCount[leaveD] > after then
        leaveEnd := "acked";
        goto Leave_done;
    else
        seen := fetchCount[leaveD];
    end if;
Wait_block:
    \* waitServicesFetch: select on the next fetch, the deadline, d.ctx
    either
        await fetchCount[leaveD] # seen;
        goto Wait_check;
    or
        await ctxDone[leaveD];
        leaveEnd := "ctx";
    or
        await Mutation # "wait-no-timeout" /\ fetchCount[leaveD] = seen;
        leaveEnd := "timeout";
        ackTimedOut := ackCounted[leaveD];
    end either;
Leave_done:
    \* DrainAndWait sleeps out its floor and returns; the endpoint stops the
    \* supervisor: tailscaled stops, and its pending requests with it
    leftAdvertising :=
        daemonUp /\ prefs # {}
        /\ ~(drainApplied /\ leaveEnd \in {"acked", "timeout"});
    stopped := TRUE;
    daemonUp := FALSE;
    supDaemon := 0;
    pending := {};
    ctxDone := [g \in Gens |-> TRUE];
end process;

\* tailscaled applies a pending prefs edit, or refuses one
process Tailscaled = "tailscaled"
begin
Tailscaled_answer:
    while ~stopped do
        either
            with r \in {q \in pending : daemonUp /\ q.gen = daemonGen} do
                if r.val # prefs then
                    dirty := TRUE;
                end if;
                if (prefs \ r.val) \cap Services # {} then
                    unfetched := unfetched \cup {r.from};
                end if;
                if r.kind = "prune" /\ Stale \in prefs then
                    pruneApplied := TRUE;
                    pruneFetched := FALSE;
                end if;
                prefs := r.val;
                pending := pending \ {r};
                if r.kind = "drain" then
                    drainApplied := TRUE;
                end if;
                everInPrefs := everInPrefs \/ r.val # {};
                if r.val # {} then
                    \* Advertised again: control's fetches of the drained
                    \* list are out of date
                    ackMade := FALSE;
                    ackCounted := [g \in Gens |-> FALSE];
                    fetchQ := [g \in Gens |-> [i \in DOMAIN fetchQ[g] |-> FALSE]];
                end if;
            end with;
        or
            \* tailscaled refuses an edit adding a Service: the
            \* configuration fails, and retryLater runs
            with r \in {q \in pending : daemonUp /\ q.gen = daemonGen
                                      /\ q.kind = "add"
                                      /\ waitOn[q.from] = q} do
                refused[r.from] := TRUE;
                pending := pending \ {r};
            end with;
        end either;
    end while;
end process;

\* control fetches the node's Service list (c2n GET /vip-services)
process Control = "control"
begin
Control_fetch:
    while ~stopped do
        await daemonUp /\ dirty;
        dirty := FALSE;
        fetchQ[daemonGen] := Append(fetchQ[daemonGen], Ack);
        ackMade := ackMade \/ Ack;
        pruneFetched := pruneFetched \/ pruneApplied;
        unfetched := {};
    end while;
end process;

\* handleLine counts a fetch from tailscaled's log, after control made it
process Counter = "counter"
begin
Counter_count:
    while ~stopped do
        with g \in {x \in Gens : fetchQ[x] # <<>> /\ Reads(x)} do
            fetchCount[g] := fetchCount[g] + 1;
            ackCounted[g] := ackCounted[g] \/ Head(fetchQ[g]);
            fetchQ[g] := Tail(fetchQ[g]);
            \* hook: fetch:counted (handleLine: d.fetches++, next closed)
        end with;
    end while;
end process;

\* tailscaled exits; its pending requests die with it
process Crash = "crash"
begin
Crash_exit:
    while ~stopped /\ crashes < MaxRestarts do
        await daemonUp;
        daemonUp := FALSE;
        crashes := crashes + 1;
        pending := {r \in pending : r.gen # daemonGen};
        dirty := FALSE;
    end while;
end process;

\* tailscaled issues the certificate the lease holder asks for, or never
process Issuer = "issuer"
begin
Cert_issue:
    while ~stopped do
        with s \in {x \in Services : ~certIssued[x]
                                     /\ pc[LeaseOf(x)] = "Lease_request"} do
            certIssued[s] := TRUE;
        end with;
    end while;
end process;

end algorithm; *)
\* BEGIN TRANSLATION
CONSTANT defaultInitValue
VARIABLES stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending, 
          waitOn, refused, dirty, fetchQ, fetchCount, certIssued, supDaemon, 
          stopped, retryGen, confReq, leaseReq, drained, advertised, lock, 
          cancelReg, cctx, drainRec, pruneRec, unpruned, gen, res, out, mark, 
          cnt, leaveRec, leaveD, after, seen, leaveEnd, everInPrefs, 
          lockedDrained, addAfterDrained, startedDrained, drainApplied, 
          ackMade, ackCounted, ackTimedOut, published, leftAdvertising, 
          pruneApplied, pruneFetched, pruneOK, unfetched, hiddenAt, pc, stack

(* define statement *)
Live(a) == daemonUp /\ daemonGen = gen[a] /\ ~ctxDone[gen[a]]

CallOK(a) == Live(a) /\ ~cctx[a]

Applied(a) == waitOn[a] \notin pending /\ ~refused[a] /\ Live(a)

RetryDue(s) ==
    /\ retryGen[s] # 0
    /\ \/ ctxDone[retryGen[s]]
       \/ retryGen[s] = daemonGen /\ daemonUp
Checks == Mutation # "no-drained-check"
Cancels == Mutation # "no-cancel"
Ack == drainApplied /\ prefs = {}



NeedsDrain == Services # {} \/ unpruned \/ pruneRec.set
NeedsDrainRead == Services # {} \/ unpruned \/ lock = None



Reads(g) ==
    \/ pc["leave"] \in {"Wait_check", "Wait_block"} /\ leaveD = g
    \/ \E p \in Drainers : pc[p] = "Drain_marked" /\ supDaemon = g

    \/ Confirmer /\ \E p \in Changers \cup Leases :
           pc[p] = "Change_locked" /\ gen[p] = g

    \/ \E c \in Confs : pc[c] = "Confirm_wait" /\ gen[c] = g

    \/ pc["sup"] = "Prune_begin" /\ gen["sup"] = g



DrainedStaysDrained == drainRec.set => prefs = {}

DrainedFlagHoldsBack == ~addAfterDrained

NoDaemonAdvertisesDrained == daemonUp /\ startedDrained => prefs = {}







LeaveReturnedEarly(how) ==
    pc["leave"] = "Done" /\ leaveEnd \in how /\ drainApplied => ackMade
LeaveWaitsForDrain ==
    LeaveReturnedEarly({"acked", "ctx", "norecord", "external",
                        "nodaemon", "unadvertised"})

S3Acked == LeaveReturnedEarly({"acked"})
S3Ctx == LeaveReturnedEarly({"ctx"})
S3NoRecord == LeaveReturnedEarly({"norecord"})
S3External == LeaveReturnedEarly({"external"})
S3NoDaemon == LeaveReturnedEarly({"nodaemon"})
S3Unadvertised == LeaveReturnedEarly({"unadvertised"})



NoLeaveWhileAdvertising == ~leftAdvertising


S8Drained == leftAdvertising => leaveEnd = "aborted"

NoWaitForNothing ==
    pc["leave"] \in {"Wait_check", "Wait_block"} => everInPrefs




PrunedWaitedFor ==
    pc["leave"] = "Done" /\ pruneApplied
    /\ leaveEnd \in {"acked", "external", "unadvertised", "nodrain"}
    => pruneFetched


S5Recorded ==
    pc["leave"] = "Done" /\ pruneOK
    /\ leaveEnd \in {"external", "unadvertised", "nodrain"}
    => pruneFetched

S5Unrecorded ==
    pc["leave"] = "Done" /\ pruneApplied /\ ~pruneOK
    /\ leaveEnd \in {"external", "unadvertised", "nodrain"}
    => pruneFetched

S5Acked ==
    pc["leave"] = "Done" /\ pruneApplied /\ leaveEnd = "acked"
    /\ drainRec.external => pruneFetched




NoHiddenDrain == hiddenAt = {}


Hidden(p) == {<<a, pc[a]>> : a \in unfetched \ (Drainers \ {p})}


S6Gap == \A h \in hiddenAt : h[2] # "Readvertise_gap"
S6Lease == \A h \in hiddenAt : h[1] \notin Leases

AckEndsWait == ~ackTimedOut

LeaveTerminates == <>(pc["leave"] = "Done")

DrainGetsLock ==
    \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"

VARIABLES svc, todo, cur, nudges, cafter, early

vars == << stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending, 
           waitOn, refused, dirty, fetchQ, fetchCount, certIssued, supDaemon, 
           stopped, retryGen, confReq, leaseReq, drained, advertised, lock, 
           cancelReg, cctx, drainRec, pruneRec, unpruned, gen, res, out, mark, 
           cnt, leaveRec, leaveD, after, seen, leaveEnd, everInPrefs, 
           lockedDrained, addAfterDrained, startedDrained, drainApplied, 
           ackMade, ackCounted, ackTimedOut, published, leftAdvertising, 
           pruneApplied, pruneFetched, pruneOK, unfetched, hiddenAt, pc, 
           stack, svc, todo, cur, nudges, cafter, early >>

ProcSet == {"sup"} \cup {"retry"} \cup (Confs) \cup (Leases) \cup {"stop"} \cup {"leave"} \cup {"tailscaled"} \cup {"control"} \cup {"counter"} \cup {"crash"} \cup {"issuer"}

Init == (* Global variables *)
        /\ stateExists \in BOOLEAN
        /\ prefs \in IF stateExists
                     THEN {{}, Services} \cup
                          (IF Prune THEN {{Stale}, Services \cup {Stale}} ELSE {})
                     ELSE {{}}
        /\ daemonGen = 0
        /\ daemonUp = FALSE
        /\ ctxDone = [g \in Gens |-> FALSE]
        /\ crashes = 0
        /\ pending = {}
        /\ waitOn = [a \in Actors |-> NoReq]
        /\ refused = [a \in Actors |-> FALSE]
        /\ dirty = FALSE
        /\ fetchQ = [g \in Gens |-> <<>>]
        /\ fetchCount = [g \in Gens |-> 0]
        /\ certIssued = [s \in Services |-> FALSE]
        /\ supDaemon = 0
        /\ stopped = FALSE
        /\ retryGen = [s \in Services |-> 0]
        /\ confReq = [s \in Services |-> NoConf]
        /\ leaseReq = [s \in Services |-> 0]
        /\ drained = FALSE
        /\ advertised = FALSE
        /\ lock = None
        /\ cancelReg = None
        /\ cctx = [a \in Actors |-> FALSE]
        /\ drainRec = NoRec
        /\ pruneRec = NoRec
        /\ unpruned = FALSE
        /\ gen = [a \in Actors |-> 0]
        /\ res = [a \in Actors |-> "ok"]
        /\ out = [a \in Actors |-> "ok"]
        /\ mark = [a \in Actors |-> 0]
        /\ cnt = [a \in Actors |-> 0]
        /\ leaveRec = NoRec
        /\ leaveD = 0
        /\ after = 0
        /\ seen = 0
        /\ leaveEnd = None
        /\ everInPrefs = (prefs # {})
        /\ lockedDrained = [a \in Actors |-> FALSE]
        /\ addAfterDrained = FALSE
        /\ startedDrained = FALSE
        /\ drainApplied = FALSE
        /\ ackMade = FALSE
        /\ ackCounted = [g \in Gens |-> FALSE]
        /\ ackTimedOut = FALSE
        /\ published = FALSE
        /\ leftAdvertising = FALSE
        /\ pruneApplied = FALSE
        /\ pruneFetched = FALSE
        /\ pruneOK = FALSE
        /\ unfetched = {}
        /\ hiddenAt = {}
        (* Procedure Configure *)
        /\ svc = [ self \in ProcSet |-> defaultInitValue]
        (* Process Supervisor *)
        /\ todo = {}
        /\ cur = None
        (* Process Confirm *)
        /\ nudges = [self \in Confs |-> 0]
        /\ cafter = [self \in Confs |-> 0]
        (* Process LeaseHolder *)
        /\ early = [self \in Leases |-> FALSE]
        /\ stack = [self \in ProcSet |-> << >>]
        /\ pc = [self \in ProcSet |-> CASE self = "sup" -> "Sup_loop"
                                        [] self = "retry" -> "Retry_poll"
                                        [] self \in Confs -> "Confirm_idle"
                                        [] self \in Leases -> "Lease_idle"
                                        [] self = "stop" -> "Stop_signal"
                                        [] self = "leave" -> "Leave_begin"
                                        [] self = "tailscaled" -> "Tailscaled_answer"
                                        [] self = "control" -> "Control_fetch"
                                        [] self = "counter" -> "Counter_count"
                                        [] self = "crash" -> "Crash_exit"
                                        [] self = "issuer" -> "Cert_issue"]

Change_lock(self) == /\ pc[self] = "Change_lock"
                     /\ lock = None
                     /\ IF drained /\ Checks
                           THEN /\ IF self = "retry"
                                      THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = 0]
                                           /\ out' = out
                                      ELSE /\ IF self \in Leases
                                                 THEN /\ out' = [out EXCEPT ![self] = "drained"]
                                                 ELSE /\ TRUE
                                                      /\ out' = out
                                           /\ UNCHANGED retryGen
                                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                /\ svc' = [svc EXCEPT ![self] = Head(stack[self]).svc]
                                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                /\ UNCHANGED << lock, cancelReg, lockedDrained >>
                           ELSE /\ lock' = self
                                /\ cancelReg' = self
                                /\ lockedDrained' = [lockedDrained EXCEPT ![self] = drained]
                                /\ pc' = [pc EXCEPT ![self] = "Change_locked"]
                                /\ UNCHANGED << retryGen, out, stack, svc >>
                     /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                     ctxDone, crashes, pending, waitOn, 
                                     refused, dirty, fetchQ, fetchCount, 
                                     certIssued, supDaemon, stopped, confReq, 
                                     leaseReq, drained, advertised, cctx, 
                                     drainRec, pruneRec, unpruned, gen, res, 
                                     mark, cnt, leaveRec, leaveD, after, seen, 
                                     leaveEnd, everInPrefs, addAfterDrained, 
                                     startedDrained, drainApplied, ackMade, 
                                     ackCounted, ackTimedOut, published, 
                                     leftAdvertising, pruneApplied, 
                                     pruneFetched, pruneOK, unfetched, 
                                     hiddenAt, todo, cur, nudges, cafter, 
                                     early >>

Change_locked(self) == /\ pc[self] = "Change_locked"
                       /\ IF drained /\ Checks
                             THEN /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                  /\ UNCHANGED << pending, waitOn, advertised, 
                                                  res, mark, addAfterDrained >>
                             ELSE /\ IF ~CallOK(self)
                                        THEN /\ res' = [res EXCEPT ![self] = "err"]
                                             /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                             /\ UNCHANGED << pending, waitOn, 
                                                             advertised, mark, 
                                                             addAfterDrained >>
                                        ELSE /\ IF svc[self] \in prefs
                                                   THEN /\ advertised' = TRUE
                                                        /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                                        /\ UNCHANGED << pending, 
                                                                        waitOn, 
                                                                        mark, 
                                                                        addAfterDrained >>
                                                   ELSE /\ LET r == [from |-> self, gen |-> gen[self],
                                                                     val |-> prefs \cup {svc[self]}, kind |-> "add"] IN
                                                             /\ pending' = (pending \cup {r})
                                                             /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                        /\ IF Confirmer
                                                              THEN /\ mark' = [mark EXCEPT ![self] = fetchCount[gen[self]]]
                                                              ELSE /\ TRUE
                                                                   /\ mark' = mark
                                                        /\ addAfterDrained' = (addAfterDrained \/ lockedDrained[self])
                                                        /\ pc' = [pc EXCEPT ![self] = "Advertise_sent"]
                                                        /\ UNCHANGED advertised
                                             /\ res' = res
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, refused, dirty, 
                                       fetchQ, fetchCount, certIssued, 
                                       supDaemon, stopped, retryGen, confReq, 
                                       leaseReq, drained, lock, cancelReg, 
                                       cctx, drainRec, pruneRec, unpruned, gen, 
                                       out, cnt, leaveRec, leaveD, after, seen, 
                                       leaveEnd, everInPrefs, lockedDrained, 
                                       startedDrained, drainApplied, ackMade, 
                                       ackCounted, ackTimedOut, published, 
                                       leftAdvertising, pruneApplied, 
                                       pruneFetched, pruneOK, unfetched, 
                                       hiddenAt, stack, svc, todo, cur, nudges, 
                                       cafter, early >>

Advertise_sent(self) == /\ pc[self] = "Advertise_sent"
                        /\ \/ /\ Applied(self)
                              /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                              /\ res' = [res EXCEPT ![self] = "added"]
                              /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                              /\ UNCHANGED refused
                           \/ /\ ~CallOK(self) \/ refused[self]
                              /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                              /\ refused' = [refused EXCEPT ![self] = FALSE]
                              /\ res' = [res EXCEPT ![self] = "err"]
                              /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                        /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                        daemonUp, ctxDone, crashes, pending, 
                                        dirty, fetchQ, fetchCount, certIssued, 
                                        supDaemon, stopped, retryGen, confReq, 
                                        leaseReq, drained, advertised, lock, 
                                        cancelReg, cctx, drainRec, pruneRec, 
                                        unpruned, gen, out, mark, cnt, 
                                        leaveRec, leaveD, after, seen, 
                                        leaveEnd, everInPrefs, lockedDrained, 
                                        addAfterDrained, startedDrained, 
                                        drainApplied, ackMade, ackCounted, 
                                        ackTimedOut, published, 
                                        leftAdvertising, pruneApplied, 
                                        pruneFetched, pruneOK, unfetched, 
                                        hiddenAt, stack, svc, todo, cur, 
                                        nudges, cafter, early >>

Change_fnReturned(self) == /\ pc[self] = "Change_fnReturned"
                           /\ IF res[self] = "added"
                                 THEN /\ advertised' = TRUE
                                      /\ IF Confirmer
                                            THEN /\ confReq' = [confReq EXCEPT ![svc[self]] = [gen |-> gen[self], after |-> mark[self]]]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED confReq
                                 ELSE /\ TRUE
                                      /\ UNCHANGED << confReq, advertised >>
                           /\ lock' = None
                           /\ cancelReg' = None
                           /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                           /\ lockedDrained' = [lockedDrained EXCEPT ![self] = FALSE]
                           /\ IF self \in Leases
                                 THEN /\ out' = [out EXCEPT ![self] = IF res[self] # "err" THEN "ok"
                                                                      ELSE IF drained /\ Checks THEN "drained" ELSE "err"]
                                      /\ UNCHANGED retryGen
                                 ELSE /\ IF res[self] = "err" /\ ~(drained /\ Checks)
                                            THEN /\ IF self = "sup"
                                                       THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = gen[self]]
                                                       ELSE /\ TRUE
                                                            /\ UNCHANGED retryGen
                                            ELSE /\ IF self = "retry"
                                                       THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = 0]
                                                       ELSE /\ TRUE
                                                            /\ UNCHANGED retryGen
                                      /\ out' = out
                           /\ res' = [res EXCEPT ![self] = "ok"]
                           /\ mark' = [mark EXCEPT ![self] = 0]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ svc' = [svc EXCEPT ![self] = Head(stack[self]).svc]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, pending, 
                                           waitOn, refused, dirty, fetchQ, 
                                           fetchCount, certIssued, supDaemon, 
                                           stopped, leaseReq, drained, 
                                           drainRec, pruneRec, unpruned, gen, 
                                           cnt, leaveRec, leaveD, after, seen, 
                                           leaveEnd, everInPrefs, 
                                           addAfterDrained, startedDrained, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, pruneApplied, 
                                           pruneFetched, pruneOK, unfetched, 
                                           hiddenAt, todo, cur, nudges, cafter, 
                                           early >>

Configure(self) == Change_lock(self) \/ Change_locked(self)
                      \/ Advertise_sent(self) \/ Change_fnReturned(self)

Drain_begin(self) == /\ pc[self] = "Drain_begin"
                     /\ drained' = TRUE
                     /\ IF cancelReg # None /\ Cancels
                           THEN /\ cctx' = [cctx EXCEPT ![cancelReg] = TRUE]
                           ELSE /\ TRUE
                                /\ cctx' = cctx
                     /\ pc' = [pc EXCEPT ![self] = "Drain_marked"]
                     /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                     ctxDone, crashes, pending, waitOn, 
                                     refused, dirty, fetchQ, fetchCount, 
                                     certIssued, supDaemon, stopped, retryGen, 
                                     confReq, leaseReq, advertised, lock, 
                                     cancelReg, drainRec, pruneRec, unpruned, 
                                     gen, res, out, mark, cnt, leaveRec, 
                                     leaveD, after, seen, leaveEnd, 
                                     everInPrefs, lockedDrained, 
                                     addAfterDrained, startedDrained, 
                                     drainApplied, ackMade, ackCounted, 
                                     ackTimedOut, published, leftAdvertising, 
                                     pruneApplied, pruneFetched, pruneOK, 
                                     unfetched, hiddenAt, stack, svc, todo, 
                                     cur, nudges, cafter, early >>

Drain_marked(self) == /\ pc[self] = "Drain_marked"
                      /\ IF supDaemon = 0
                            THEN /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                 /\ UNCHANGED << lock, cctx, gen, cnt >>
                            ELSE /\ lock = None
                                 /\ lock' = self
                                 /\ gen' = [gen EXCEPT ![self] = supDaemon]
                                 /\ IF cancelReg # None /\ Cancels
                                       THEN /\ cctx' = [cctx EXCEPT ![cancelReg] = TRUE]
                                       ELSE /\ TRUE
                                            /\ cctx' = cctx
                                 /\ IF drainRec.set
                                       THEN /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                            /\ cnt' = cnt
                                       ELSE /\ cnt' = [cnt EXCEPT ![self] = fetchCount[supDaemon]]
                                            /\ pc' = [pc EXCEPT ![self] = "Drain_countRead"]
                                 /\ stack' = stack
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, fetchQ, fetchCount, 
                                      certIssued, supDaemon, stopped, retryGen, 
                                      confReq, leaseReq, drained, advertised, 
                                      cancelReg, drainRec, pruneRec, unpruned, 
                                      res, out, mark, leaveRec, leaveD, after, 
                                      seen, leaveEnd, everInPrefs, 
                                      lockedDrained, addAfterDrained, 
                                      startedDrained, drainApplied, ackMade, 
                                      ackCounted, ackTimedOut, published, 
                                      leftAdvertising, pruneApplied, 
                                      pruneFetched, pruneOK, unfetched, 
                                      hiddenAt, svc, todo, cur, nudges, cafter, 
                                      early >>

Drain_countRead(self) == /\ pc[self] = "Drain_countRead"
                         /\ IF ~Live(self)
                               THEN /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                    /\ UNCHANGED << pending, waitOn, drainRec, 
                                                    hiddenAt >>
                               ELSE /\ IF prefs = {}
                                          THEN /\ drainRec' = [set |-> TRUE, gen |-> gen[self], fetches |-> 0,
                                                               external |-> TRUE]
                                               /\ hiddenAt' = (hiddenAt \cup Hidden(self))
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                               /\ UNCHANGED << pending, waitOn >>
                                          ELSE /\ LET r == [from |-> self, gen |-> gen[self], val |-> {},
                                                            kind |-> "drain"] IN
                                                    /\ pending' = (pending \cup {r})
                                                    /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_sent"]
                                               /\ UNCHANGED << drainRec, 
                                                               hiddenAt >>
                         /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                         daemonUp, ctxDone, crashes, refused, 
                                         dirty, fetchQ, fetchCount, certIssued, 
                                         supDaemon, stopped, retryGen, confReq, 
                                         leaseReq, drained, advertised, lock, 
                                         cancelReg, cctx, pruneRec, unpruned, 
                                         gen, res, out, mark, cnt, leaveRec, 
                                         leaveD, after, seen, leaveEnd, 
                                         everInPrefs, lockedDrained, 
                                         addAfterDrained, startedDrained, 
                                         drainApplied, ackMade, ackCounted, 
                                         ackTimedOut, published, 
                                         leftAdvertising, pruneApplied, 
                                         pruneFetched, pruneOK, unfetched, 
                                         stack, svc, todo, cur, nudges, cafter, 
                                         early >>

Drain_sent(self) == /\ pc[self] = "Drain_sent"
                    /\ \/ /\ Applied(self)
                          /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                          /\ pc' = [pc EXCEPT ![self] = "Drain_edited"]
                       \/ /\ ~Live(self)
                          /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                          /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                       \/ /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                          /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                    /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                    ctxDone, crashes, pending, refused, dirty, 
                                    fetchQ, fetchCount, certIssued, supDaemon, 
                                    stopped, retryGen, confReq, leaseReq, 
                                    drained, advertised, lock, cancelReg, cctx, 
                                    drainRec, pruneRec, unpruned, gen, res, 
                                    out, mark, cnt, leaveRec, leaveD, after, 
                                    seen, leaveEnd, everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    drainApplied, ackMade, ackCounted, 
                                    ackTimedOut, published, leftAdvertising, 
                                    pruneApplied, pruneFetched, pruneOK, 
                                    unfetched, hiddenAt, stack, svc, todo, cur, 
                                    nudges, cafter, early >>

Drain_edited(self) == /\ pc[self] = "Drain_edited"
                      /\ drainRec' = [set |-> TRUE, gen |-> gen[self], fetches |-> cnt[self],
                                      external |-> Mutation = "drain-records-external-always"]
                      /\ IF Mutation = "drain-records-external-always"
                            THEN /\ hiddenAt' = (hiddenAt \cup Hidden(self))
                            ELSE /\ TRUE
                                 /\ UNCHANGED hiddenAt
                      /\ IF lock = self
                            THEN /\ lock' = None
                            ELSE /\ TRUE
                                 /\ lock' = lock
                      /\ cnt' = [cnt EXCEPT ![self] = 0]
                      /\ gen' = [gen EXCEPT ![self] = 0]
                      /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                      /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, fetchQ, fetchCount, 
                                      certIssued, supDaemon, stopped, retryGen, 
                                      confReq, leaseReq, drained, advertised, 
                                      cancelReg, cctx, pruneRec, unpruned, res, 
                                      out, mark, leaveRec, leaveD, after, seen, 
                                      leaveEnd, everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      pruneApplied, pruneFetched, pruneOK, 
                                      unfetched, svc, todo, cur, nudges, 
                                      cafter, early >>

Drain_release(self) == /\ pc[self] = "Drain_release"
                       /\ IF lock = self
                             THEN /\ lock' = None
                             ELSE /\ TRUE
                                  /\ lock' = lock
                       /\ cnt' = [cnt EXCEPT ![self] = 0]
                       /\ gen' = [gen EXCEPT ![self] = 0]
                       /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                       /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, pending, waitOn, 
                                       refused, dirty, fetchQ, fetchCount, 
                                       certIssued, supDaemon, stopped, 
                                       retryGen, confReq, leaseReq, drained, 
                                       advertised, cancelReg, cctx, drainRec, 
                                       pruneRec, unpruned, res, out, mark, 
                                       leaveRec, leaveD, after, seen, leaveEnd, 
                                       everInPrefs, lockedDrained, 
                                       addAfterDrained, startedDrained, 
                                       drainApplied, ackMade, ackCounted, 
                                       ackTimedOut, published, leftAdvertising, 
                                       pruneApplied, pruneFetched, pruneOK, 
                                       unfetched, hiddenAt, svc, todo, cur, 
                                       nudges, cafter, early >>

DrainProc(self) == Drain_begin(self) \/ Drain_marked(self)
                      \/ Drain_countRead(self) \/ Drain_sent(self)
                      \/ Drain_edited(self) \/ Drain_release(self)

Sup_loop == /\ pc["sup"] = "Sup_loop"
            /\ IF ~stopped
                  THEN /\ daemonGen' = daemonGen + 1
                       /\ gen' = [gen EXCEPT !["sup"] = daemonGen']
                       /\ IF stateExists
                             THEN /\ advertised' = TRUE
                             ELSE /\ TRUE
                                  /\ UNCHANGED advertised
                       /\ daemonUp' = TRUE
                       /\ stateExists' = TRUE
                       /\ startedDrained' = drained
                       /\ dirty' = TRUE
                       /\ todo' = Services
                       /\ IF ~Prune
                             THEN /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                             ELSE /\ pc' = [pc EXCEPT !["sup"] = "Prune_begin"]
                  ELSE /\ pc' = [pc EXCEPT !["sup"] = "Done"]
                       /\ UNCHANGED << stateExists, daemonGen, daemonUp, dirty, 
                                       advertised, gen, startedDrained, todo >>
            /\ UNCHANGED << prefs, ctxDone, crashes, pending, waitOn, refused, 
                            fetchQ, fetchCount, certIssued, supDaemon, stopped, 
                            retryGen, confReq, leaseReq, drained, lock, 
                            cancelReg, cctx, drainRec, pruneRec, unpruned, res, 
                            out, mark, cnt, leaveRec, leaveD, after, seen, 
                            leaveEnd, everInPrefs, lockedDrained, 
                            addAfterDrained, drainApplied, ackMade, ackCounted, 
                            ackTimedOut, published, leftAdvertising, 
                            pruneApplied, pruneFetched, pruneOK, unfetched, 
                            hiddenAt, stack, svc, cur, nudges, cafter, early >>

Prune_begin == /\ pc["sup"] = "Prune_begin"
               /\ lock = None
               /\ IF drained
                     THEN /\ unpruned' = FALSE
                          /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                          /\ UNCHANGED << pending, waitOn, lock, cancelReg, 
                                          cnt >>
                     ELSE /\ IF ~CallOK("sup")
                                THEN /\ unpruned' = TRUE
                                     /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                                     /\ UNCHANGED << pending, waitOn, lock, 
                                                     cancelReg, cnt >>
                                ELSE /\ IF Stale \notin prefs
                                           THEN /\ unpruned' = FALSE
                                                /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                                                /\ UNCHANGED << pending, 
                                                                waitOn, lock, 
                                                                cancelReg, cnt >>
                                           ELSE /\ lock' = "sup"
                                                /\ cancelReg' = "sup"
                                                /\ cnt' = [cnt EXCEPT !["sup"] = fetchCount[gen["sup"]]]
                                                /\ LET r == [from |-> "sup", gen |-> gen["sup"],
                                                             val |-> prefs \ {Stale}, kind |-> "prune"] IN
                                                     /\ pending' = (pending \cup {r})
                                                     /\ waitOn' = [waitOn EXCEPT !["sup"] = r]
                                                /\ pc' = [pc EXCEPT !["sup"] = "Prune_sent"]
                                                /\ UNCHANGED unpruned
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, refused, dirty, fetchQ, 
                               fetchCount, certIssued, supDaemon, stopped, 
                               retryGen, confReq, leaseReq, drained, 
                               advertised, cctx, drainRec, pruneRec, gen, res, 
                               out, mark, leaveRec, leaveD, after, seen, 
                               leaveEnd, everInPrefs, lockedDrained, 
                               addAfterDrained, startedDrained, drainApplied, 
                               ackMade, ackCounted, ackTimedOut, published, 
                               leftAdvertising, pruneApplied, pruneFetched, 
                               pruneOK, unfetched, hiddenAt, stack, svc, todo, 
                               cur, nudges, cafter, early >>

Prune_sent == /\ pc["sup"] = "Prune_sent"
              /\ \/ /\ Applied("sup")
                    /\ res' = [res EXCEPT !["sup"] = "added"]
                 \/ /\ ~CallOK("sup")
                    /\ res' = [res EXCEPT !["sup"] = "err"]
                 \/ /\ res' = [res EXCEPT !["sup"] = "err"]
              /\ waitOn' = [waitOn EXCEPT !["sup"] = NoReq]
              /\ pc' = [pc EXCEPT !["sup"] = "Prune_edited"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, refused, dirty, fetchQ, 
                              fetchCount, certIssued, supDaemon, stopped, 
                              retryGen, confReq, leaseReq, drained, advertised, 
                              lock, cancelReg, cctx, drainRec, pruneRec, 
                              unpruned, gen, out, mark, cnt, leaveRec, leaveD, 
                              after, seen, leaveEnd, everInPrefs, 
                              lockedDrained, addAfterDrained, startedDrained, 
                              drainApplied, ackMade, ackCounted, ackTimedOut, 
                              published, leftAdvertising, pruneApplied, 
                              pruneFetched, pruneOK, unfetched, hiddenAt, 
                              stack, svc, todo, cur, nudges, cafter, early >>

Prune_edited == /\ pc["sup"] = "Prune_edited"
                /\ IF res["sup"] = "added"
                      THEN /\ pruneOK' = TRUE
                           /\ IF Mutation # "prune-no-record"
                                 THEN /\ pruneRec' = [set |-> TRUE, gen |-> gen["sup"],
                                                      fetches |-> cnt["sup"], external |-> FALSE]
                                 ELSE /\ TRUE
                                      /\ UNCHANGED pruneRec
                           /\ advertised' = TRUE
                      ELSE /\ TRUE
                           /\ UNCHANGED << advertised, pruneRec, pruneOK >>
                /\ unpruned' = (res["sup"] = "err" /\ ~drained)
                /\ lock' = None
                /\ cancelReg' = None
                /\ cctx' = [cctx EXCEPT !["sup"] = FALSE]
                /\ res' = [res EXCEPT !["sup"] = "ok"]
                /\ cnt' = [cnt EXCEPT !["sup"] = 0]
                /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, pending, waitOn, refused, 
                                dirty, fetchQ, fetchCount, certIssued, 
                                supDaemon, stopped, retryGen, confReq, 
                                leaseReq, drained, drainRec, gen, out, mark, 
                                leaveRec, leaveD, after, seen, leaveEnd, 
                                everInPrefs, lockedDrained, addAfterDrained, 
                                startedDrained, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, pruneApplied, pruneFetched, 
                                unfetched, hiddenAt, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Start_backends == /\ pc["sup"] = "Start_backends"
                  /\ IF todo # {}
                        THEN /\ cur' = (CHOOSE s \in todo : TRUE)
                             /\ todo' = todo \ {cur'}
                             /\ IF Lease /\ ~certIssued[cur']
                                   THEN /\ IF ~drained
                                              THEN /\ leaseReq' = [leaseReq EXCEPT ![cur'] = gen["sup"]]
                                              ELSE /\ TRUE
                                                   /\ UNCHANGED leaseReq
                                        /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                                        /\ UNCHANGED << stack, svc >>
                                   ELSE /\ /\ stack' = [stack EXCEPT !["sup"] = << [ procedure |->  "Configure",
                                                                                     pc        |->  "Start_backends",
                                                                                     svc       |->  svc["sup"] ] >>
                                                                                 \o stack["sup"]]
                                           /\ svc' = [svc EXCEPT !["sup"] = cur']
                                        /\ pc' = [pc EXCEPT !["sup"] = "Change_lock"]
                                        /\ UNCHANGED leaseReq
                             /\ UNCHANGED << supDaemon, published >>
                        ELSE /\ supDaemon' = gen["sup"]
                             /\ published' = (published \/ daemonUp)
                             /\ cur' = None
                             /\ pc' = [pc EXCEPT !["sup"] = "Daemon_started"]
                             /\ UNCHANGED << leaseReq, stack, svc, todo >>
                  /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                  ctxDone, crashes, pending, waitOn, refused, 
                                  dirty, fetchQ, fetchCount, certIssued, 
                                  stopped, retryGen, confReq, drained, 
                                  advertised, lock, cancelReg, cctx, drainRec, 
                                  pruneRec, unpruned, gen, res, out, mark, cnt, 
                                  leaveRec, leaveD, after, seen, leaveEnd, 
                                  everInPrefs, lockedDrained, addAfterDrained, 
                                  startedDrained, drainApplied, ackMade, 
                                  ackCounted, ackTimedOut, leftAdvertising, 
                                  pruneApplied, pruneFetched, pruneOK, 
                                  unfetched, hiddenAt, nudges, cafter, early >>

Daemon_started == /\ pc["sup"] = "Daemon_started"
                  /\ ~daemonUp \/ stopped
                  /\ IF ~stopped
                        THEN /\ supDaemon' = 0
                             /\ ctxDone' = [ctxDone EXCEPT ![gen["sup"]] = TRUE]
                        ELSE /\ TRUE
                             /\ UNCHANGED << ctxDone, supDaemon >>
                  /\ pc' = [pc EXCEPT !["sup"] = "Sup_loop"]
                  /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                  crashes, pending, waitOn, refused, dirty, 
                                  fetchQ, fetchCount, certIssued, stopped, 
                                  retryGen, confReq, leaseReq, drained, 
                                  advertised, lock, cancelReg, cctx, drainRec, 
                                  pruneRec, unpruned, gen, res, out, mark, cnt, 
                                  leaveRec, leaveD, after, seen, leaveEnd, 
                                  everInPrefs, lockedDrained, addAfterDrained, 
                                  startedDrained, drainApplied, ackMade, 
                                  ackCounted, ackTimedOut, published, 
                                  leftAdvertising, pruneApplied, pruneFetched, 
                                  pruneOK, unfetched, hiddenAt, stack, svc, 
                                  todo, cur, nudges, cafter, early >>

Supervisor == Sup_loop \/ Prune_begin \/ Prune_sent \/ Prune_edited
                 \/ Start_backends \/ Daemon_started

Retry_poll == /\ pc["retry"] = "Retry_poll"
              /\ IF ~stopped
                    THEN /\ stopped \/ \E s \in Services : RetryDue(s)
                         /\ IF ~stopped
                               THEN /\ \E s \in {x \in Services : RetryDue(x)}:
                                         IF ctxDone[retryGen[s]]
                                            THEN /\ retryGen' = [retryGen EXCEPT ![s] = 0]
                                                 /\ pc' = [pc EXCEPT !["retry"] = "Retry_poll"]
                                                 /\ UNCHANGED << gen, stack, 
                                                                 svc >>
                                            ELSE /\ gen' = [gen EXCEPT !["retry"] = retryGen[s]]
                                                 /\ /\ stack' = [stack EXCEPT !["retry"] = << [ procedure |->  "Configure",
                                                                                                pc        |->  "Retry_poll",
                                                                                                svc       |->  svc["retry"] ] >>
                                                                                            \o stack["retry"]]
                                                    /\ svc' = [svc EXCEPT !["retry"] = s]
                                                 /\ pc' = [pc EXCEPT !["retry"] = "Change_lock"]
                                                 /\ UNCHANGED retryGen
                               ELSE /\ pc' = [pc EXCEPT !["retry"] = "Retry_poll"]
                                    /\ UNCHANGED << retryGen, gen, stack, svc >>
                    ELSE /\ pc' = [pc EXCEPT !["retry"] = "Done"]
                         /\ UNCHANGED << retryGen, gen, stack, svc >>
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, fetchQ, 
                              fetchCount, certIssued, supDaemon, stopped, 
                              confReq, leaseReq, drained, advertised, lock, 
                              cancelReg, cctx, drainRec, pruneRec, unpruned, 
                              res, out, mark, cnt, leaveRec, leaveD, after, 
                              seen, leaveEnd, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              leftAdvertising, pruneApplied, pruneFetched, 
                              pruneOK, unfetched, hiddenAt, todo, cur, nudges, 
                              cafter, early >>

Retry == Retry_poll

Confirm_idle(self) == /\ pc[self] = "Confirm_idle"
                      /\ IF ~stopped
                            THEN /\ confReq[SvcOf(self)].gen # 0
                                 /\ gen' = [gen EXCEPT ![self] = confReq[SvcOf(self)].gen]
                                 /\ cafter' = [cafter EXCEPT ![self] = confReq[SvcOf(self)].after]
                                 /\ nudges' = [nudges EXCEPT ![self] = 0]
                                 /\ confReq' = [confReq EXCEPT ![SvcOf(self)] = NoConf]
                                 /\ pc' = [pc EXCEPT ![self] = "Confirm_wait"]
                            ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                                 /\ UNCHANGED << confReq, gen, nudges, cafter >>
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, fetchQ, fetchCount, 
                                      certIssued, supDaemon, stopped, retryGen, 
                                      leaseReq, drained, advertised, lock, 
                                      cancelReg, cctx, drainRec, pruneRec, 
                                      unpruned, res, out, mark, cnt, leaveRec, 
                                      leaveD, after, seen, leaveEnd, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      pruneApplied, pruneFetched, pruneOK, 
                                      unfetched, hiddenAt, stack, svc, todo, 
                                      cur, early >>

Confirm_wait(self) == /\ pc[self] = "Confirm_wait"
                      /\ \/ /\ fetchCount[gen[self]] > cafter[self] \/ ctxDone[gen[self]]
                            /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                            /\ UNCHANGED <<nudges, cafter>>
                         \/ /\ fetchCount[gen[self]] <= cafter[self]
                            /\ IF ctxDone[gen[self]] \/ drained
                                  THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                       /\ UNCHANGED << nudges, cafter >>
                                  ELSE /\ IF nudges[self] >= MaxNudges /\ Mutation # "unbounded-nudges"
                                             THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                                  /\ UNCHANGED << nudges, 
                                                                  cafter >>
                                             ELSE /\ nudges' = [nudges EXCEPT ![self] = IF nudges[self] > MaxNudges THEN nudges[self] ELSE nudges[self] + 1]
                                                  /\ cafter' = [cafter EXCEPT ![self] = fetchCount[gen[self]]]
                                                  /\ pc' = [pc EXCEPT ![self] = "Readvertise_begin"]
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, fetchQ, fetchCount, 
                                      certIssued, supDaemon, stopped, retryGen, 
                                      confReq, leaseReq, drained, advertised, 
                                      lock, cancelReg, cctx, drainRec, 
                                      pruneRec, unpruned, gen, res, out, mark, 
                                      cnt, leaveRec, leaveD, after, seen, 
                                      leaveEnd, everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      pruneApplied, pruneFetched, pruneOK, 
                                      unfetched, hiddenAt, stack, svc, todo, 
                                      cur, early >>

Readvertise_begin(self) == /\ pc[self] = "Readvertise_begin"
                           /\ lock = None
                           /\ IF drained /\ Checks
                                 THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                      /\ UNCHANGED << pending, waitOn, lock, 
                                                      cancelReg >>
                                 ELSE /\ IF ~CallOK(self)
                                            THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                                 /\ UNCHANGED << pending, 
                                                                 waitOn, lock, 
                                                                 cancelReg >>
                                            ELSE /\ IF SvcOf(self) \notin prefs
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "Readvertise_gap"]
                                                            /\ UNCHANGED << pending, 
                                                                            waitOn, 
                                                                            lock, 
                                                                            cancelReg >>
                                                       ELSE /\ lock' = self
                                                            /\ cancelReg' = self
                                                            /\ LET r == [from |-> self, gen |-> gen[self],
                                                                         val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] IN
                                                                 /\ pending' = (pending \cup {r})
                                                                 /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                            /\ pc' = [pc EXCEPT ![self] = "Readvertise_removed"]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, refused, 
                                           dirty, fetchQ, fetchCount, 
                                           certIssued, supDaemon, stopped, 
                                           retryGen, confReq, leaseReq, 
                                           drained, advertised, cctx, drainRec, 
                                           pruneRec, unpruned, gen, res, out, 
                                           mark, cnt, leaveRec, leaveD, after, 
                                           seen, leaveEnd, everInPrefs, 
                                           lockedDrained, addAfterDrained, 
                                           startedDrained, drainApplied, 
                                           ackMade, ackCounted, ackTimedOut, 
                                           published, leftAdvertising, 
                                           pruneApplied, pruneFetched, pruneOK, 
                                           unfetched, hiddenAt, stack, svc, 
                                           todo, cur, nudges, cafter, early >>

Readvertise_removed(self) == /\ pc[self] = "Readvertise_removed"
                             /\ \/ /\ Applied(self)
                                   /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                   /\ lock' = None
                                   /\ cancelReg' = None
                                   /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                   /\ pc' = [pc EXCEPT ![self] = "Readvertise_gap"]
                                \/ /\ ~CallOK(self)
                                   /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                   /\ lock' = None
                                   /\ cancelReg' = None
                                   /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                   /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                             /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                             daemonUp, ctxDone, crashes, 
                                             pending, refused, dirty, fetchQ, 
                                             fetchCount, certIssued, supDaemon, 
                                             stopped, retryGen, confReq, 
                                             leaseReq, drained, advertised, 
                                             drainRec, pruneRec, unpruned, gen, 
                                             res, out, mark, cnt, leaveRec, 
                                             leaveD, after, seen, leaveEnd, 
                                             everInPrefs, lockedDrained, 
                                             addAfterDrained, startedDrained, 
                                             drainApplied, ackMade, ackCounted, 
                                             ackTimedOut, published, 
                                             leftAdvertising, pruneApplied, 
                                             pruneFetched, pruneOK, unfetched, 
                                             hiddenAt, stack, svc, todo, cur, 
                                             nudges, cafter, early >>

Readvertise_gap(self) == /\ pc[self] = "Readvertise_gap"
                         /\ lock = None \/ ctxDone[gen[self]]
                         /\ IF ctxDone[gen[self]]
                               THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                    /\ UNCHANGED << pending, waitOn, lock, 
                                                    cancelReg, lockedDrained, 
                                                    addAfterDrained >>
                               ELSE /\ IF drained /\ Checks
                                          THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                               /\ UNCHANGED << pending, waitOn, 
                                                               lock, cancelReg, 
                                                               lockedDrained, 
                                                               addAfterDrained >>
                                          ELSE /\ IF ~CallOK(self)
                                                     THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                                          /\ UNCHANGED << pending, 
                                                                          waitOn, 
                                                                          lock, 
                                                                          cancelReg, 
                                                                          lockedDrained, 
                                                                          addAfterDrained >>
                                                     ELSE /\ IF SvcOf(self) \in prefs
                                                                THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_wait"]
                                                                     /\ UNCHANGED << pending, 
                                                                                     waitOn, 
                                                                                     lock, 
                                                                                     cancelReg, 
                                                                                     lockedDrained, 
                                                                                     addAfterDrained >>
                                                                ELSE /\ lock' = self
                                                                     /\ cancelReg' = self
                                                                     /\ lockedDrained' = [lockedDrained EXCEPT ![self] = drained]
                                                                     /\ LET r == [from |-> self, gen |-> gen[self],
                                                                                  val |-> prefs \cup {SvcOf(self)}, kind |-> "add"] IN
                                                                          /\ pending' = (pending \cup {r})
                                                                          /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                                     /\ addAfterDrained' = (addAfterDrained \/ drained)
                                                                     /\ pc' = [pc EXCEPT ![self] = "Readvertise_added"]
                         /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                         daemonUp, ctxDone, crashes, refused, 
                                         dirty, fetchQ, fetchCount, certIssued, 
                                         supDaemon, stopped, retryGen, confReq, 
                                         leaseReq, drained, advertised, cctx, 
                                         drainRec, pruneRec, unpruned, gen, 
                                         res, out, mark, cnt, leaveRec, leaveD, 
                                         after, seen, leaveEnd, everInPrefs, 
                                         startedDrained, drainApplied, ackMade, 
                                         ackCounted, ackTimedOut, published, 
                                         leftAdvertising, pruneApplied, 
                                         pruneFetched, pruneOK, unfetched, 
                                         hiddenAt, stack, svc, todo, cur, 
                                         nudges, cafter, early >>

Readvertise_added(self) == /\ pc[self] = "Readvertise_added"
                           /\ \/ /\ Applied(self)
                                 /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                 /\ lock' = None
                                 /\ cancelReg' = None
                                 /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                 /\ lockedDrained' = [lockedDrained EXCEPT ![self] = FALSE]
                                 /\ pc' = [pc EXCEPT ![self] = "Confirm_wait"]
                                 /\ UNCHANGED refused
                              \/ /\ ~CallOK(self) \/ refused[self]
                                 /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                 /\ refused' = [refused EXCEPT ![self] = FALSE]
                                 /\ lock' = None
                                 /\ cancelReg' = None
                                 /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                 /\ lockedDrained' = [lockedDrained EXCEPT ![self] = FALSE]
                                 /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, pending, 
                                           dirty, fetchQ, fetchCount, 
                                           certIssued, supDaemon, stopped, 
                                           retryGen, confReq, leaseReq, 
                                           drained, advertised, drainRec, 
                                           pruneRec, unpruned, gen, res, out, 
                                           mark, cnt, leaveRec, leaveD, after, 
                                           seen, leaveEnd, everInPrefs, 
                                           addAfterDrained, startedDrained, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, pruneApplied, 
                                           pruneFetched, pruneOK, unfetched, 
                                           hiddenAt, stack, svc, todo, cur, 
                                           nudges, cafter, early >>

Confirm(self) == Confirm_idle(self) \/ Confirm_wait(self)
                    \/ Readvertise_begin(self) \/ Readvertise_removed(self)
                    \/ Readvertise_gap(self) \/ Readvertise_added(self)

Lease_idle(self) == /\ pc[self] = "Lease_idle"
                    /\ IF ~stopped
                          THEN /\ leaseReq[SvcOf(self)] # 0
                               /\ gen' = [gen EXCEPT ![self] = leaseReq[SvcOf(self)]]
                               /\ leaseReq' = [leaseReq EXCEPT ![SvcOf(self)] = 0]
                               /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                          ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
                               /\ UNCHANGED << leaseReq, gen >>
                    /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                    ctxDone, crashes, pending, waitOn, refused, 
                                    dirty, fetchQ, fetchCount, certIssued, 
                                    supDaemon, stopped, retryGen, confReq, 
                                    drained, advertised, lock, cancelReg, cctx, 
                                    drainRec, pruneRec, unpruned, res, out, 
                                    mark, cnt, leaveRec, leaveD, after, seen, 
                                    leaveEnd, everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    drainApplied, ackMade, ackCounted, 
                                    ackTimedOut, published, leftAdvertising, 
                                    pruneApplied, pruneFetched, pruneOK, 
                                    unfetched, hiddenAt, stack, svc, todo, cur, 
                                    nudges, cafter, early >>

Lease_poll(self) == /\ pc[self] = "Lease_poll"
                    /\ early' = [early EXCEPT ![self] = FALSE]
                    /\ IF ctxDone[gen[self]] \/ drained
                          THEN /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                               /\ UNCHANGED << stack, svc >>
                          ELSE /\ IF certIssued[SvcOf(self)]
                                     THEN /\ /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Configure",
                                                                                      pc        |->  "Lease_result",
                                                                                      svc       |->  svc[self] ] >>
                                                                                  \o stack[self]]
                                             /\ svc' = [svc EXCEPT ![self] = SvcOf(self)]
                                          /\ pc' = [pc EXCEPT ![self] = "Change_lock"]
                                     ELSE /\ pc' = [pc EXCEPT ![self] = "Lease_unadvertise"]
                                          /\ UNCHANGED << stack, svc >>
                    /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                    ctxDone, crashes, pending, waitOn, refused, 
                                    dirty, fetchQ, fetchCount, certIssued, 
                                    supDaemon, stopped, retryGen, confReq, 
                                    leaseReq, drained, advertised, lock, 
                                    cancelReg, cctx, drainRec, pruneRec, 
                                    unpruned, gen, res, out, mark, cnt, 
                                    leaveRec, leaveD, after, seen, leaveEnd, 
                                    everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    drainApplied, ackMade, ackCounted, 
                                    ackTimedOut, published, leftAdvertising, 
                                    pruneApplied, pruneFetched, pruneOK, 
                                    unfetched, hiddenAt, todo, cur, nudges, 
                                    cafter >>

Lease_unadvertise(self) == /\ pc[self] = "Lease_unadvertise"
                           /\ lock = None
                           /\ IF drained /\ Checks
                                 THEN /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                                      /\ UNCHANGED << pending, waitOn, lock, 
                                                      cancelReg >>
                                 ELSE /\ IF ~CallOK(self)
                                            THEN /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                                                 /\ UNCHANGED << pending, 
                                                                 waitOn, lock, 
                                                                 cancelReg >>
                                            ELSE /\ IF SvcOf(self) \notin prefs
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "Lease_request"]
                                                            /\ UNCHANGED << pending, 
                                                                            waitOn, 
                                                                            lock, 
                                                                            cancelReg >>
                                                       ELSE /\ lock' = self
                                                            /\ cancelReg' = self
                                                            /\ LET r == [from |-> self, gen |-> gen[self],
                                                                         val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] IN
                                                                 /\ pending' = (pending \cup {r})
                                                                 /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                            /\ pc' = [pc EXCEPT ![self] = "Lease_unadvertiseSent"]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, refused, 
                                           dirty, fetchQ, fetchCount, 
                                           certIssued, supDaemon, stopped, 
                                           retryGen, confReq, leaseReq, 
                                           drained, advertised, cctx, drainRec, 
                                           pruneRec, unpruned, gen, res, out, 
                                           mark, cnt, leaveRec, leaveD, after, 
                                           seen, leaveEnd, everInPrefs, 
                                           lockedDrained, addAfterDrained, 
                                           startedDrained, drainApplied, 
                                           ackMade, ackCounted, ackTimedOut, 
                                           published, leftAdvertising, 
                                           pruneApplied, pruneFetched, pruneOK, 
                                           unfetched, hiddenAt, stack, svc, 
                                           todo, cur, nudges, cafter, early >>

Lease_unadvertiseSent(self) == /\ pc[self] = "Lease_unadvertiseSent"
                               /\ \/ /\ Applied(self)
                                     /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                     /\ lock' = None
                                     /\ cancelReg' = None
                                     /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                     /\ pc' = [pc EXCEPT ![self] = "Lease_request"]
                                  \/ /\ ~CallOK(self)
                                     /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                                     /\ lock' = None
                                     /\ cancelReg' = None
                                     /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                                     /\ IF drained
                                           THEN /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                                           ELSE /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                               /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                               daemonUp, ctxDone, crashes, 
                                               pending, refused, dirty, fetchQ, 
                                               fetchCount, certIssued, 
                                               supDaemon, stopped, retryGen, 
                                               confReq, leaseReq, drained, 
                                               advertised, drainRec, pruneRec, 
                                               unpruned, gen, res, out, mark, 
                                               cnt, leaveRec, leaveD, after, 
                                               seen, leaveEnd, everInPrefs, 
                                               lockedDrained, addAfterDrained, 
                                               startedDrained, drainApplied, 
                                               ackMade, ackCounted, 
                                               ackTimedOut, published, 
                                               leftAdvertising, pruneApplied, 
                                               pruneFetched, pruneOK, 
                                               unfetched, hiddenAt, stack, svc, 
                                               todo, cur, nudges, cafter, 
                                               early >>

Lease_request(self) == /\ pc[self] = "Lease_request"
                       /\ certIssued[SvcOf(self)] \/ ~early[self] \/ ctxDone[gen[self]]
                          \/ drained
                       /\ IF ctxDone[gen[self]] \/ drained
                             THEN /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                                  /\ UNCHANGED << stack, svc >>
                             ELSE /\ IF certIssued[SvcOf(self)]
                                        THEN /\ /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Configure",
                                                                                         pc        |->  "Lease_result",
                                                                                         svc       |->  svc[self] ] >>
                                                                                     \o stack[self]]
                                                /\ svc' = [svc EXCEPT ![self] = SvcOf(self)]
                                             /\ pc' = [pc EXCEPT ![self] = "Change_lock"]
                                        ELSE /\ /\ stack' = [stack EXCEPT ![self] = << [ procedure |->  "Configure",
                                                                                         pc        |->  "Lease_advertisedEarly",
                                                                                         svc       |->  svc[self] ] >>
                                                                                     \o stack[self]]
                                                /\ svc' = [svc EXCEPT ![self] = SvcOf(self)]
                                             /\ pc' = [pc EXCEPT ![self] = "Change_lock"]
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, pending, waitOn, 
                                       refused, dirty, fetchQ, fetchCount, 
                                       certIssued, supDaemon, stopped, 
                                       retryGen, confReq, leaseReq, drained, 
                                       advertised, lock, cancelReg, cctx, 
                                       drainRec, pruneRec, unpruned, gen, res, 
                                       out, mark, cnt, leaveRec, leaveD, after, 
                                       seen, leaveEnd, everInPrefs, 
                                       lockedDrained, addAfterDrained, 
                                       startedDrained, drainApplied, ackMade, 
                                       ackCounted, ackTimedOut, published, 
                                       leftAdvertising, pruneApplied, 
                                       pruneFetched, pruneOK, unfetched, 
                                       hiddenAt, todo, cur, nudges, cafter, 
                                       early >>

Lease_advertisedEarly(self) == /\ pc[self] = "Lease_advertisedEarly"
                               /\ IF out[self] = "ok"
                                     THEN /\ early' = [early EXCEPT ![self] = TRUE]
                                          /\ out' = [out EXCEPT ![self] = "ok"]
                                          /\ pc' = [pc EXCEPT ![self] = "Lease_request"]
                                     ELSE /\ IF out[self] = "drained"
                                                THEN /\ out' = [out EXCEPT ![self] = "ok"]
                                                     /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                                                ELSE /\ out' = [out EXCEPT ![self] = "ok"]
                                                     /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                                          /\ early' = early
                               /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                               daemonUp, ctxDone, crashes, 
                                               pending, waitOn, refused, dirty, 
                                               fetchQ, fetchCount, certIssued, 
                                               supDaemon, stopped, retryGen, 
                                               confReq, leaseReq, drained, 
                                               advertised, lock, cancelReg, 
                                               cctx, drainRec, pruneRec, 
                                               unpruned, gen, res, mark, cnt, 
                                               leaveRec, leaveD, after, seen, 
                                               leaveEnd, everInPrefs, 
                                               lockedDrained, addAfterDrained, 
                                               startedDrained, drainApplied, 
                                               ackMade, ackCounted, 
                                               ackTimedOut, published, 
                                               leftAdvertising, pruneApplied, 
                                               pruneFetched, pruneOK, 
                                               unfetched, hiddenAt, stack, svc, 
                                               todo, cur, nudges, cafter >>

Lease_result(self) == /\ pc[self] = "Lease_result"
                      /\ IF out[self] = "err" /\ ~ctxDone[gen[self]]
                            THEN /\ out' = [out EXCEPT ![self] = "ok"]
                                 /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                                 /\ early' = early
                            ELSE /\ out' = [out EXCEPT ![self] = "ok"]
                                 /\ early' = [early EXCEPT ![self] = FALSE]
                                 /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, fetchQ, fetchCount, 
                                      certIssued, supDaemon, stopped, retryGen, 
                                      confReq, leaseReq, drained, advertised, 
                                      lock, cancelReg, cctx, drainRec, 
                                      pruneRec, unpruned, gen, res, mark, cnt, 
                                      leaveRec, leaveD, after, seen, leaveEnd, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      pruneApplied, pruneFetched, pruneOK, 
                                      unfetched, hiddenAt, stack, svc, todo, 
                                      cur, nudges, cafter >>

LeaseHolder(self) == Lease_idle(self) \/ Lease_poll(self)
                        \/ Lease_unadvertise(self)
                        \/ Lease_unadvertiseSent(self)
                        \/ Lease_request(self)
                        \/ Lease_advertisedEarly(self)
                        \/ Lease_result(self)

Stop_signal == /\ pc["stop"] = "Stop_signal"
               /\ \/ /\ published /\ NeedsDrainRead
                     /\ IF NeedsDrain
                           THEN /\ stack' = [stack EXCEPT !["stop"] = << [ procedure |->  "DrainProc",
                                                                           pc        |->  "Done" ] >>
                                                                       \o stack["stop"]]
                                /\ pc' = [pc EXCEPT !["stop"] = "Drain_begin"]
                           ELSE /\ pc' = [pc EXCEPT !["stop"] = "Done"]
                                /\ stack' = stack
                  \/ /\ TRUE
                     /\ pc' = [pc EXCEPT !["stop"] = "Done"]
                     /\ stack' = stack
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, fetchQ, fetchCount, certIssued, 
                               supDaemon, stopped, retryGen, confReq, leaseReq, 
                               drained, advertised, lock, cancelReg, cctx, 
                               drainRec, pruneRec, unpruned, gen, res, out, 
                               mark, cnt, leaveRec, leaveD, after, seen, 
                               leaveEnd, everInPrefs, lockedDrained, 
                               addAfterDrained, startedDrained, drainApplied, 
                               ackMade, ackCounted, ackTimedOut, published, 
                               leftAdvertising, pruneApplied, pruneFetched, 
                               pruneOK, unfetched, hiddenAt, svc, todo, cur, 
                               nudges, cafter, early >>

StopSignal == Stop_signal

Leave_begin == /\ pc["leave"] = "Leave_begin"
               /\ ~published \/ NeedsDrainRead
               /\ IF ~published
                     THEN /\ leaveEnd' = "aborted"
                          /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                          /\ stack' = stack
                     ELSE /\ IF ~NeedsDrain
                                THEN /\ leaveEnd' = "nodrain"
                                     /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                     /\ stack' = stack
                                ELSE /\ stack' = [stack EXCEPT !["leave"] = << [ procedure |->  "DrainProc",
                                                                                 pc        |->  "Wait_lock" ] >>
                                                                             \o stack["leave"]]
                                     /\ pc' = [pc EXCEPT !["leave"] = "Drain_begin"]
                                     /\ UNCHANGED leaveEnd
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, fetchQ, fetchCount, certIssued, 
                               supDaemon, stopped, retryGen, confReq, leaseReq, 
                               drained, advertised, lock, cancelReg, cctx, 
                               drainRec, pruneRec, unpruned, gen, res, out, 
                               mark, cnt, leaveRec, leaveD, after, seen, 
                               everInPrefs, lockedDrained, addAfterDrained, 
                               startedDrained, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, pruneApplied, pruneFetched, 
                               pruneOK, unfetched, hiddenAt, svc, todo, cur, 
                               nudges, cafter, early >>

Wait_lock == /\ pc["leave"] = "Wait_lock"
             /\ lock = None
             /\ leaveRec' = (IF drainRec.external /\ pruneRec.set THEN pruneRec
                             ELSE drainRec)
             /\ pc' = [pc EXCEPT !["leave"] = "Wait_recordRead"]
             /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                             crashes, pending, waitOn, refused, dirty, fetchQ, 
                             fetchCount, certIssued, supDaemon, stopped, 
                             retryGen, confReq, leaseReq, drained, advertised, 
                             lock, cancelReg, cctx, drainRec, pruneRec, 
                             unpruned, gen, res, out, mark, cnt, leaveD, after, 
                             seen, leaveEnd, everInPrefs, lockedDrained, 
                             addAfterDrained, startedDrained, drainApplied, 
                             ackMade, ackCounted, ackTimedOut, published, 
                             leftAdvertising, pruneApplied, pruneFetched, 
                             pruneOK, unfetched, hiddenAt, stack, svc, todo, 
                             cur, nudges, cafter, early >>

Wait_recordRead == /\ pc["leave"] = "Wait_recordRead"
                   /\ leaveD' = supDaemon
                   /\ IF ~leaveRec.set
                         THEN /\ leaveEnd' = "norecord"
                              /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                              /\ after' = after
                         ELSE /\ IF leaveRec.external /\ Mutation # "always-wait"
                                    THEN /\ leaveEnd' = "external"
                                         /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                         /\ after' = after
                                    ELSE /\ IF supDaemon = 0
                                               THEN /\ leaveEnd' = "nodaemon"
                                                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                                    /\ after' = after
                                               ELSE /\ IF ~advertised /\ Mutation # "always-wait"
                                                          THEN /\ leaveEnd' = "unadvertised"
                                                               /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                                               /\ after' = after
                                                          ELSE /\ after' = (IF supDaemon # leaveRec.gen /\ Mutation # "ignore-restart"
                                                                            THEN 0 ELSE leaveRec.fetches)
                                                               /\ pc' = [pc EXCEPT !["leave"] = "Wait_check"]
                                                               /\ UNCHANGED leaveEnd
                   /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                   ctxDone, crashes, pending, waitOn, refused, 
                                   dirty, fetchQ, fetchCount, certIssued, 
                                   supDaemon, stopped, retryGen, confReq, 
                                   leaseReq, drained, advertised, lock, 
                                   cancelReg, cctx, drainRec, pruneRec, 
                                   unpruned, gen, res, out, mark, cnt, 
                                   leaveRec, seen, everInPrefs, lockedDrained, 
                                   addAfterDrained, startedDrained, 
                                   drainApplied, ackMade, ackCounted, 
                                   ackTimedOut, published, leftAdvertising, 
                                   pruneApplied, pruneFetched, pruneOK, 
                                   unfetched, hiddenAt, stack, svc, todo, cur, 
                                   nudges, cafter, early >>

Wait_check == /\ pc["leave"] = "Wait_check"
              /\ IF fetchCount[leaveD] > after
                    THEN /\ leaveEnd' = "acked"
                         /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                         /\ seen' = seen
                    ELSE /\ seen' = fetchCount[leaveD]
                         /\ pc' = [pc EXCEPT !["leave"] = "Wait_block"]
                         /\ UNCHANGED leaveEnd
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, fetchQ, 
                              fetchCount, certIssued, supDaemon, stopped, 
                              retryGen, confReq, leaseReq, drained, advertised, 
                              lock, cancelReg, cctx, drainRec, pruneRec, 
                              unpruned, gen, res, out, mark, cnt, leaveRec, 
                              leaveD, after, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              leftAdvertising, pruneApplied, pruneFetched, 
                              pruneOK, unfetched, hiddenAt, stack, svc, todo, 
                              cur, nudges, cafter, early >>

Wait_block == /\ pc["leave"] = "Wait_block"
              /\ \/ /\ fetchCount[leaveD] # seen
                    /\ pc' = [pc EXCEPT !["leave"] = "Wait_check"]
                    /\ UNCHANGED <<leaveEnd, ackTimedOut>>
                 \/ /\ ctxDone[leaveD]
                    /\ leaveEnd' = "ctx"
                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                    /\ UNCHANGED ackTimedOut
                 \/ /\ Mutation # "wait-no-timeout" /\ fetchCount[leaveD] = seen
                    /\ leaveEnd' = "timeout"
                    /\ ackTimedOut' = ackCounted[leaveD]
                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, fetchQ, 
                              fetchCount, certIssued, supDaemon, stopped, 
                              retryGen, confReq, leaseReq, drained, advertised, 
                              lock, cancelReg, cctx, drainRec, pruneRec, 
                              unpruned, gen, res, out, mark, cnt, leaveRec, 
                              leaveD, after, seen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, published, leftAdvertising, 
                              pruneApplied, pruneFetched, pruneOK, unfetched, 
                              hiddenAt, stack, svc, todo, cur, nudges, cafter, 
                              early >>

Leave_done == /\ pc["leave"] = "Leave_done"
              /\ leftAdvertising' = (daemonUp /\ prefs # {}
                                     /\ ~(drainApplied /\ leaveEnd \in {"acked", "timeout"}))
              /\ stopped' = TRUE
              /\ daemonUp' = FALSE
              /\ supDaemon' = 0
              /\ pending' = {}
              /\ ctxDone' = [g \in Gens |-> TRUE]
              /\ pc' = [pc EXCEPT !["leave"] = "Done"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, crashes, waitOn, 
                              refused, dirty, fetchQ, fetchCount, certIssued, 
                              retryGen, confReq, leaseReq, drained, advertised, 
                              lock, cancelReg, cctx, drainRec, pruneRec, 
                              unpruned, gen, res, out, mark, cnt, leaveRec, 
                              leaveD, after, seen, leaveEnd, everInPrefs, 
                              lockedDrained, addAfterDrained, startedDrained, 
                              drainApplied, ackMade, ackCounted, ackTimedOut, 
                              published, pruneApplied, pruneFetched, pruneOK, 
                              unfetched, hiddenAt, stack, svc, todo, cur, 
                              nudges, cafter, early >>

Leave == Leave_begin \/ Wait_lock \/ Wait_recordRead \/ Wait_check
            \/ Wait_block \/ Leave_done

Tailscaled_answer == /\ pc["tailscaled"] = "Tailscaled_answer"
                     /\ IF ~stopped
                           THEN /\ \/ /\ \E r \in {q \in pending : daemonUp /\ q.gen = daemonGen}:
                                           /\ IF r.val # prefs
                                                 THEN /\ dirty' = TRUE
                                                 ELSE /\ TRUE
                                                      /\ dirty' = dirty
                                           /\ IF (prefs \ r.val) \cap Services # {}
                                                 THEN /\ unfetched' = (unfetched \cup {r.from})
                                                 ELSE /\ TRUE
                                                      /\ UNCHANGED unfetched
                                           /\ IF r.kind = "prune" /\ Stale \in prefs
                                                 THEN /\ pruneApplied' = TRUE
                                                      /\ pruneFetched' = FALSE
                                                 ELSE /\ TRUE
                                                      /\ UNCHANGED << pruneApplied, 
                                                                      pruneFetched >>
                                           /\ prefs' = r.val
                                           /\ pending' = pending \ {r}
                                           /\ IF r.kind = "drain"
                                                 THEN /\ drainApplied' = TRUE
                                                 ELSE /\ TRUE
                                                      /\ UNCHANGED drainApplied
                                           /\ everInPrefs' = (everInPrefs \/ r.val # {})
                                           /\ IF r.val # {}
                                                 THEN /\ ackMade' = FALSE
                                                      /\ ackCounted' = [g \in Gens |-> FALSE]
                                                      /\ fetchQ' = [g \in Gens |-> [i \in DOMAIN fetchQ[g] |-> FALSE]]
                                                 ELSE /\ TRUE
                                                      /\ UNCHANGED << fetchQ, 
                                                                      ackMade, 
                                                                      ackCounted >>
                                      /\ UNCHANGED refused
                                   \/ /\ \E r \in {q \in pending : daemonUp /\ q.gen = daemonGen
                                                                 /\ q.kind = "add"
                                                                 /\ waitOn[q.from] = q}:
                                           /\ refused' = [refused EXCEPT ![r.from] = TRUE]
                                           /\ pending' = pending \ {r}
                                      /\ UNCHANGED <<prefs, dirty, fetchQ, everInPrefs, drainApplied, ackMade, ackCounted, pruneApplied, pruneFetched, unfetched>>
                                /\ pc' = [pc EXCEPT !["tailscaled"] = "Tailscaled_answer"]
                           ELSE /\ pc' = [pc EXCEPT !["tailscaled"] = "Done"]
                                /\ UNCHANGED << prefs, pending, refused, dirty, 
                                                fetchQ, everInPrefs, 
                                                drainApplied, ackMade, 
                                                ackCounted, pruneApplied, 
                                                pruneFetched, unfetched >>
                     /\ UNCHANGED << stateExists, daemonGen, daemonUp, ctxDone, 
                                     crashes, waitOn, fetchCount, certIssued, 
                                     supDaemon, stopped, retryGen, confReq, 
                                     leaseReq, drained, advertised, lock, 
                                     cancelReg, cctx, drainRec, pruneRec, 
                                     unpruned, gen, res, out, mark, cnt, 
                                     leaveRec, leaveD, after, seen, leaveEnd, 
                                     lockedDrained, addAfterDrained, 
                                     startedDrained, ackTimedOut, published, 
                                     leftAdvertising, pruneOK, hiddenAt, stack, 
                                     svc, todo, cur, nudges, cafter, early >>

Tailscaled == Tailscaled_answer

Control_fetch == /\ pc["control"] = "Control_fetch"
                 /\ IF ~stopped
                       THEN /\ daemonUp /\ dirty
                            /\ dirty' = FALSE
                            /\ fetchQ' = [fetchQ EXCEPT ![daemonGen] = Append(fetchQ[daemonGen], Ack)]
                            /\ ackMade' = (ackMade \/ Ack)
                            /\ pruneFetched' = (pruneFetched \/ pruneApplied)
                            /\ unfetched' = {}
                            /\ pc' = [pc EXCEPT !["control"] = "Control_fetch"]
                       ELSE /\ pc' = [pc EXCEPT !["control"] = "Done"]
                            /\ UNCHANGED << dirty, fetchQ, ackMade, 
                                            pruneFetched, unfetched >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 fetchCount, certIssued, supDaemon, stopped, 
                                 retryGen, confReq, leaseReq, drained, 
                                 advertised, lock, cancelReg, cctx, drainRec, 
                                 pruneRec, unpruned, gen, res, out, mark, cnt, 
                                 leaveRec, leaveD, after, seen, leaveEnd, 
                                 everInPrefs, lockedDrained, addAfterDrained, 
                                 startedDrained, drainApplied, ackCounted, 
                                 ackTimedOut, published, leftAdvertising, 
                                 pruneApplied, pruneOK, hiddenAt, stack, svc, 
                                 todo, cur, nudges, cafter, early >>

Control == Control_fetch

Counter_count == /\ pc["counter"] = "Counter_count"
                 /\ IF ~stopped
                       THEN /\ \E g \in {x \in Gens : fetchQ[x] # <<>> /\ Reads(x)}:
                                 /\ fetchCount' = [fetchCount EXCEPT ![g] = fetchCount[g] + 1]
                                 /\ ackCounted' = [ackCounted EXCEPT ![g] = ackCounted[g] \/ Head(fetchQ[g])]
                                 /\ fetchQ' = [fetchQ EXCEPT ![g] = Tail(fetchQ[g])]
                            /\ pc' = [pc EXCEPT !["counter"] = "Counter_count"]
                       ELSE /\ pc' = [pc EXCEPT !["counter"] = "Done"]
                            /\ UNCHANGED << fetchQ, fetchCount, ackCounted >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 dirty, certIssued, supDaemon, stopped, 
                                 retryGen, confReq, leaseReq, drained, 
                                 advertised, lock, cancelReg, cctx, drainRec, 
                                 pruneRec, unpruned, gen, res, out, mark, cnt, 
                                 leaveRec, leaveD, after, seen, leaveEnd, 
                                 everInPrefs, lockedDrained, addAfterDrained, 
                                 startedDrained, drainApplied, ackMade, 
                                 ackTimedOut, published, leftAdvertising, 
                                 pruneApplied, pruneFetched, pruneOK, 
                                 unfetched, hiddenAt, stack, svc, todo, cur, 
                                 nudges, cafter, early >>

Counter == Counter_count

Crash_exit == /\ pc["crash"] = "Crash_exit"
              /\ IF ~stopped /\ crashes < MaxRestarts
                    THEN /\ daemonUp
                         /\ daemonUp' = FALSE
                         /\ crashes' = crashes + 1
                         /\ pending' = {r \in pending : r.gen # daemonGen}
                         /\ dirty' = FALSE
                         /\ pc' = [pc EXCEPT !["crash"] = "Crash_exit"]
                    ELSE /\ pc' = [pc EXCEPT !["crash"] = "Done"]
                         /\ UNCHANGED << daemonUp, crashes, pending, dirty >>
              /\ UNCHANGED << stateExists, prefs, daemonGen, ctxDone, waitOn, 
                              refused, fetchQ, fetchCount, certIssued, 
                              supDaemon, stopped, retryGen, confReq, leaseReq, 
                              drained, advertised, lock, cancelReg, cctx, 
                              drainRec, pruneRec, unpruned, gen, res, out, 
                              mark, cnt, leaveRec, leaveD, after, seen, 
                              leaveEnd, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              leftAdvertising, pruneApplied, pruneFetched, 
                              pruneOK, unfetched, hiddenAt, stack, svc, todo, 
                              cur, nudges, cafter, early >>

Crash == Crash_exit

Cert_issue == /\ pc["issuer"] = "Cert_issue"
              /\ IF ~stopped
                    THEN /\ \E s \in {x \in Services : ~certIssued[x]
                                                       /\ pc[LeaseOf(x)] = "Lease_request"}:
                              certIssued' = [certIssued EXCEPT ![s] = TRUE]
                         /\ pc' = [pc EXCEPT !["issuer"] = "Cert_issue"]
                    ELSE /\ pc' = [pc EXCEPT !["issuer"] = "Done"]
                         /\ UNCHANGED certIssued
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, fetchQ, 
                              fetchCount, supDaemon, stopped, retryGen, 
                              confReq, leaseReq, drained, advertised, lock, 
                              cancelReg, cctx, drainRec, pruneRec, unpruned, 
                              gen, res, out, mark, cnt, leaveRec, leaveD, 
                              after, seen, leaveEnd, everInPrefs, 
                              lockedDrained, addAfterDrained, startedDrained, 
                              drainApplied, ackMade, ackCounted, ackTimedOut, 
                              published, leftAdvertising, pruneApplied, 
                              pruneFetched, pruneOK, unfetched, hiddenAt, 
                              stack, svc, todo, cur, nudges, cafter, early >>

Issuer == Cert_issue

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == Supervisor \/ Retry \/ StopSignal \/ Leave \/ Tailscaled \/ Control
           \/ Counter \/ Crash \/ Issuer
           \/ (\E self \in ProcSet: Configure(self) \/ DrainProc(self))
           \/ (\E self \in Confs: Confirm(self))
           \/ (\E self \in Leases: LeaseHolder(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(Supervisor) /\ WF_vars(Configure("sup"))
        /\ WF_vars(Retry) /\ WF_vars(Configure("retry"))
        /\ \A self \in Confs : WF_vars(Confirm(self))
        /\ \A self \in Leases : WF_vars(LeaseHolder(self)) /\ WF_vars(Configure(self))
        /\ WF_vars(StopSignal) /\ WF_vars(DrainProc("stop"))
        /\ WF_vars(Leave) /\ WF_vars(DrainProc("leave"))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION

\* Once Leave stopped the supervisor, nothing the properties read changes:
\* the cfgs explore no step after it (ACTION_CONSTRAINT Running), which
\* leaves out the other processes' ways to their end.
Running == ~stopped

\* The state as far as the fetch counts matter (VIEW CountView, in the
\* cfgs that enable the confirmer): a count read from daemon g's
\* d.fetches is only ever compared with d.fetches of the same daemon,
\* which only grows (DrainAndWait compares a count of an earlier daemon
\* with 0 instead). So what decides every later step is whether a fetch
\* was counted since the read, not the numbers: states that agree on that
\* are the same to every property. (Not for ignore-restart, which compares
\* counts of different daemons.)
Since(v, g) ==
    IF g = 0 THEN <<"raw", v>> ELSE <<"since", fetchCount[g] > v>>
CountView ==
    << stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending,
       waitOn, refused, dirty, fetchQ, [g \in Gens |-> fetchCount[g] > 0],
       certIssued, supDaemon, stopped, retryGen,
       [s \in Services |-> [gen |-> confReq[s].gen,
                            after |-> Since(confReq[s].after, confReq[s].gen)]],
       leaseReq, drained, advertised, lock, cancelReg, cctx,
       [drainRec EXCEPT !.fetches = Since(drainRec.fetches, drainRec.gen)],
       [pruneRec EXCEPT !.fetches = Since(pruneRec.fetches, pruneRec.gen)],
       unpruned, gen, res, out,
       [a \in Actors |-> Since(mark[a], gen[a])],
       [a \in Actors |-> Since(cnt[a], gen[a])],
       [leaveRec EXCEPT !.fetches = Since(leaveRec.fetches, leaveRec.gen)],
       leaveD, Since(after, leaveD), Since(seen, leaveD), leaveEnd,
       everInPrefs, lockedDrained, addAfterDrained, startedDrained,
       drainApplied, ackMade, ackCounted, ackTimedOut, published,
       leftAdvertising, pruneApplied, pruneFetched, pruneOK, unfetched, hiddenAt, pc,
       stack, svc, todo, cur, nudges,
       [c \in Confs |-> Since(cafter[c], gen[c])], early >>

\* L3: a confirmer re-advertises at most advertiseNudges times, and stops
\* (on a fetch, a drain, its daemon's end, or after the nudges). After the
\* translation: it reads the confirmers' own variable nudges
ConfirmerStops ==
    /\ [](\A c \in Confs : nudges[c] <= MaxNudges)
    /\ \A c \in Confs :
           pc[c] # "Confirm_idle" ~> pc[c] \in {"Confirm_idle", "Done"}
====
