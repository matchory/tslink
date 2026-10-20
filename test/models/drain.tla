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
\* The procedures Configure (configureServiceWhenCertified,
\* configureServiceBackend, serviceGate.change, runAdvertise, addAdvertised)
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
\* - Not modelled here (Task 3): pruneServices, the confirmer
\*   (confirmAdvertisedLater, readvertise, MaxNudges), the certificate lease
\*   holder (advertise after certDomainGrace), checkApprovalLater.
\*
\* Bounds (drain.cfg): two Services, one restart.
\*
\* Mutations (drain-mut-<name>.cfg, each must break its property):
\*   no-drained-check  no isDrained/drained check on the configure path
\*   always-wait       DrainAndWait ignores external and advertised
\*   ignore-restart    DrainAndWait keeps drain.fetches after a restart
\*   wait-no-timeout   waitServicesFetch has no deadline
\*   no-cancel         markDrained cancels nothing
EXTENDS Naturals, Sequences, TLC

CONSTANTS
    Services,       \* the container's Services
    MaxRestarts,    \* how often tailscaled may exit and restart
    MaxNudges,      \* advertiseNudges (the confirmer); unused until Task 3
    Mutation        \* "none", or the name of a mutation (drain-mut-*.cfg)

None == "none"
Changers == {"sup", "retry"}       \* run changes of the gate
Drainers == {"stop", "leave"}       \* call DrainProc
Actors == Changers \cup Drainers
Gens == 1..(MaxRestarts + 1)        \* tailscaled processes, by start
NoReq == [from |-> None, gen |-> 0, val |-> {}, kind |-> None]
NoRec == [set |-> FALSE, gen |-> 0, fetches |-> 0, external |-> FALSE]

(*--algorithm drain
variables
    \* tailscaled and control
    stateExists \in BOOLEAN,        \* StateExists(StateDir)
    \* the node's persisted AdvertiseServices; from a previous run, if any
    prefs \in IF stateExists THEN {{}, Services} ELSE {{}},
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
    \* the supervisor
    supDaemon = 0,                  \* s.daemon; 0 is nil
    stopped = FALSE,                \* the endpoint stopped the supervisor
    retryGen = [s \in Services |-> 0], \* retryLater runs, for daemon g
    \* the gate (serviceGate)
    drained = FALSE,
    advertised = FALSE,
    lock = None,                    \* g.mu's holder
    cancelReg = None,               \* g.cancel: whose change it cancels
    cctx = [a \in Actors |-> FALSE], \* a's change context is cancelled
    drainRec = NoRec,               \* g.drain
    \* per actor
    gen = [a \in Actors |-> 0],     \* the daemon a works on (b.d, d)
    res = [a \in Actors |-> "ok"],  \* Configure failed: err
    cnt = [a \in Actors |-> 0],     \* fetches read by daemon.Drain
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
    leftAdvertising = FALSE;        \* Leave stopped a daemon advertising

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
    \* An actor reads daemon g's fetch count in its next step. Counting a
    \* fetch changes nothing else, so the model counts only then: an
    \* earlier count is the same as one just before the read.
    Reads(g) ==
        \/ pc["leave"] \in {"Wait_check", "Wait_block"} /\ leaveD = g
        \/ \E p \in Drainers : pc[p] = "Drain_marked" /\ supDaemon = g

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
    \* the first start, which runs no DrainAndWait.
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
    \* S7: a counted fetch after the drain ends the wait before its timeout
    AckEndsWait == ~ackTimedOut
    \* L1: DrainAndWait returns, also if control never fetches
    LeaveTerminates == <>(pc["leave"] = "Done")
    \* L2: a drain gets the gate's lock: the change holding it returns
    DrainGetsLock ==
        \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"
end define;

\* configureServiceWhenCertified and configureServiceBackend(true) for svc,
\* without a certificate wait (Task 3), by the supervisor's start
\* (serviceBackend.start, which asks for retryLater if it fails) or by the
\* retry.
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
    \* if b.d.isDrained() { return errDrained }; runAdvertise: addAdvertised:
    \* GetPrefs, then EditPrefs unless the Service is listed
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
        addAfterDrained := addAfterDrained \/ lockedDrained[self];
    end if;
Advertise_sent:
    \* addAdvertised: EditPrefs returns, or fails (refused, cancelled,
    \* tailscaled gone); a request that did not return stays pending
    either
        await Applied(self);
        waitOn[self] := NoReq;
    or
        await ~CallOK(self) \/ refused[self];
        waitOn[self] := NoReq;
        refused[self] := FALSE;
        res[self] := "err";
        goto Change_fnReturned;
    end either;
Advertise_added:
    \* runAdvertise: gate.advertised.Store(true) (markAdvertised and the
    \* confirmer: Task 3); fn returns nil
    advertised := TRUE;
Change_fnReturned:
    \* change: fn returned; if it failed: if g.drained.Load() { return
    \* errDrained }, which ignoreDrained makes nil; setCancel(nil),
    \* g.mu.Unlock(). serviceBackend.start: retryLater on an error; a retry
    \* polls again, or ends
    lock := None;
    cancelReg := None;
    cctx[self] := FALSE;
    lockedDrained[self] := FALSE;
    if res[self] = "err" /\ ~(drained /\ Checks) then
        if self = "sup" then
            retryGen[svc] := gen[self];
        end if;
    elsif self = "retry" then
        retryGen[svc] := 0;
    end if;
    res[self] := "ok";
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
                 external |-> FALSE];
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
Start_backends:
        \* configureServing: serviceBackend.start for each backend in turn;
        \* then startDaemon: s.daemon = d
        while todo # {} do
            cur := CHOOSE s \in todo : TRUE;
            todo := todo \ {cur};
            call Configure(cur);
        end while;
        supDaemon := gen["sup"];
        published := TRUE;
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

\* The stop signal: Endpoint.DrainService, which reaches the supervisor only
\* once startSupervisor returned (the first daemon was published), or none
fair process StopSignal = "stop"
begin
Stop_signal:
    either
        await published;
        call DrainProc();
    or
        skip;
    end either;
end process;

\* The endpoint leaves: DrainAndWait, then the supervisor's Stop
fair process Leave = "leave"
begin
Leave_begin:
    \* Endpoint.stopTailscale: with the supervisor, DrainAndWait; during the
    \* first start (no supervisor yet) RunTailscale aborts the start, with
    \* no drain
    if published then
        call DrainProc();
    else
        leaveEnd := "aborted";
        goto Leave_done;
    end if;
Wait_lock:
    \* DrainAndWait: g.mu.Lock(); drain := g.drain; g.mu.Unlock()
    await lock = None;
    leaveRec := drainRec;
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
        end with;
Fetch_counted:
        \* handleLine: d.fetches++ done, next closed
        skip;
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

end algorithm; *)
\* BEGIN TRANSLATION
CONSTANT defaultInitValue
VARIABLES stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending, 
          waitOn, refused, dirty, fetchQ, fetchCount, supDaemon, stopped, 
          retryGen, drained, advertised, lock, cancelReg, cctx, drainRec, gen, 
          res, cnt, leaveRec, leaveD, after, seen, leaveEnd, everInPrefs, 
          lockedDrained, addAfterDrained, startedDrained, drainApplied, 
          ackMade, ackCounted, ackTimedOut, published, leftAdvertising, pc, 
          stack

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



Reads(g) ==
    \/ pc["leave"] \in {"Wait_check", "Wait_block"} /\ leaveD = g
    \/ \E p \in Drainers : pc[p] = "Drain_marked" /\ supDaemon = g



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

AckEndsWait == ~ackTimedOut

LeaveTerminates == <>(pc["leave"] = "Done")

DrainGetsLock ==
    \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"

VARIABLES svc, todo, cur

vars == << stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending, 
           waitOn, refused, dirty, fetchQ, fetchCount, supDaemon, stopped, 
           retryGen, drained, advertised, lock, cancelReg, cctx, drainRec, 
           gen, res, cnt, leaveRec, leaveD, after, seen, leaveEnd, 
           everInPrefs, lockedDrained, addAfterDrained, startedDrained, 
           drainApplied, ackMade, ackCounted, ackTimedOut, published, 
           leftAdvertising, pc, stack, svc, todo, cur >>

ProcSet == {"sup"} \cup {"retry"} \cup {"stop"} \cup {"leave"} \cup {"tailscaled"} \cup {"control"} \cup {"counter"} \cup {"crash"}

Init == (* Global variables *)
        /\ stateExists \in BOOLEAN
        /\ prefs \in IF stateExists THEN {{}, Services} ELSE {{}}
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
        /\ supDaemon = 0
        /\ stopped = FALSE
        /\ retryGen = [s \in Services |-> 0]
        /\ drained = FALSE
        /\ advertised = FALSE
        /\ lock = None
        /\ cancelReg = None
        /\ cctx = [a \in Actors |-> FALSE]
        /\ drainRec = NoRec
        /\ gen = [a \in Actors |-> 0]
        /\ res = [a \in Actors |-> "ok"]
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
        (* Procedure Configure *)
        /\ svc = [ self \in ProcSet |-> defaultInitValue]
        (* Process Supervisor *)
        /\ todo = {}
        /\ cur = None
        /\ stack = [self \in ProcSet |-> << >>]
        /\ pc = [self \in ProcSet |-> CASE self = "sup" -> "Sup_loop"
                                        [] self = "retry" -> "Retry_poll"
                                        [] self = "stop" -> "Stop_signal"
                                        [] self = "leave" -> "Leave_begin"
                                        [] self = "tailscaled" -> "Tailscaled_answer"
                                        [] self = "control" -> "Control_fetch"
                                        [] self = "counter" -> "Counter_count"
                                        [] self = "crash" -> "Crash_exit"]

Change_lock(self) == /\ pc[self] = "Change_lock"
                     /\ lock = None
                     /\ IF drained /\ Checks
                           THEN /\ IF self = "retry"
                                      THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = 0]
                                      ELSE /\ TRUE
                                           /\ UNCHANGED retryGen
                                /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                /\ svc' = [svc EXCEPT ![self] = Head(stack[self]).svc]
                                /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                /\ UNCHANGED << lock, cancelReg, lockedDrained >>
                           ELSE /\ lock' = self
                                /\ cancelReg' = self
                                /\ lockedDrained' = [lockedDrained EXCEPT ![self] = drained]
                                /\ pc' = [pc EXCEPT ![self] = "Change_locked"]
                                /\ UNCHANGED << retryGen, stack, svc >>
                     /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                     ctxDone, crashes, pending, waitOn, 
                                     refused, dirty, fetchQ, fetchCount, 
                                     supDaemon, stopped, drained, advertised, 
                                     cctx, drainRec, gen, res, cnt, leaveRec, 
                                     leaveD, after, seen, leaveEnd, 
                                     everInPrefs, addAfterDrained, 
                                     startedDrained, drainApplied, ackMade, 
                                     ackCounted, ackTimedOut, published, 
                                     leftAdvertising, todo, cur >>

Change_locked(self) == /\ pc[self] = "Change_locked"
                       /\ IF drained /\ Checks
                             THEN /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                  /\ UNCHANGED << pending, waitOn, advertised, 
                                                  res, addAfterDrained >>
                             ELSE /\ IF ~CallOK(self)
                                        THEN /\ res' = [res EXCEPT ![self] = "err"]
                                             /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                             /\ UNCHANGED << pending, waitOn, 
                                                             advertised, 
                                                             addAfterDrained >>
                                        ELSE /\ IF svc[self] \in prefs
                                                   THEN /\ advertised' = TRUE
                                                        /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                                        /\ UNCHANGED << pending, 
                                                                        waitOn, 
                                                                        addAfterDrained >>
                                                   ELSE /\ LET r == [from |-> self, gen |-> gen[self],
                                                                     val |-> prefs \cup {svc[self]}, kind |-> "add"] IN
                                                             /\ pending' = (pending \cup {r})
                                                             /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                        /\ addAfterDrained' = (addAfterDrained \/ lockedDrained[self])
                                                        /\ pc' = [pc EXCEPT ![self] = "Advertise_sent"]
                                                        /\ UNCHANGED advertised
                                             /\ res' = res
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, refused, dirty, 
                                       fetchQ, fetchCount, supDaemon, stopped, 
                                       retryGen, drained, lock, cancelReg, 
                                       cctx, drainRec, gen, cnt, leaveRec, 
                                       leaveD, after, seen, leaveEnd, 
                                       everInPrefs, lockedDrained, 
                                       startedDrained, drainApplied, ackMade, 
                                       ackCounted, ackTimedOut, published, 
                                       leftAdvertising, stack, svc, todo, cur >>

Advertise_sent(self) == /\ pc[self] = "Advertise_sent"
                        /\ \/ /\ Applied(self)
                              /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                              /\ pc' = [pc EXCEPT ![self] = "Advertise_added"]
                              /\ UNCHANGED <<refused, res>>
                           \/ /\ ~CallOK(self) \/ refused[self]
                              /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                              /\ refused' = [refused EXCEPT ![self] = FALSE]
                              /\ res' = [res EXCEPT ![self] = "err"]
                              /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                        /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                        daemonUp, ctxDone, crashes, pending, 
                                        dirty, fetchQ, fetchCount, supDaemon, 
                                        stopped, retryGen, drained, advertised, 
                                        lock, cancelReg, cctx, drainRec, gen, 
                                        cnt, leaveRec, leaveD, after, seen, 
                                        leaveEnd, everInPrefs, lockedDrained, 
                                        addAfterDrained, startedDrained, 
                                        drainApplied, ackMade, ackCounted, 
                                        ackTimedOut, published, 
                                        leftAdvertising, stack, svc, todo, cur >>

Advertise_added(self) == /\ pc[self] = "Advertise_added"
                         /\ advertised' = TRUE
                         /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                         /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                         daemonUp, ctxDone, crashes, pending, 
                                         waitOn, refused, dirty, fetchQ, 
                                         fetchCount, supDaemon, stopped, 
                                         retryGen, drained, lock, cancelReg, 
                                         cctx, drainRec, gen, res, cnt, 
                                         leaveRec, leaveD, after, seen, 
                                         leaveEnd, everInPrefs, lockedDrained, 
                                         addAfterDrained, startedDrained, 
                                         drainApplied, ackMade, ackCounted, 
                                         ackTimedOut, published, 
                                         leftAdvertising, stack, svc, todo, 
                                         cur >>

Change_fnReturned(self) == /\ pc[self] = "Change_fnReturned"
                           /\ lock' = None
                           /\ cancelReg' = None
                           /\ cctx' = [cctx EXCEPT ![self] = FALSE]
                           /\ lockedDrained' = [lockedDrained EXCEPT ![self] = FALSE]
                           /\ IF res[self] = "err" /\ ~(drained /\ Checks)
                                 THEN /\ IF self = "sup"
                                            THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = gen[self]]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED retryGen
                                 ELSE /\ IF self = "retry"
                                            THEN /\ retryGen' = [retryGen EXCEPT ![svc[self]] = 0]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED retryGen
                           /\ res' = [res EXCEPT ![self] = "ok"]
                           /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                           /\ svc' = [svc EXCEPT ![self] = Head(stack[self]).svc]
                           /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, pending, 
                                           waitOn, refused, dirty, fetchQ, 
                                           fetchCount, supDaemon, stopped, 
                                           drained, advertised, drainRec, gen, 
                                           cnt, leaveRec, leaveD, after, seen, 
                                           leaveEnd, everInPrefs, 
                                           addAfterDrained, startedDrained, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, todo, cur >>

Configure(self) == Change_lock(self) \/ Change_locked(self)
                      \/ Advertise_sent(self) \/ Advertise_added(self)
                      \/ Change_fnReturned(self)

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
                                     supDaemon, stopped, retryGen, advertised, 
                                     lock, cancelReg, drainRec, gen, res, cnt, 
                                     leaveRec, leaveD, after, seen, leaveEnd, 
                                     everInPrefs, lockedDrained, 
                                     addAfterDrained, startedDrained, 
                                     drainApplied, ackMade, ackCounted, 
                                     ackTimedOut, published, leftAdvertising, 
                                     stack, svc, todo, cur >>

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
                                      supDaemon, stopped, retryGen, drained, 
                                      advertised, cancelReg, drainRec, res, 
                                      leaveRec, leaveD, after, seen, leaveEnd, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      svc, todo, cur >>

Drain_countRead(self) == /\ pc[self] = "Drain_countRead"
                         /\ IF ~Live(self)
                               THEN /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                    /\ UNCHANGED << pending, waitOn, drainRec >>
                               ELSE /\ IF prefs = {}
                                          THEN /\ drainRec' = [set |-> TRUE, gen |-> gen[self], fetches |-> 0,
                                                               external |-> TRUE]
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                               /\ UNCHANGED << pending, waitOn >>
                                          ELSE /\ LET r == [from |-> self, gen |-> gen[self], val |-> {},
                                                            kind |-> "drain"] IN
                                                    /\ pending' = (pending \cup {r})
                                                    /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_sent"]
                                               /\ UNCHANGED drainRec
                         /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                         daemonUp, ctxDone, crashes, refused, 
                                         dirty, fetchQ, fetchCount, supDaemon, 
                                         stopped, retryGen, drained, 
                                         advertised, lock, cancelReg, cctx, 
                                         gen, res, cnt, leaveRec, leaveD, 
                                         after, seen, leaveEnd, everInPrefs, 
                                         lockedDrained, addAfterDrained, 
                                         startedDrained, drainApplied, ackMade, 
                                         ackCounted, ackTimedOut, published, 
                                         leftAdvertising, stack, svc, todo, 
                                         cur >>

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
                                    fetchQ, fetchCount, supDaemon, stopped, 
                                    retryGen, drained, advertised, lock, 
                                    cancelReg, cctx, drainRec, gen, res, cnt, 
                                    leaveRec, leaveD, after, seen, leaveEnd, 
                                    everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    drainApplied, ackMade, ackCounted, 
                                    ackTimedOut, published, leftAdvertising, 
                                    stack, svc, todo, cur >>

Drain_edited(self) == /\ pc[self] = "Drain_edited"
                      /\ drainRec' = [set |-> TRUE, gen |-> gen[self], fetches |-> cnt[self],
                                      external |-> FALSE]
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
                                      supDaemon, stopped, retryGen, drained, 
                                      advertised, cancelReg, cctx, res, 
                                      leaveRec, leaveD, after, seen, leaveEnd, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      drainApplied, ackMade, ackCounted, 
                                      ackTimedOut, published, leftAdvertising, 
                                      svc, todo, cur >>

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
                                       supDaemon, stopped, retryGen, drained, 
                                       advertised, cancelReg, cctx, drainRec, 
                                       res, leaveRec, leaveD, after, seen, 
                                       leaveEnd, everInPrefs, lockedDrained, 
                                       addAfterDrained, startedDrained, 
                                       drainApplied, ackMade, ackCounted, 
                                       ackTimedOut, published, leftAdvertising, 
                                       svc, todo, cur >>

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
                       /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                  ELSE /\ pc' = [pc EXCEPT !["sup"] = "Done"]
                       /\ UNCHANGED << stateExists, daemonGen, daemonUp, dirty, 
                                       advertised, gen, startedDrained, todo >>
            /\ UNCHANGED << prefs, ctxDone, crashes, pending, waitOn, refused, 
                            fetchQ, fetchCount, supDaemon, stopped, retryGen, 
                            drained, lock, cancelReg, cctx, drainRec, res, cnt, 
                            leaveRec, leaveD, after, seen, leaveEnd, 
                            everInPrefs, lockedDrained, addAfterDrained, 
                            drainApplied, ackMade, ackCounted, ackTimedOut, 
                            published, leftAdvertising, stack, svc, cur >>

Start_backends == /\ pc["sup"] = "Start_backends"
                  /\ IF todo # {}
                        THEN /\ cur' = (CHOOSE s \in todo : TRUE)
                             /\ todo' = todo \ {cur'}
                             /\ /\ stack' = [stack EXCEPT !["sup"] = << [ procedure |->  "Configure",
                                                                          pc        |->  "Start_backends",
                                                                          svc       |->  svc["sup"] ] >>
                                                                      \o stack["sup"]]
                                /\ svc' = [svc EXCEPT !["sup"] = cur']
                             /\ pc' = [pc EXCEPT !["sup"] = "Change_lock"]
                             /\ UNCHANGED << supDaemon, published >>
                        ELSE /\ supDaemon' = gen["sup"]
                             /\ published' = TRUE
                             /\ cur' = None
                             /\ pc' = [pc EXCEPT !["sup"] = "Daemon_started"]
                             /\ UNCHANGED << stack, svc, todo >>
                  /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                  ctxDone, crashes, pending, waitOn, refused, 
                                  dirty, fetchQ, fetchCount, stopped, retryGen, 
                                  drained, advertised, lock, cancelReg, cctx, 
                                  drainRec, gen, res, cnt, leaveRec, leaveD, 
                                  after, seen, leaveEnd, everInPrefs, 
                                  lockedDrained, addAfterDrained, 
                                  startedDrained, drainApplied, ackMade, 
                                  ackCounted, ackTimedOut, leftAdvertising >>

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
                                  fetchQ, fetchCount, stopped, retryGen, 
                                  drained, advertised, lock, cancelReg, cctx, 
                                  drainRec, gen, res, cnt, leaveRec, leaveD, 
                                  after, seen, leaveEnd, everInPrefs, 
                                  lockedDrained, addAfterDrained, 
                                  startedDrained, drainApplied, ackMade, 
                                  ackCounted, ackTimedOut, published, 
                                  leftAdvertising, stack, svc, todo, cur >>

Supervisor == Sup_loop \/ Start_backends \/ Daemon_started

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
                              fetchCount, supDaemon, stopped, drained, 
                              advertised, lock, cancelReg, cctx, drainRec, res, 
                              cnt, leaveRec, leaveD, after, seen, leaveEnd, 
                              everInPrefs, lockedDrained, addAfterDrained, 
                              startedDrained, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, 
                              leftAdvertising, todo, cur >>

Retry == Retry_poll

Stop_signal == /\ pc["stop"] = "Stop_signal"
               /\ \/ /\ published
                     /\ stack' = [stack EXCEPT !["stop"] = << [ procedure |->  "DrainProc",
                                                                pc        |->  "Done" ] >>
                                                            \o stack["stop"]]
                     /\ pc' = [pc EXCEPT !["stop"] = "Drain_begin"]
                  \/ /\ TRUE
                     /\ pc' = [pc EXCEPT !["stop"] = "Done"]
                     /\ stack' = stack
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, fetchQ, fetchCount, supDaemon, stopped, 
                               retryGen, drained, advertised, lock, cancelReg, 
                               cctx, drainRec, gen, res, cnt, leaveRec, leaveD, 
                               after, seen, leaveEnd, everInPrefs, 
                               lockedDrained, addAfterDrained, startedDrained, 
                               drainApplied, ackMade, ackCounted, ackTimedOut, 
                               published, leftAdvertising, svc, todo, cur >>

StopSignal == Stop_signal

Leave_begin == /\ pc["leave"] = "Leave_begin"
               /\ IF published
                     THEN /\ stack' = [stack EXCEPT !["leave"] = << [ procedure |->  "DrainProc",
                                                                      pc        |->  "Wait_lock" ] >>
                                                                  \o stack["leave"]]
                          /\ pc' = [pc EXCEPT !["leave"] = "Drain_begin"]
                          /\ UNCHANGED leaveEnd
                     ELSE /\ leaveEnd' = "aborted"
                          /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                          /\ stack' = stack
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, fetchQ, fetchCount, supDaemon, stopped, 
                               retryGen, drained, advertised, lock, cancelReg, 
                               cctx, drainRec, gen, res, cnt, leaveRec, leaveD, 
                               after, seen, everInPrefs, lockedDrained, 
                               addAfterDrained, startedDrained, drainApplied, 
                               ackMade, ackCounted, ackTimedOut, published, 
                               leftAdvertising, svc, todo, cur >>

Wait_lock == /\ pc["leave"] = "Wait_lock"
             /\ lock = None
             /\ leaveRec' = drainRec
             /\ pc' = [pc EXCEPT !["leave"] = "Wait_recordRead"]
             /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                             crashes, pending, waitOn, refused, dirty, fetchQ, 
                             fetchCount, supDaemon, stopped, retryGen, drained, 
                             advertised, lock, cancelReg, cctx, drainRec, gen, 
                             res, cnt, leaveD, after, seen, leaveEnd, 
                             everInPrefs, lockedDrained, addAfterDrained, 
                             startedDrained, drainApplied, ackMade, ackCounted, 
                             ackTimedOut, published, leftAdvertising, stack, 
                             svc, todo, cur >>

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
                                   dirty, fetchQ, fetchCount, supDaemon, 
                                   stopped, retryGen, drained, advertised, 
                                   lock, cancelReg, cctx, drainRec, gen, res, 
                                   cnt, leaveRec, seen, everInPrefs, 
                                   lockedDrained, addAfterDrained, 
                                   startedDrained, drainApplied, ackMade, 
                                   ackCounted, ackTimedOut, published, 
                                   leftAdvertising, stack, svc, todo, cur >>

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
                              fetchCount, supDaemon, stopped, retryGen, 
                              drained, advertised, lock, cancelReg, cctx, 
                              drainRec, gen, res, cnt, leaveRec, leaveD, after, 
                              everInPrefs, lockedDrained, addAfterDrained, 
                              startedDrained, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, 
                              leftAdvertising, stack, svc, todo, cur >>

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
                              fetchCount, supDaemon, stopped, retryGen, 
                              drained, advertised, lock, cancelReg, cctx, 
                              drainRec, gen, res, cnt, leaveRec, leaveD, after, 
                              seen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, published, leftAdvertising, 
                              stack, svc, todo, cur >>

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
                              refused, dirty, fetchQ, fetchCount, retryGen, 
                              drained, advertised, lock, cancelReg, cctx, 
                              drainRec, gen, res, cnt, leaveRec, leaveD, after, 
                              seen, leaveEnd, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              stack, svc, todo, cur >>

Leave == Leave_begin \/ Wait_lock \/ Wait_recordRead \/ Wait_check
            \/ Wait_block \/ Leave_done

Tailscaled_answer == /\ pc["tailscaled"] = "Tailscaled_answer"
                     /\ IF ~stopped
                           THEN /\ \/ /\ \E r \in {q \in pending : daemonUp /\ q.gen = daemonGen}:
                                           /\ IF r.val # prefs
                                                 THEN /\ dirty' = TRUE
                                                 ELSE /\ TRUE
                                                      /\ dirty' = dirty
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
                                      /\ UNCHANGED <<prefs, dirty, fetchQ, everInPrefs, drainApplied, ackMade, ackCounted>>
                                /\ pc' = [pc EXCEPT !["tailscaled"] = "Tailscaled_answer"]
                           ELSE /\ pc' = [pc EXCEPT !["tailscaled"] = "Done"]
                                /\ UNCHANGED << prefs, pending, refused, dirty, 
                                                fetchQ, everInPrefs, 
                                                drainApplied, ackMade, 
                                                ackCounted >>
                     /\ UNCHANGED << stateExists, daemonGen, daemonUp, ctxDone, 
                                     crashes, waitOn, fetchCount, supDaemon, 
                                     stopped, retryGen, drained, advertised, 
                                     lock, cancelReg, cctx, drainRec, gen, res, 
                                     cnt, leaveRec, leaveD, after, seen, 
                                     leaveEnd, lockedDrained, addAfterDrained, 
                                     startedDrained, ackTimedOut, published, 
                                     leftAdvertising, stack, svc, todo, cur >>

Tailscaled == Tailscaled_answer

Control_fetch == /\ pc["control"] = "Control_fetch"
                 /\ IF ~stopped
                       THEN /\ daemonUp /\ dirty
                            /\ dirty' = FALSE
                            /\ fetchQ' = [fetchQ EXCEPT ![daemonGen] = Append(fetchQ[daemonGen], Ack)]
                            /\ ackMade' = (ackMade \/ Ack)
                            /\ pc' = [pc EXCEPT !["control"] = "Control_fetch"]
                       ELSE /\ pc' = [pc EXCEPT !["control"] = "Done"]
                            /\ UNCHANGED << dirty, fetchQ, ackMade >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 fetchCount, supDaemon, stopped, retryGen, 
                                 drained, advertised, lock, cancelReg, cctx, 
                                 drainRec, gen, res, cnt, leaveRec, leaveD, 
                                 after, seen, leaveEnd, everInPrefs, 
                                 lockedDrained, addAfterDrained, 
                                 startedDrained, drainApplied, ackCounted, 
                                 ackTimedOut, published, leftAdvertising, 
                                 stack, svc, todo, cur >>

Control == Control_fetch

Counter_count == /\ pc["counter"] = "Counter_count"
                 /\ IF ~stopped
                       THEN /\ \E g \in {x \in Gens : fetchQ[x] # <<>> /\ Reads(x)}:
                                 /\ fetchCount' = [fetchCount EXCEPT ![g] = fetchCount[g] + 1]
                                 /\ ackCounted' = [ackCounted EXCEPT ![g] = ackCounted[g] \/ Head(fetchQ[g])]
                                 /\ fetchQ' = [fetchQ EXCEPT ![g] = Tail(fetchQ[g])]
                            /\ pc' = [pc EXCEPT !["counter"] = "Fetch_counted"]
                       ELSE /\ pc' = [pc EXCEPT !["counter"] = "Done"]
                            /\ UNCHANGED << fetchQ, fetchCount, ackCounted >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 dirty, supDaemon, stopped, retryGen, drained, 
                                 advertised, lock, cancelReg, cctx, drainRec, 
                                 gen, res, cnt, leaveRec, leaveD, after, seen, 
                                 leaveEnd, everInPrefs, lockedDrained, 
                                 addAfterDrained, startedDrained, drainApplied, 
                                 ackMade, ackTimedOut, published, 
                                 leftAdvertising, stack, svc, todo, cur >>

Fetch_counted == /\ pc["counter"] = "Fetch_counted"
                 /\ TRUE
                 /\ pc' = [pc EXCEPT !["counter"] = "Counter_count"]
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 dirty, fetchQ, fetchCount, supDaemon, stopped, 
                                 retryGen, drained, advertised, lock, 
                                 cancelReg, cctx, drainRec, gen, res, cnt, 
                                 leaveRec, leaveD, after, seen, leaveEnd, 
                                 everInPrefs, lockedDrained, addAfterDrained, 
                                 startedDrained, drainApplied, ackMade, 
                                 ackCounted, ackTimedOut, published, 
                                 leftAdvertising, stack, svc, todo, cur >>

Counter == Counter_count \/ Fetch_counted

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
                              refused, fetchQ, fetchCount, supDaemon, stopped, 
                              retryGen, drained, advertised, lock, cancelReg, 
                              cctx, drainRec, gen, res, cnt, leaveRec, leaveD, 
                              after, seen, leaveEnd, everInPrefs, 
                              lockedDrained, addAfterDrained, startedDrained, 
                              drainApplied, ackMade, ackCounted, ackTimedOut, 
                              published, leftAdvertising, stack, svc, todo, 
                              cur >>

Crash == Crash_exit

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == Supervisor \/ Retry \/ StopSignal \/ Leave \/ Tailscaled \/ Control
           \/ Counter \/ Crash
           \/ (\E self \in ProcSet: Configure(self) \/ DrainProc(self))
           \/ Terminating

Spec == /\ Init /\ [][Next]_vars
        /\ WF_vars(Supervisor) /\ WF_vars(Configure("sup"))
        /\ WF_vars(Retry) /\ WF_vars(Configure("retry"))
        /\ WF_vars(StopSignal) /\ WF_vars(DrainProc("stop"))
        /\ WF_vars(Leave) /\ WF_vars(DrainProc("leave"))

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION

\* Once Leave stopped the supervisor, nothing the properties read changes:
\* the cfgs explore no step after it (ACTION_CONSTRAINT Running), which
\* leaves out the other processes' ways to their end.
Running == ~stopped
====
