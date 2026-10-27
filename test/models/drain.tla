---- MODULE drain ----
\* Guards: G7
\*
\* The drain gate of a container's Service backends (pkg/tailscale/drain.go):
\* a drained backend is not advertised again, and a node leaves only once
\* control has fetched its Service list since tslink's last removal of a
\* Service took effect. The model describes the code as it is; it verifies
\* the design, not the Go code.
\*
\* Actors (PlusCal processes), each step naming the Go function it models:
\*   sup          DaemonSupervisor.supervisionLoop, startDaemon, daemon.Start
\*                (drainAtStart, configureServing: each backend's
\*                serviceBackend.start in turn), waitForExit, stopDaemon
\*   retry        serviceBackend.retryLater, of every backend
\*   stop         the stop signal: DaemonSupervisor.Drain (or none), once
\*                the endpoint has its supervisor
\*   leave        DaemonSupervisor.DrainAndWait, then the endpoint's Stop;
\*                during the first start, Leaving and the start's end
\*   redrain      daemon.drainAgain, started by an edit advertising a
\*                Service once drained
\*   tailscaled   applies a LocalAPI prefs edit, or refuses one adding a
\*                Service
\*   orphans      tailscaled drops a request a cancelled call left
\*   control      fetches the node's Service list after a change, or never
\*   counter      handleLine: tslink reads tailscaled's log in order, later:
\*                fetches and applied edits of the advertised Services
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
\*   floor are events, not durations. The deadline does not fire in the
\*   same instant as the wait's condition holds (Go's select would pick
\*   either): Wait_block times out only while it does not.
\* - The floor's sleep (Leave_floor, Wait_settle) outlasts what takes
\*   milliseconds (Settled, an environment assumption): a call a drain
\*   cancelled was carried out by then, if at all; tslink read the log line
\*   of any edit advertising a Service; the drainAgain it started recorded
\*   its removal.
\* - A change's calls have no timeout here: tailscaled may hold an edit
\*   until the change's context is cancelled, so liveness cannot rest on
\*   their 10 s timeouts (DrainGetsLock). The drain's own edit, on the
\*   daemon's context, times out: a fair step.
\* - A cancelled or timed-out call returns, and its request stays pending:
\*   tailscaled may apply it later, after other edits (the design's
\*   conservative assumption), or drop it (orphans). tailscaled's exit
\*   drops its pending requests (it may have applied them before).
\* - tailscaled's log is one queue per daemon, read in order: an applied
\*   edit of the advertised Services is logged in the step that applies it
\*   (editPrefsLocked logs it under the lock it sets the prefs with), a
\*   fetch in the step that reads them (handleC2NVIPServicesGet logs before
\*   it takes that lock).
\* - editServe is not modelled: it is taken to succeed at once. Failures
\*   that make the start retry are tailscaled refusing an add instead.
\* - GetPrefs and the EditPrefs built from it are one step. An edit applied
\*   between them in Go has the same outcome as one applied after the send,
\*   before ours: EditPrefs writes the whole list.
\* - Steps are merged where no other actor can tell them apart: the outer
\*   isDrained checks with change's lock, markDrained's store with its
\*   cancel, getDaemon with daemon.Drain's lock (see each).
\* - Control may fetch once after any change of the advertised Services and
\*   after tailscaled starts, later, or never. tslink reads a log line
\*   later, in the model only when an actor is to read its state or the
\*   line advertises a Service once drained (Reads, RelistLine): nothing
\*   else depends on it, so an earlier read is the same.
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
\*   the cfgs use VIEW CountView, which keeps of each count read only
\*   whether a fetch was counted since (see there).
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
\*   no-drained-check   no isDrained/drained check on the configure path
\*   always-wait        DrainAndWait waits without a removal to wait for
\*   off-by-one         DrainAndWait waits for a second fetch
\*   count-before-edit  the wait compares with a count read before the edit
\*   record-on-return   a removal is recorded only once its call returned
\*   drain-only-record  only drains record their removals
\*   no-follow-restart  DrainAndWait ends with its daemon
\*   no-drain-at-start  a daemon started drained keeps what its state
\*                      advertises
\*   no-redrain         an edit advertising a Service once drained stays
\*   abort-first-start  Leave during the first start aborts it
\*   no-settle          DrainAndWait does not look again after a cancel
\*   wait-no-timeout    the wait has no deadline
\*   no-cancel          markDrained cancels nothing
\*   unbounded-nudges   the confirmer does not stop after advertiseNudges
\*   drained-pruned     a prune the drain stopped counts as done
\*   no-start-record    a drain with nothing to remove leaves the record of
\*                      an earlier daemon
\*   no-listed-check    a removal counts as fetched though a Service was
\*                      advertised again since
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
Actors == Changers \cup Drainers \cup Confs \cup Leases \cup {"redrain"}
Gens == 1..(MaxRestarts + 1)        \* tailscaled processes, by start
NoReq == [from |-> None, gen |-> 0, val |-> {}, kind |-> None]
\* serviceGate.removal: the last edit of tslink's removing Services, sent
\* to daemon gen when it had logged removals removals, leaving leaves; or a
\* start record (start); fetches is the count then, which only the
\* count-before-edit mutation compares with
NoRm == [set |-> FALSE, gen |-> 0, removals |-> 0, leaves |-> {},
         start |-> FALSE, fetches |-> 0]
\* A line of tailscaled's log: a fetch (ack: of the drained list) or an
\* applied edit of the advertised Services (val)
FetchLine(a) == [k |-> "fetch", val |-> {}, ack |-> a]
EditLine(v) == [k |-> "edit", val |-> v, ack |-> FALSE]
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
    logQ = [g \in Gens |-> <<>>],   \* daemon g's log lines, not yet read
    \* handleLine, per daemon: d.fetches, d.removals, d.removedAt,
    \* d.removedTo, d.listed and d.listKnown, d.relisted; and the
    \* drainAgain it starts
    fetchCount = [g \in Gens |-> 0],
    removals = [g \in Gens |-> 0],
    removedAt = [g \in Gens |-> 0],
    removedTo = [g \in Gens |-> {}],
    listed = [g \in Gens |-> {}],
    listKnown = [g \in Gens |-> FALSE],
    relisted = [g \in Gens |-> FALSE],
    redrainReq = [g \in Gens |-> FALSE],
    certIssued = [s \in Services |-> FALSE], \* s's certificate exists
    \* the supervisor
    supDaemon = 0,                  \* s.daemon; 0 is nil
    stopped = FALSE,                \* the endpoint stopped the supervisor
    leaving = FALSE,                \* s.leaving: Leave during the first start
    retryGen = [s \in Services |-> 0], \* retryLater runs, for daemon g
    confReq = [s \in Services |-> NoConf], \* confirmAdvertisedLater(s) asked
    leaseReq = [s \in Services |-> 0], \* configureServiceAfterCert(s) asked
    \* the gate (serviceGate)
    drained = FALSE,
    lock = None,                    \* g.mu's holder
    cancelReg = None,               \* g.cancel: whose change it cancels
    cctx = [a \in Actors |-> FALSE], \* a's change context is cancelled
    cancelled = FALSE,              \* g.cancelledAt: a drain cancelled one
    rmRec = NoRm,                   \* g.removal
    unpruned = FALSE,               \* g.unpruned
    \* per actor
    gen = [a \in Actors |-> 0],     \* the daemon a works on (b.d, d)
    res = [a \in Actors |-> "ok"],  \* Configure's fn: ok, added, err
    out = [a \in Actors |-> "ok"],  \* what Configure returned to a lease
                                    \* holder: ok, err, drained
    mark = [a \in Actors |-> 0],    \* fetches read by runAdvertise
    \* DrainAndWait: the record it waits on, and that record's daemon
    lrec = NoRm, leaveD = 0, leaveEnd = None, settled = FALSE,
    drainedGen = 0,                 \* drainWait.drained: the daemon it drained
    \* history, for the properties
    everInPrefs = (prefs # {}),     \* a Service was ever in the prefs
    lockedDrained = [a \in Actors |-> FALSE], \* a's change began drained
    addAfterDrained = FALSE,        \* such a change sent an add
    startedDrained = FALSE,         \* the running daemon started drained
    startDrainFailed = [g \in Gens |-> FALSE], \* its drainAtStart failed
    drainApplied = FALSE,           \* daemon.Drain's edit took effect
    \* control fetched the drained list since (none if advertised again)
    ackMade = FALSE,
    ackCounted = [g \in Gens |-> FALSE], \* tslink counted such a fetch
    ackTimedOut = FALSE,            \* Leave timed out with one counted
    published = FALSE,              \* the first daemon was published
    leftAdvertising = FALSE,        \* Leave stopped a daemon advertising
    upAtLeave = FALSE,              \* tailscaled ran when Leave stopped it
    pruneApplied = FALSE,           \* a prune's edit took effect
    pruneFetched = FALSE,           \* control fetched the list since
    \* actors whose edit removed a Service of the container since control
    \* last fetched the list
    unfetched = {};

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
    \* NeedsDrain: the container names a Service, a prune failed, or tslink
    \* removed one. It takes g.mu to read g.removal: NeedsDrainRead is when
    \* that read can happen
    NeedsDrain == Services # {} \/ unpruned \/ rmRec.set
    NeedsDrainRead == Services # {} \/ unpruned \/ lock = None
    \* Whether actor a's edit removing Services records itself in the gate
    \* before it is sent (editAdvertised); the mutations record only a
    \* drain's, or only once the call returned
    RecordsAtSend(a) ==
        /\ Mutation # "record-on-return"
        /\ Mutation = "drain-only-record" => a \in Drainers \cup {"redrain", "sup"}
    \* removalFetched: control fetched the Service list after rec's edit
    \* took effect, nothing being advertised again since, on daemon g
    RemovalFetched(rec, g) ==
        IF Mutation = "count-before-edit"
        THEN fetchCount[g] > rec.fetches
        ELSE /\ \/ rec.start
                \/ removals[g] > rec.removals /\ removedTo[g] \subseteq rec.leaves
             /\ ~relisted[g]
             /\ Mutation # "no-listed-check" => listed[g] \subseteq rec.leaves
             /\ fetchCount[g] > removedAt[g] + (IF Mutation = "off-by-one" THEN 1 ELSE 0)
    \* The labels at which an actor reads daemon g's log state to record a
    \* removal (noteRemoval reads d.removals)
    SendLabels == {"Drain_countRead", "Prune_begin", "Readvertise_begin",
                   "Lease_unadvertise", "Start_drain", "Redrain_lock"}
    \* An actor reads daemon g's log state in its next step. Reading a line
    \* changes nothing else, but for an edit advertising a Service once
    \* drained (RelistLine), so the model reads the others only then: an
    \* earlier read is the same as one just before.
    Reads(g) ==
        \/ pc["leave"] \in {"Wait_block", "Leave_floor"} /\ leaveD = g
        \/ \E p \in Actors : pc[p] \in SendLabels /\ gen[p] = g
        \* runAdvertise's read, the confirmer's after
        \/ Confirmer /\ \E p \in Changers \cup Leases :
               pc[p] = "Change_locked" /\ gen[p] = g
        \* waitServicesFetch and the next after, of a confirmer
        \/ \E c \in Confs : pc[c] = "Confirm_wait" /\ gen[c] = g
    RelistLine(g) ==
        drained /\ \E i \in DOMAIN logQ[g] :
                       logQ[g][i].k = "edit" /\ logQ[g][i].val # {}
    \* s.running(): a daemon is published, and its tailscaled runs
    PublishedRuns == supDaemon # 0 /\ daemonUp /\ supDaemon = daemonGen
    \* A request a call returned without, which tailscaled may still carry
    \* out: one the drain's cancel left behind
    Orphan(r) == waitOn[r.from] # r
    \* What the floor's sleep settles (environment assumption): a call the
    \* drain cancelled was carried out, if at all, tslink read the log line
    \* of any edit advertising a Service, also of a tailscaled that stopped
    \* (its output is read to the end), and the drainAgain it started
    \* recorded its removal
    Settled ==
        /\ \A r \in pending : r.kind = "add" => ~Orphan(r)
        /\ \A g \in Gens : ~RelistLine(g)
        /\ \A g \in Gens : ~redrainReq[g]
        /\ pc["redrain"] # "Redrain_lock"

    \* S1: an edit advertising a Service of a running daemon once drained,
    \* as a cancelled call carried out late, is undone: once read, a drain
    \* again is requested, runs, or its edit is pending
    ReadvertisedDrainedAgain ==
        \A g \in Gens :
            relisted[g] /\ ~ctxDone[g] /\ logQ[g] = <<>>
            /\ daemonUp /\ g = daemonGen
            => \/ redrainReq[g] \/ pc["redrain"] # "Redrain_idle" \/ prefs = {}
               \/ \E r \in pending : r.gen = g /\ r.val = {}
    \* S2: no change that began once drained was set adds a Service
    DrainedFlagHoldsBack == ~addAfterDrained
    \* S2b: a daemon started once drained was set is published without the
    \* Services, unless removing them failed or is still in flight
    NoDaemonAdvertisesDrained ==
        daemonUp /\ startedDrained /\ supDaemon = daemonGen
        => \/ prefs = {}
           \/ startDrainFailed[daemonGen]
           \/ \E r \in pending : r.gen = daemonGen /\ r.val = {}
    \* S3: Leave returns before its timeout only after control fetched the
    \* Service list since every removal of tslink's that took effect: the
    \* drain's (ackMade), a prune's (pruneFetched), any other (unfetched).
    \* leaveEnd is how DrainAndWait returned: acked (a fetch after the
    \* last removal), timeout, nothing (no removal to wait for), stopped
    \* (its daemon gone, not followed: no-follow-restart), or aborted, a
    \* Leave during the first start with no tailscaled running. A Leave
    \* that stops no running tailscaled is not judged: nothing would
    \* answer control's fetch
    LeaveWaitsForDrain ==
        pc["leave"] = "Done" /\ leaveEnd \notin {"timeout", "aborted"}
        /\ upAtLeave
        => /\ drainApplied => ackMade
           /\ pruneApplied => pruneFetched
           /\ unfetched = {}
    \* S8: Leave stops tailscaled only once it advertises no Service, or
    \* after its wait timed out
    NoLeaveWhileAdvertising == ~leftAdvertising
    \* S4: no wait for a fetch if no Service was ever in the prefs
    NoWaitForNothing ==
        pc["leave"] = "Wait_block" => everInPrefs
    \* S7: a counted fetch of the drained list ends the wait before its
    \* timeout
    AckEndsWait == ~ackTimedOut
    \* L1: DrainAndWait returns, also if control never fetches
    LeaveTerminates == <>(pc["leave"] = "Done")
    \* L2: a drain gets the gate's lock: the change holding it returns
    DrainGetsLock ==
        \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"
end define;

\* editAdvertised: serviceGate.noteRemoval(d, kept), before the EditPrefs
\* removing Services is sent to daemon g, leaving leaves
macro NoteRemoval(g, leaves) begin
    rmRec := [set |-> TRUE, gen |-> g, removals |-> removals[g],
              leaves |-> leaves, start |-> FALSE, fetches |-> fetchCount[g]];
end macro;

\* The endpoint stops the supervisor once DrainAndWait returned: tailscaled
\* stops, and its pending requests with it
macro StopSupervisor() begin
    leftAdvertising := daemonUp /\ prefs # {} /\ leaveEnd # "timeout";
    upAtLeave := daemonUp;
    stopped := TRUE;
    daemonUp := FALSE;
    supDaemon := 0;
    pending := {};
    ctxDone := [g \in Gens |-> TRUE];
end macro;

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
        \* runAdvertise: not added
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
    \* On the add path first runAdvertise: markAdvertised (not modelled:
    \* nothing here reads fetchMark) and confirmAdvertisedLater(svc,
    \* fetches)
    if res[self] = "added" then
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
        cancelled := TRUE;
    end if;
Drain_marked:
    \* s.getDaemon(): nil is "tailscaled is not running"; else daemon.Drain:
    \* markDrained again, g.mu.Lock(). Abstraction: getDaemon is read once
    \* the lock is free. In Go a daemon replaced while the drainer waits for
    \* the lock is drained as the stale d, which fails (its context is done);
    \* here the drainer drains the new daemon, which drained itself as it
    \* started (drainAtStart): no property sees the difference.
    if self = "leave" then
        drainedGen := supDaemon;
    end if;
    if supDaemon = 0 then
        return;
    else
        await lock = None;
        lock := self;
        gen[self] := supDaemon;
        if cancelReg # None /\ Cancels then
            cctx[cancelReg] := TRUE;
            cancelled := TRUE;
        end if;
    end if;
Drain_countRead:
    \* unadvertiseAll: editAdvertised(d.ctx, keep nothing): GetPrefs;
    \* nothing to remove is left as it is, with a start record if an
    \* earlier daemon's removal is in the gate; else noteRemoval, EditPrefs
    if ~Live(self) then
        goto Drain_release;
    elsif prefs = {} then
        if rmRec.set /\ rmRec.gen # gen[self]
           /\ Mutation # "no-start-record" then
            rmRec := [rmRec EXCEPT !.gen = gen[self], !.start = TRUE];
        end if;
        goto Drain_release;
    else
        if RecordsAtSend(self) then
            NoteRemoval(gen[self], {});
        end if;
        with r = [from |-> self, gen |-> gen[self], val |-> {},
                  kind |-> "drain"] do
            pending := pending \cup {r};
            waitOn[self] := r;
        end with;
    end if;
Drain_sent:
    \* EditPrefs returns: applied, tailscaled gone, or its 10 s timeout
    \* (the request stays pending: tailscaled may carry it out)
    either
        await Applied(self);
        if Mutation = "record-on-return" then
            NoteRemoval(gen[self], {});
        end if;
    or
        await ~Live(self);
    or
        skip;
    end either;
    waitOn[self] := NoReq;
Drain_release:
    \* g.mu.Unlock() (deferred)
    if lock = self then
        lock := None;
    end if;
    gen[self] := 0;
    return;
end procedure;

\* supervisionLoop: start a daemon, wait for its exit, restart it
fair process Supervisor = "sup"
variables todo = {}, cur = None;
begin
Sup_loop:
    while ~stopped do
        \* startDaemon: newDaemon; daemon.Start: startProcess: tailscaled
        \* runs on the node's state, and control may fetch its Service
        \* list. (An assignment's new value is what later reads of the
        \* same step see.)
        daemonGen := daemonGen + 1;
        gen["sup"] := daemonGen;
        daemonUp := TRUE;
        stateExists := TRUE;
        startedDrained := drained;
        dirty := TRUE;
        todo := Services;
Start_drain:
        \* daemon.Start: drainAtStart, once the socket is up: if drained,
        \* g.mu.Lock(); editAdvertised(keep nothing), recorded; with nothing
        \* to remove, a start record if an earlier daemon's removal is in
        \* the gate
        if drained /\ Mutation # "no-drain-at-start" then
            await lock = None;
            if ~CallOK("sup") then
                startDrainFailed[gen["sup"]] := TRUE;
            elsif prefs = {} then
                if rmRec.set /\ rmRec.gen # gen["sup"] then
                    rmRec := [rmRec EXCEPT !.gen = gen["sup"], !.start = TRUE];
                end if;
            else
                lock := "sup";
                NoteRemoval(gen["sup"], {});
                with r = [from |-> "sup", gen |-> gen["sup"], val |-> {},
                          kind |-> "drain"] do
                    pending := pending \cup {r};
                    waitOn["sup"] := r;
                end with;
                goto Start_drainSent;
            end if;
        end if;
Start_next:
        if Prune then
            goto Prune_begin;
        else
            goto Start_backends;
        end if;
Start_drainSent:
        \* EditPrefs returns: applied, failed (tailscaled gone) or timed out
        \* (pending); g.mu.Unlock()
        either
            await Applied("sup");
        or
            await ~Live("sup");
            startDrainFailed[gen["sup"]] := TRUE;
        or
            startDrainFailed[gen["sup"]] := TRUE;
        end either;
        waitOn["sup"] := NoReq;
        lock := None;
        if ~Prune then
            goto Start_backends;
        end if;
Prune_begin:
        \* configureServing: pruneServices: change: g.mu.Lock(),
        \* setCancel, if g.drained { return errDrained }; fn: editServe
        \* (instant), editAdvertised(ctx, keep): GetPrefs, noteRemoval and
        \* EditPrefs unless nothing is to be removed. Then
        \* g.unpruned.Store(err != nil), errDrained included. (The lock and
        \* the send are one step: a drain between them makes the change
        \* return errDrained before the send, as a drain before the lock
        \* does.)
        await lock = None;
        if drained then
            unpruned := Mutation # "drained-pruned";
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
            if RecordsAtSend("sup") then
                NoteRemoval(gen["sup"], prefs \ {Stale});
            end if;
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
            if Mutation = "record-on-return" then
                NoteRemoval(gen["sup"], prefs);
            end if;
        or
            await ~CallOK("sup");
            res["sup"] := "err";
        or
            res["sup"] := "err";
        end either;
        waitOn["sup"] := NoReq;
Prune_edited:
        \* editAdvertised returned; change: if fn failed and g.drained {
        \* errDrained }; setCancel(nil), Unlock; g.unpruned.Store(err != nil)
        unpruned := res["sup"] = "err"
                    /\ (Mutation # "drained-pruned" \/ ~drained);
        lock := None;
        cancelReg := None;
        cctx["sup"] := FALSE;
        res["sup"] := "ok";
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
            if RecordsAtSend(self) then
                NoteRemoval(gen[self], prefs \ {SvcOf(self)});
            end if;
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
            if Mutation = "record-on-return" then
                NoteRemoval(gen[self], prefs);
            end if;
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
            if RecordsAtSend(self) then
                NoteRemoval(gen[self], prefs \ {SvcOf(self)});
            end if;
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
            if Mutation = "record-on-return" then
                NoteRemoval(gen[self], prefs);
            end if;
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
    \* Endpoint.Leave: with the supervisor, stopTailscale: DrainAndWait if
    \* NeedsDrain. During the first start (no supervisor stored yet) a
    \* tailscaled that runs makes the start go on drained
    \* (DaemonSupervisor.Leaving, abortStart), and Leave waits for it;
    \* with none running yet the start is aborted, in one step with its
    \* stop: a tailscaled the start runs later is killed by the cancelled
    \* context (abort-first-start: aborted whatever runs)
    await ~published \/ NeedsDrainRead;
    if ~published then
        if daemonUp /\ Mutation # "abort-first-start" then
            leaving := TRUE;
            drained := TRUE;
            if cancelReg # None /\ Cancels then
                cctx[cancelReg] := TRUE;
                cancelled := TRUE;
            end if;
        else
            leaveEnd := "aborted";
            StopSupervisor();
            goto Done;
        end if;
    elsif ~NeedsDrain then
        leaveEnd := "nodrain";
        goto Leave_done;
    else
        call DrainProc();
        goto Wait_loop;
    end if;
Leave_started:
    \* StartContext: the first start returns; drainIfLeaving: DrainAndWait
    \* if NeedsDrain. A start that failed (tailscaled exited) stops the
    \* supervisor
    await published \/ ~daemonUp;
    if ~published then
        leaveEnd := "aborted";
        StopSupervisor();
        goto Done;
    elsif ~NeedsDrain then
        leaveEnd := "nodrain";
        goto Leave_done;
    else
        call DrainProc();
    end if;
Wait_loop:
    \* DrainAndWait: g.lastRemoval(), under g.mu: a record of a live daemon
    \* is waited on; with tailscaled restarting (none published, or the one
    \* published exited) and a record or node state, the next daemon is
    \* waited for; else nothing is left to wait for
    await lock = None;
    lrec := rmRec;
    if rmRec.set /\ ~ctxDone[rmRec.gen] then
        leaveD := rmRec.gen;
    elsif Mutation = "always-wait" /\ PublishedRuns then
        leaveD := supDaemon;
    elsif ~PublishedRuns /\ (rmRec.set \/ stateExists)
          /\ Mutation # "no-follow-restart" then
        goto Wait_restart;
    elsif PublishedRuns /\ supDaemon # drainedGen then
        \* published since the drain, which did not reach it: drain it
        call DrainProc();
        goto Wait_loop;
    elsif cancelled /\ ~settled /\ Mutation # "no-settle" then
        goto Wait_settle;
    else
        leaveEnd := IF rmRec.set THEN "stopped" ELSE "nothing";
        goto Leave_done;
    end if;
Wait_block:
    \* waitRemovalFetched: control fetched the list after the record's edit
    \* took effect, the record changed, its daemon stopped, or the deadline
    either
        await RemovalFetched(lrec, leaveD);
    or
        await rmRec # lrec;
        goto Wait_loop;
    or
        await ctxDone[leaveD];
        if Mutation = "no-follow-restart" then
            leaveEnd := "stopped";
            goto Leave_done;
        else
            goto Wait_loop;
        end if;
    or
        await Mutation # "wait-no-timeout" /\ ~RemovalFetched(lrec, leaveD);
        leaveEnd := "timeout";
        ackTimedOut := ackCounted[leaveD];
        goto Leave_done;
    end either;
Leave_floor:
    \* waitFetched: the floor's sleep, after which a call the drain
    \* cancelled was carried out, if at all (environment assumption); then
    \* the record and its fetch are checked again
    await Settled;
    if rmRec = lrec /\ RemovalFetched(lrec, leaveD) then
        leaveEnd := "acked";
        goto Leave_done;
    else
        goto Wait_loop;
    end if;
Wait_settle:
    \* A call the drain cancelled may still be carried out: the floor's
    \* sleep, after which it was, if at all; then look again
    await Settled;
    settled := TRUE;
    goto Wait_loop;
Wait_restart:
    \* waitNextDaemon: a daemon published that runs, the record changed, or
    \* the deadline
    either
        await PublishedRuns \/ rmRec # lrec;
        goto Wait_loop;
    or
        await Mutation # "wait-no-timeout";
        leaveEnd := "timeout";
    end either;
Leave_done:
    \* DrainAndWait returns; the endpoint stops the supervisor
    StopSupervisor();
end process;

\* drainAgain: the drain again an edit advertising a Service once drained
\* starts (handleLine, noteAdvertisedEdit), on that edit's daemon
fair process Redrain = "redrain"
begin
Redrain_idle:
    while ~stopped do
        await \E g \in Gens : redrainReq[g];
        with g \in {x \in Gens : redrainReq[x]} do
            redrainReq[g] := FALSE;
            gen["redrain"] := g;
        end with;
Redrain_lock:
        \* g.mu.Lock(); unadvertiseAll: editAdvertised(d.ctx, keep
        \* nothing): GetPrefs, noteRemoval, EditPrefs unless nothing is left
        await lock = None;
        if ~Live("redrain") \/ prefs = {} then
            goto Redrain_idle;
        else
            lock := "redrain";
            NoteRemoval(gen["redrain"], {});
            with r = [from |-> "redrain", gen |-> gen["redrain"], val |-> {},
                      kind |-> "drain"] do
                pending := pending \cup {r};
                waitOn["redrain"] := r;
            end with;
        end if;
Redrain_sent:
        \* EditPrefs returns: applied, tailscaled gone, or timed out;
        \* g.mu.Unlock()
        either
            await Applied("redrain");
        or
            await ~Live("redrain");
        or
            skip;
        end either;
        waitOn["redrain"] := NoReq;
        lock := None;
    end while;
end process;

\* tailscaled applies a pending prefs edit, or refuses one. It logs an
\* edit that changes the advertised Services before it sets them
\* (editPrefsLocked)
process Tailscaled = "tailscaled"
begin
Tailscaled_answer:
    while ~stopped do
        either
            with r \in {q \in pending : daemonUp /\ q.gen = daemonGen} do
                if r.val # prefs then
                    dirty := TRUE;
                    logQ[r.gen] := Append(
                        IF r.val # {}
                        THEN [i \in DOMAIN logQ[r.gen] |->
                                [logQ[r.gen][i] EXCEPT !.ack = FALSE]]
                        ELSE logQ[r.gen],
                        EditLine(r.val));
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
                elsif r.kind = "drain" then
                    \* A drain that changed nothing, once control fetched
                    \* the list as it is, needs no fetch of its own
                    ackMade := ackMade \/ ~dirty;
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

\* A request the call returned without (Orphan) is carried out within the
\* drain's floor, if at all: tailscaled drops one it has not carried out
\* (environment assumption; Leave_floor relies on it)
fair process Orphans = "orphans"
begin
Orphans_drop:
    while ~stopped do
        await \E r \in pending : r.kind = "add" /\ Orphan(r);
        with r \in {q \in pending : q.kind = "add" /\ Orphan(q)} do
            pending := pending \ {r};
        end with;
    end while;
end process;

\* control fetches the node's Service list (c2n GET /vip-services), and
\* tailscaled logs it before it reads the Services
process Control = "control"
begin
Control_fetch:
    while ~stopped do
        await daemonUp /\ dirty;
        dirty := FALSE;
        logQ[daemonGen] := Append(logQ[daemonGen], FetchLine(Ack));
        ackMade := ackMade \/ Ack;
        pruneFetched := pruneFetched \/ pruneApplied;
        unfetched := {};
    end while;
end process;

\* handleLine reads tailscaled's log in order, later: a fetch is counted;
\* an edit that removed a Service, or the first, is a removal; an edit
\* advertising a Service once drained starts drainAgain
fair process Counter = "counter"
begin
Counter_count:
    while ~stopped do
        with g \in {x \in Gens : logQ[x] # <<>> /\ (Reads(x) \/ RelistLine(x))},
             e = Head(logQ[g]) do
            if e.k = "fetch" then
                fetchCount[g] := fetchCount[g] + 1;
                ackCounted[g] := ackCounted[g] \/ e.ack;
            else
                if ~listKnown[g] \/ listed[g] \ e.val # {} then
                    removals[g] := removals[g] + 1;
                    removedAt[g] := fetchCount[g];
                    removedTo[g] := e.val;
                end if;
                listed[g] := e.val;
                listKnown[g] := TRUE;
                relisted[g] := drained /\ e.val # {};
                if drained /\ e.val # {} /\ ~ctxDone[g]
                   /\ Mutation # "no-redrain" then
                    redrainReq[g] := TRUE;
                end if;
            end if;
            logQ[g] := Tail(logQ[g]);
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
          waitOn, refused, dirty, logQ, fetchCount, removals, removedAt, 
          removedTo, listed, listKnown, relisted, redrainReq, certIssued, 
          supDaemon, stopped, leaving, retryGen, confReq, leaseReq, drained, 
          lock, cancelReg, cctx, cancelled, rmRec, unpruned, gen, res, out, 
          mark, lrec, leaveD, leaveEnd, settled, drainedGen, everInPrefs, 
          lockedDrained, addAfterDrained, startedDrained, startDrainFailed, 
          drainApplied, ackMade, ackCounted, ackTimedOut, published, 
          leftAdvertising, upAtLeave, pruneApplied, pruneFetched, unfetched, 
          pc, stack

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



NeedsDrain == Services # {} \/ unpruned \/ rmRec.set
NeedsDrainRead == Services # {} \/ unpruned \/ lock = None



RecordsAtSend(a) ==
    /\ Mutation # "record-on-return"
    /\ Mutation = "drain-only-record" => a \in Drainers \cup {"redrain", "sup"}


RemovalFetched(rec, g) ==
    IF Mutation = "count-before-edit"
    THEN fetchCount[g] > rec.fetches
    ELSE /\ \/ rec.start
            \/ removals[g] > rec.removals /\ removedTo[g] \subseteq rec.leaves
         /\ ~relisted[g]
         /\ Mutation # "no-listed-check" => listed[g] \subseteq rec.leaves
         /\ fetchCount[g] > removedAt[g] + (IF Mutation = "off-by-one" THEN 1 ELSE 0)


SendLabels == {"Drain_countRead", "Prune_begin", "Readvertise_begin",
               "Lease_unadvertise", "Start_drain", "Redrain_lock"}




Reads(g) ==
    \/ pc["leave"] \in {"Wait_block", "Leave_floor"} /\ leaveD = g
    \/ \E p \in Actors : pc[p] \in SendLabels /\ gen[p] = g

    \/ Confirmer /\ \E p \in Changers \cup Leases :
           pc[p] = "Change_locked" /\ gen[p] = g

    \/ \E c \in Confs : pc[c] = "Confirm_wait" /\ gen[c] = g
RelistLine(g) ==
    drained /\ \E i \in DOMAIN logQ[g] :
                   logQ[g][i].k = "edit" /\ logQ[g][i].val # {}

PublishedRuns == supDaemon # 0 /\ daemonUp /\ supDaemon = daemonGen


Orphan(r) == waitOn[r.from] # r





Settled ==
    /\ \A r \in pending : r.kind = "add" => ~Orphan(r)
    /\ \A g \in Gens : ~RelistLine(g)
    /\ \A g \in Gens : ~redrainReq[g]
    /\ pc["redrain"] # "Redrain_lock"




ReadvertisedDrainedAgain ==
    \A g \in Gens :
        relisted[g] /\ ~ctxDone[g] /\ logQ[g] = <<>>
        /\ daemonUp /\ g = daemonGen
        => \/ redrainReq[g] \/ pc["redrain"] # "Redrain_idle" \/ prefs = {}
           \/ \E r \in pending : r.gen = g /\ r.val = {}

DrainedFlagHoldsBack == ~addAfterDrained


NoDaemonAdvertisesDrained ==
    daemonUp /\ startedDrained /\ supDaemon = daemonGen
    => \/ prefs = {}
       \/ startDrainFailed[daemonGen]
       \/ \E r \in pending : r.gen = daemonGen /\ r.val = {}









LeaveWaitsForDrain ==
    pc["leave"] = "Done" /\ leaveEnd \notin {"timeout", "aborted"}
    /\ upAtLeave
    => /\ drainApplied => ackMade
       /\ pruneApplied => pruneFetched
       /\ unfetched = {}


NoLeaveWhileAdvertising == ~leftAdvertising

NoWaitForNothing ==
    pc["leave"] = "Wait_block" => everInPrefs


AckEndsWait == ~ackTimedOut

LeaveTerminates == <>(pc["leave"] = "Done")

DrainGetsLock ==
    \A p \in Drainers : pc[p] = "Drain_marked" ~> pc[p] # "Drain_marked"

VARIABLES svc, todo, cur, nudges, cafter, early

vars == << stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending, 
           waitOn, refused, dirty, logQ, fetchCount, removals, removedAt, 
           removedTo, listed, listKnown, relisted, redrainReq, certIssued, 
           supDaemon, stopped, leaving, retryGen, confReq, leaseReq, drained, 
           lock, cancelReg, cctx, cancelled, rmRec, unpruned, gen, res, out, 
           mark, lrec, leaveD, leaveEnd, settled, drainedGen, everInPrefs, 
           lockedDrained, addAfterDrained, startedDrained, startDrainFailed, 
           drainApplied, ackMade, ackCounted, ackTimedOut, published, 
           leftAdvertising, upAtLeave, pruneApplied, pruneFetched, unfetched, 
           pc, stack, svc, todo, cur, nudges, cafter, early >>

ProcSet == {"sup"} \cup {"retry"} \cup (Confs) \cup (Leases) \cup {"stop"} \cup {"leave"} \cup {"redrain"} \cup {"tailscaled"} \cup {"orphans"} \cup {"control"} \cup {"counter"} \cup {"crash"} \cup {"issuer"}

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
        /\ logQ = [g \in Gens |-> <<>>]
        /\ fetchCount = [g \in Gens |-> 0]
        /\ removals = [g \in Gens |-> 0]
        /\ removedAt = [g \in Gens |-> 0]
        /\ removedTo = [g \in Gens |-> {}]
        /\ listed = [g \in Gens |-> {}]
        /\ listKnown = [g \in Gens |-> FALSE]
        /\ relisted = [g \in Gens |-> FALSE]
        /\ redrainReq = [g \in Gens |-> FALSE]
        /\ certIssued = [s \in Services |-> FALSE]
        /\ supDaemon = 0
        /\ stopped = FALSE
        /\ leaving = FALSE
        /\ retryGen = [s \in Services |-> 0]
        /\ confReq = [s \in Services |-> NoConf]
        /\ leaseReq = [s \in Services |-> 0]
        /\ drained = FALSE
        /\ lock = None
        /\ cancelReg = None
        /\ cctx = [a \in Actors |-> FALSE]
        /\ cancelled = FALSE
        /\ rmRec = NoRm
        /\ unpruned = FALSE
        /\ gen = [a \in Actors |-> 0]
        /\ res = [a \in Actors |-> "ok"]
        /\ out = [a \in Actors |-> "ok"]
        /\ mark = [a \in Actors |-> 0]
        /\ lrec = NoRm
        /\ leaveD = 0
        /\ leaveEnd = None
        /\ settled = FALSE
        /\ drainedGen = 0
        /\ everInPrefs = (prefs # {})
        /\ lockedDrained = [a \in Actors |-> FALSE]
        /\ addAfterDrained = FALSE
        /\ startedDrained = FALSE
        /\ startDrainFailed = [g \in Gens |-> FALSE]
        /\ drainApplied = FALSE
        /\ ackMade = FALSE
        /\ ackCounted = [g \in Gens |-> FALSE]
        /\ ackTimedOut = FALSE
        /\ published = FALSE
        /\ leftAdvertising = FALSE
        /\ upAtLeave = FALSE
        /\ pruneApplied = FALSE
        /\ pruneFetched = FALSE
        /\ unfetched = {}
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
                                        [] self = "redrain" -> "Redrain_idle"
                                        [] self = "tailscaled" -> "Tailscaled_answer"
                                        [] self = "orphans" -> "Orphans_drop"
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
                                     refused, dirty, logQ, fetchCount, 
                                     removals, removedAt, removedTo, listed, 
                                     listKnown, relisted, redrainReq, 
                                     certIssued, supDaemon, stopped, leaving, 
                                     confReq, leaseReq, drained, cctx, 
                                     cancelled, rmRec, unpruned, gen, res, 
                                     mark, lrec, leaveD, leaveEnd, settled, 
                                     drainedGen, everInPrefs, addAfterDrained, 
                                     startedDrained, startDrainFailed, 
                                     drainApplied, ackMade, ackCounted, 
                                     ackTimedOut, published, leftAdvertising, 
                                     upAtLeave, pruneApplied, pruneFetched, 
                                     unfetched, todo, cur, nudges, cafter, 
                                     early >>

Change_locked(self) == /\ pc[self] = "Change_locked"
                       /\ IF drained /\ Checks
                             THEN /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                  /\ UNCHANGED << pending, waitOn, res, mark, 
                                                  addAfterDrained >>
                             ELSE /\ IF ~CallOK(self)
                                        THEN /\ res' = [res EXCEPT ![self] = "err"]
                                             /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
                                             /\ UNCHANGED << pending, waitOn, 
                                                             mark, 
                                                             addAfterDrained >>
                                        ELSE /\ IF svc[self] \in prefs
                                                   THEN /\ pc' = [pc EXCEPT ![self] = "Change_fnReturned"]
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
                                             /\ res' = res
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, refused, dirty, logQ, 
                                       fetchCount, removals, removedAt, 
                                       removedTo, listed, listKnown, relisted, 
                                       redrainReq, certIssued, supDaemon, 
                                       stopped, leaving, retryGen, confReq, 
                                       leaseReq, drained, lock, cancelReg, 
                                       cctx, cancelled, rmRec, unpruned, gen, 
                                       out, lrec, leaveD, leaveEnd, settled, 
                                       drainedGen, everInPrefs, lockedDrained, 
                                       startedDrained, startDrainFailed, 
                                       drainApplied, ackMade, ackCounted, 
                                       ackTimedOut, published, leftAdvertising, 
                                       upAtLeave, pruneApplied, pruneFetched, 
                                       unfetched, stack, svc, todo, cur, 
                                       nudges, cafter, early >>

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
                                        dirty, logQ, fetchCount, removals, 
                                        removedAt, removedTo, listed, 
                                        listKnown, relisted, redrainReq, 
                                        certIssued, supDaemon, stopped, 
                                        leaving, retryGen, confReq, leaseReq, 
                                        drained, lock, cancelReg, cctx, 
                                        cancelled, rmRec, unpruned, gen, out, 
                                        mark, lrec, leaveD, leaveEnd, settled, 
                                        drainedGen, everInPrefs, lockedDrained, 
                                        addAfterDrained, startedDrained, 
                                        startDrainFailed, drainApplied, 
                                        ackMade, ackCounted, ackTimedOut, 
                                        published, leftAdvertising, upAtLeave, 
                                        pruneApplied, pruneFetched, unfetched, 
                                        stack, svc, todo, cur, nudges, cafter, 
                                        early >>

Change_fnReturned(self) == /\ pc[self] = "Change_fnReturned"
                           /\ IF res[self] = "added"
                                 THEN /\ IF Confirmer
                                            THEN /\ confReq' = [confReq EXCEPT ![svc[self]] = [gen |-> gen[self], after |-> mark[self]]]
                                            ELSE /\ TRUE
                                                 /\ UNCHANGED confReq
                                 ELSE /\ TRUE
                                      /\ UNCHANGED confReq
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
                                           waitOn, refused, dirty, logQ, 
                                           fetchCount, removals, removedAt, 
                                           removedTo, listed, listKnown, 
                                           relisted, redrainReq, certIssued, 
                                           supDaemon, stopped, leaving, 
                                           leaseReq, drained, cancelled, rmRec, 
                                           unpruned, gen, lrec, leaveD, 
                                           leaveEnd, settled, drainedGen, 
                                           everInPrefs, addAfterDrained, 
                                           startedDrained, startDrainFailed, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, upAtLeave, 
                                           pruneApplied, pruneFetched, 
                                           unfetched, todo, cur, nudges, 
                                           cafter, early >>

Configure(self) == Change_lock(self) \/ Change_locked(self)
                      \/ Advertise_sent(self) \/ Change_fnReturned(self)

Drain_begin(self) == /\ pc[self] = "Drain_begin"
                     /\ drained' = TRUE
                     /\ IF cancelReg # None /\ Cancels
                           THEN /\ cctx' = [cctx EXCEPT ![cancelReg] = TRUE]
                                /\ cancelled' = TRUE
                           ELSE /\ TRUE
                                /\ UNCHANGED << cctx, cancelled >>
                     /\ pc' = [pc EXCEPT ![self] = "Drain_marked"]
                     /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                     ctxDone, crashes, pending, waitOn, 
                                     refused, dirty, logQ, fetchCount, 
                                     removals, removedAt, removedTo, listed, 
                                     listKnown, relisted, redrainReq, 
                                     certIssued, supDaemon, stopped, leaving, 
                                     retryGen, confReq, leaseReq, lock, 
                                     cancelReg, rmRec, unpruned, gen, res, out, 
                                     mark, lrec, leaveD, leaveEnd, settled, 
                                     drainedGen, everInPrefs, lockedDrained, 
                                     addAfterDrained, startedDrained, 
                                     startDrainFailed, drainApplied, ackMade, 
                                     ackCounted, ackTimedOut, published, 
                                     leftAdvertising, upAtLeave, pruneApplied, 
                                     pruneFetched, unfetched, stack, svc, todo, 
                                     cur, nudges, cafter, early >>

Drain_marked(self) == /\ pc[self] = "Drain_marked"
                      /\ IF self = "leave"
                            THEN /\ drainedGen' = supDaemon
                            ELSE /\ TRUE
                                 /\ UNCHANGED drainedGen
                      /\ IF supDaemon = 0
                            THEN /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                                 /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                                 /\ UNCHANGED << lock, cctx, cancelled, gen >>
                            ELSE /\ lock = None
                                 /\ lock' = self
                                 /\ gen' = [gen EXCEPT ![self] = supDaemon]
                                 /\ IF cancelReg # None /\ Cancels
                                       THEN /\ cctx' = [cctx EXCEPT ![cancelReg] = TRUE]
                                            /\ cancelled' = TRUE
                                       ELSE /\ TRUE
                                            /\ UNCHANGED << cctx, cancelled >>
                                 /\ pc' = [pc EXCEPT ![self] = "Drain_countRead"]
                                 /\ stack' = stack
                      /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                      ctxDone, crashes, pending, waitOn, 
                                      refused, dirty, logQ, fetchCount, 
                                      removals, removedAt, removedTo, listed, 
                                      listKnown, relisted, redrainReq, 
                                      certIssued, supDaemon, stopped, leaving, 
                                      retryGen, confReq, leaseReq, drained, 
                                      cancelReg, rmRec, unpruned, res, out, 
                                      mark, lrec, leaveD, leaveEnd, settled, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      startDrainFailed, drainApplied, ackMade, 
                                      ackCounted, ackTimedOut, published, 
                                      leftAdvertising, upAtLeave, pruneApplied, 
                                      pruneFetched, unfetched, svc, todo, cur, 
                                      nudges, cafter, early >>

Drain_countRead(self) == /\ pc[self] = "Drain_countRead"
                         /\ IF ~Live(self)
                               THEN /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                    /\ UNCHANGED << pending, waitOn, rmRec >>
                               ELSE /\ IF prefs = {}
                                          THEN /\ IF rmRec.set /\ rmRec.gen # gen[self]
                                                     /\ Mutation # "no-start-record"
                                                     THEN /\ rmRec' = [rmRec EXCEPT !.gen = gen[self], !.start = TRUE]
                                                     ELSE /\ TRUE
                                                          /\ rmRec' = rmRec
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                                               /\ UNCHANGED << pending, waitOn >>
                                          ELSE /\ IF RecordsAtSend(self)
                                                     THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                                       leaves |-> ({}), start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                                     ELSE /\ TRUE
                                                          /\ rmRec' = rmRec
                                               /\ LET r == [from |-> self, gen |-> gen[self], val |-> {},
                                                            kind |-> "drain"] IN
                                                    /\ pending' = (pending \cup {r})
                                                    /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                               /\ pc' = [pc EXCEPT ![self] = "Drain_sent"]
                         /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                         daemonUp, ctxDone, crashes, refused, 
                                         dirty, logQ, fetchCount, removals, 
                                         removedAt, removedTo, listed, 
                                         listKnown, relisted, redrainReq, 
                                         certIssued, supDaemon, stopped, 
                                         leaving, retryGen, confReq, leaseReq, 
                                         drained, lock, cancelReg, cctx, 
                                         cancelled, unpruned, gen, res, out, 
                                         mark, lrec, leaveD, leaveEnd, settled, 
                                         drainedGen, everInPrefs, 
                                         lockedDrained, addAfterDrained, 
                                         startedDrained, startDrainFailed, 
                                         drainApplied, ackMade, ackCounted, 
                                         ackTimedOut, published, 
                                         leftAdvertising, upAtLeave, 
                                         pruneApplied, pruneFetched, unfetched, 
                                         stack, svc, todo, cur, nudges, cafter, 
                                         early >>

Drain_sent(self) == /\ pc[self] = "Drain_sent"
                    /\ \/ /\ Applied(self)
                          /\ IF Mutation = "record-on-return"
                                THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                  leaves |-> ({}), start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                ELSE /\ TRUE
                                     /\ rmRec' = rmRec
                       \/ /\ ~Live(self)
                          /\ rmRec' = rmRec
                       \/ /\ TRUE
                          /\ rmRec' = rmRec
                    /\ waitOn' = [waitOn EXCEPT ![self] = NoReq]
                    /\ pc' = [pc EXCEPT ![self] = "Drain_release"]
                    /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                    ctxDone, crashes, pending, refused, dirty, 
                                    logQ, fetchCount, removals, removedAt, 
                                    removedTo, listed, listKnown, relisted, 
                                    redrainReq, certIssued, supDaemon, stopped, 
                                    leaving, retryGen, confReq, leaseReq, 
                                    drained, lock, cancelReg, cctx, cancelled, 
                                    unpruned, gen, res, out, mark, lrec, 
                                    leaveD, leaveEnd, settled, drainedGen, 
                                    everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    startDrainFailed, drainApplied, ackMade, 
                                    ackCounted, ackTimedOut, published, 
                                    leftAdvertising, upAtLeave, pruneApplied, 
                                    pruneFetched, unfetched, stack, svc, todo, 
                                    cur, nudges, cafter, early >>

Drain_release(self) == /\ pc[self] = "Drain_release"
                       /\ IF lock = self
                             THEN /\ lock' = None
                             ELSE /\ TRUE
                                  /\ lock' = lock
                       /\ gen' = [gen EXCEPT ![self] = 0]
                       /\ pc' = [pc EXCEPT ![self] = Head(stack[self]).pc]
                       /\ stack' = [stack EXCEPT ![self] = Tail(stack[self])]
                       /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                       ctxDone, crashes, pending, waitOn, 
                                       refused, dirty, logQ, fetchCount, 
                                       removals, removedAt, removedTo, listed, 
                                       listKnown, relisted, redrainReq, 
                                       certIssued, supDaemon, stopped, leaving, 
                                       retryGen, confReq, leaseReq, drained, 
                                       cancelReg, cctx, cancelled, rmRec, 
                                       unpruned, res, out, mark, lrec, leaveD, 
                                       leaveEnd, settled, drainedGen, 
                                       everInPrefs, lockedDrained, 
                                       addAfterDrained, startedDrained, 
                                       startDrainFailed, drainApplied, ackMade, 
                                       ackCounted, ackTimedOut, published, 
                                       leftAdvertising, upAtLeave, 
                                       pruneApplied, pruneFetched, unfetched, 
                                       svc, todo, cur, nudges, cafter, early >>

DrainProc(self) == Drain_begin(self) \/ Drain_marked(self)
                      \/ Drain_countRead(self) \/ Drain_sent(self)
                      \/ Drain_release(self)

Sup_loop == /\ pc["sup"] = "Sup_loop"
            /\ IF ~stopped
                  THEN /\ daemonGen' = daemonGen + 1
                       /\ gen' = [gen EXCEPT !["sup"] = daemonGen']
                       /\ daemonUp' = TRUE
                       /\ stateExists' = TRUE
                       /\ startedDrained' = drained
                       /\ dirty' = TRUE
                       /\ todo' = Services
                       /\ pc' = [pc EXCEPT !["sup"] = "Start_drain"]
                  ELSE /\ pc' = [pc EXCEPT !["sup"] = "Done"]
                       /\ UNCHANGED << stateExists, daemonGen, daemonUp, dirty, 
                                       gen, startedDrained, todo >>
            /\ UNCHANGED << prefs, ctxDone, crashes, pending, waitOn, refused, 
                            logQ, fetchCount, removals, removedAt, removedTo, 
                            listed, listKnown, relisted, redrainReq, 
                            certIssued, supDaemon, stopped, leaving, retryGen, 
                            confReq, leaseReq, drained, lock, cancelReg, cctx, 
                            cancelled, rmRec, unpruned, res, out, mark, lrec, 
                            leaveD, leaveEnd, settled, drainedGen, everInPrefs, 
                            lockedDrained, addAfterDrained, startDrainFailed, 
                            drainApplied, ackMade, ackCounted, ackTimedOut, 
                            published, leftAdvertising, upAtLeave, 
                            pruneApplied, pruneFetched, unfetched, stack, svc, 
                            cur, nudges, cafter, early >>

Start_drain == /\ pc["sup"] = "Start_drain"
               /\ IF drained /\ Mutation # "no-drain-at-start"
                     THEN /\ lock = None
                          /\ IF ~CallOK("sup")
                                THEN /\ startDrainFailed' = [startDrainFailed EXCEPT ![gen["sup"]] = TRUE]
                                     /\ pc' = [pc EXCEPT !["sup"] = "Start_next"]
                                     /\ UNCHANGED << pending, waitOn, lock, 
                                                     rmRec >>
                                ELSE /\ IF prefs = {}
                                           THEN /\ IF rmRec.set /\ rmRec.gen # gen["sup"]
                                                      THEN /\ rmRec' = [rmRec EXCEPT !.gen = gen["sup"], !.start = TRUE]
                                                      ELSE /\ TRUE
                                                           /\ rmRec' = rmRec
                                                /\ pc' = [pc EXCEPT !["sup"] = "Start_next"]
                                                /\ UNCHANGED << pending, 
                                                                waitOn, lock >>
                                           ELSE /\ lock' = "sup"
                                                /\ rmRec' = [set |-> TRUE, gen |-> (gen["sup"]), removals |-> removals[(gen["sup"])],
                                                             leaves |-> ({}), start |-> FALSE, fetches |-> fetchCount[(gen["sup"])]]
                                                /\ LET r == [from |-> "sup", gen |-> gen["sup"], val |-> {},
                                                             kind |-> "drain"] IN
                                                     /\ pending' = (pending \cup {r})
                                                     /\ waitOn' = [waitOn EXCEPT !["sup"] = r]
                                                /\ pc' = [pc EXCEPT !["sup"] = "Start_drainSent"]
                                     /\ UNCHANGED startDrainFailed
                     ELSE /\ pc' = [pc EXCEPT !["sup"] = "Start_next"]
                          /\ UNCHANGED << pending, waitOn, lock, rmRec, 
                                          startDrainFailed >>
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, refused, dirty, logQ, 
                               fetchCount, removals, removedAt, removedTo, 
                               listed, listKnown, relisted, redrainReq, 
                               certIssued, supDaemon, stopped, leaving, 
                               retryGen, confReq, leaseReq, drained, cancelReg, 
                               cctx, cancelled, unpruned, gen, res, out, mark, 
                               lrec, leaveD, leaveEnd, settled, drainedGen, 
                               everInPrefs, lockedDrained, addAfterDrained, 
                               startedDrained, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, upAtLeave, pruneApplied, 
                               pruneFetched, unfetched, stack, svc, todo, cur, 
                               nudges, cafter, early >>

Start_next == /\ pc["sup"] = "Start_next"
              /\ IF Prune
                    THEN /\ pc' = [pc EXCEPT !["sup"] = "Prune_begin"]
                    ELSE /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, logQ, 
                              fetchCount, removals, removedAt, removedTo, 
                              listed, listKnown, relisted, redrainReq, 
                              certIssued, supDaemon, stopped, leaving, 
                              retryGen, confReq, leaseReq, drained, lock, 
                              cancelReg, cctx, cancelled, rmRec, unpruned, gen, 
                              res, out, mark, lrec, leaveD, leaveEnd, settled, 
                              drainedGen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, 
                              startDrainFailed, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, 
                              leftAdvertising, upAtLeave, pruneApplied, 
                              pruneFetched, unfetched, stack, svc, todo, cur, 
                              nudges, cafter, early >>

Start_drainSent == /\ pc["sup"] = "Start_drainSent"
                   /\ \/ /\ Applied("sup")
                         /\ UNCHANGED startDrainFailed
                      \/ /\ ~Live("sup")
                         /\ startDrainFailed' = [startDrainFailed EXCEPT ![gen["sup"]] = TRUE]
                      \/ /\ startDrainFailed' = [startDrainFailed EXCEPT ![gen["sup"]] = TRUE]
                   /\ waitOn' = [waitOn EXCEPT !["sup"] = NoReq]
                   /\ lock' = None
                   /\ IF ~Prune
                         THEN /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                         ELSE /\ pc' = [pc EXCEPT !["sup"] = "Prune_begin"]
                   /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                   ctxDone, crashes, pending, refused, dirty, 
                                   logQ, fetchCount, removals, removedAt, 
                                   removedTo, listed, listKnown, relisted, 
                                   redrainReq, certIssued, supDaemon, stopped, 
                                   leaving, retryGen, confReq, leaseReq, 
                                   drained, cancelReg, cctx, cancelled, rmRec, 
                                   unpruned, gen, res, out, mark, lrec, leaveD, 
                                   leaveEnd, settled, drainedGen, everInPrefs, 
                                   lockedDrained, addAfterDrained, 
                                   startedDrained, drainApplied, ackMade, 
                                   ackCounted, ackTimedOut, published, 
                                   leftAdvertising, upAtLeave, pruneApplied, 
                                   pruneFetched, unfetched, stack, svc, todo, 
                                   cur, nudges, cafter, early >>

Prune_begin == /\ pc["sup"] = "Prune_begin"
               /\ lock = None
               /\ IF drained
                     THEN /\ unpruned' = (Mutation # "drained-pruned")
                          /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                          /\ UNCHANGED << pending, waitOn, lock, cancelReg, 
                                          rmRec >>
                     ELSE /\ IF ~CallOK("sup")
                                THEN /\ unpruned' = TRUE
                                     /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                                     /\ UNCHANGED << pending, waitOn, lock, 
                                                     cancelReg, rmRec >>
                                ELSE /\ IF Stale \notin prefs
                                           THEN /\ unpruned' = FALSE
                                                /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                                                /\ UNCHANGED << pending, 
                                                                waitOn, lock, 
                                                                cancelReg, 
                                                                rmRec >>
                                           ELSE /\ lock' = "sup"
                                                /\ cancelReg' = "sup"
                                                /\ IF RecordsAtSend("sup")
                                                      THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen["sup"]), removals |-> removals[(gen["sup"])],
                                                                        leaves |-> (prefs \ {Stale}), start |-> FALSE, fetches |-> fetchCount[(gen["sup"])]]
                                                      ELSE /\ TRUE
                                                           /\ rmRec' = rmRec
                                                /\ LET r == [from |-> "sup", gen |-> gen["sup"],
                                                             val |-> prefs \ {Stale}, kind |-> "prune"] IN
                                                     /\ pending' = (pending \cup {r})
                                                     /\ waitOn' = [waitOn EXCEPT !["sup"] = r]
                                                /\ pc' = [pc EXCEPT !["sup"] = "Prune_sent"]
                                                /\ UNCHANGED unpruned
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, refused, dirty, logQ, 
                               fetchCount, removals, removedAt, removedTo, 
                               listed, listKnown, relisted, redrainReq, 
                               certIssued, supDaemon, stopped, leaving, 
                               retryGen, confReq, leaseReq, drained, cctx, 
                               cancelled, gen, res, out, mark, lrec, leaveD, 
                               leaveEnd, settled, drainedGen, everInPrefs, 
                               lockedDrained, addAfterDrained, startedDrained, 
                               startDrainFailed, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, upAtLeave, pruneApplied, 
                               pruneFetched, unfetched, stack, svc, todo, cur, 
                               nudges, cafter, early >>

Prune_sent == /\ pc["sup"] = "Prune_sent"
              /\ \/ /\ Applied("sup")
                    /\ res' = [res EXCEPT !["sup"] = "added"]
                    /\ IF Mutation = "record-on-return"
                          THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen["sup"]), removals |-> removals[(gen["sup"])],
                                            leaves |-> prefs, start |-> FALSE, fetches |-> fetchCount[(gen["sup"])]]
                          ELSE /\ TRUE
                               /\ rmRec' = rmRec
                 \/ /\ ~CallOK("sup")
                    /\ res' = [res EXCEPT !["sup"] = "err"]
                    /\ rmRec' = rmRec
                 \/ /\ res' = [res EXCEPT !["sup"] = "err"]
                    /\ rmRec' = rmRec
              /\ waitOn' = [waitOn EXCEPT !["sup"] = NoReq]
              /\ pc' = [pc EXCEPT !["sup"] = "Prune_edited"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, refused, dirty, logQ, 
                              fetchCount, removals, removedAt, removedTo, 
                              listed, listKnown, relisted, redrainReq, 
                              certIssued, supDaemon, stopped, leaving, 
                              retryGen, confReq, leaseReq, drained, lock, 
                              cancelReg, cctx, cancelled, unpruned, gen, out, 
                              mark, lrec, leaveD, leaveEnd, settled, 
                              drainedGen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, 
                              startDrainFailed, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, 
                              leftAdvertising, upAtLeave, pruneApplied, 
                              pruneFetched, unfetched, stack, svc, todo, cur, 
                              nudges, cafter, early >>

Prune_edited == /\ pc["sup"] = "Prune_edited"
                /\ unpruned' = (res["sup"] = "err"
                                /\ (Mutation # "drained-pruned" \/ ~drained))
                /\ lock' = None
                /\ cancelReg' = None
                /\ cctx' = [cctx EXCEPT !["sup"] = FALSE]
                /\ res' = [res EXCEPT !["sup"] = "ok"]
                /\ pc' = [pc EXCEPT !["sup"] = "Start_backends"]
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, pending, waitOn, refused, 
                                dirty, logQ, fetchCount, removals, removedAt, 
                                removedTo, listed, listKnown, relisted, 
                                redrainReq, certIssued, supDaemon, stopped, 
                                leaving, retryGen, confReq, leaseReq, drained, 
                                cancelled, rmRec, gen, out, mark, lrec, leaveD, 
                                leaveEnd, settled, drainedGen, everInPrefs, 
                                lockedDrained, addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
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
                                  dirty, logQ, fetchCount, removals, removedAt, 
                                  removedTo, listed, listKnown, relisted, 
                                  redrainReq, certIssued, stopped, leaving, 
                                  retryGen, confReq, drained, lock, cancelReg, 
                                  cctx, cancelled, rmRec, unpruned, gen, res, 
                                  out, mark, lrec, leaveD, leaveEnd, settled, 
                                  drainedGen, everInPrefs, lockedDrained, 
                                  addAfterDrained, startedDrained, 
                                  startDrainFailed, drainApplied, ackMade, 
                                  ackCounted, ackTimedOut, leftAdvertising, 
                                  upAtLeave, pruneApplied, pruneFetched, 
                                  unfetched, nudges, cafter, early >>

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
                                  logQ, fetchCount, removals, removedAt, 
                                  removedTo, listed, listKnown, relisted, 
                                  redrainReq, certIssued, stopped, leaving, 
                                  retryGen, confReq, leaseReq, drained, lock, 
                                  cancelReg, cctx, cancelled, rmRec, unpruned, 
                                  gen, res, out, mark, lrec, leaveD, leaveEnd, 
                                  settled, drainedGen, everInPrefs, 
                                  lockedDrained, addAfterDrained, 
                                  startedDrained, startDrainFailed, 
                                  drainApplied, ackMade, ackCounted, 
                                  ackTimedOut, published, leftAdvertising, 
                                  upAtLeave, pruneApplied, pruneFetched, 
                                  unfetched, stack, svc, todo, cur, nudges, 
                                  cafter, early >>

Supervisor == Sup_loop \/ Start_drain \/ Start_next \/ Start_drainSent
                 \/ Prune_begin \/ Prune_sent \/ Prune_edited
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
                              crashes, pending, waitOn, refused, dirty, logQ, 
                              fetchCount, removals, removedAt, removedTo, 
                              listed, listKnown, relisted, redrainReq, 
                              certIssued, supDaemon, stopped, leaving, confReq, 
                              leaseReq, drained, lock, cancelReg, cctx, 
                              cancelled, rmRec, unpruned, res, out, mark, lrec, 
                              leaveD, leaveEnd, settled, drainedGen, 
                              everInPrefs, lockedDrained, addAfterDrained, 
                              startedDrained, startDrainFailed, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              leftAdvertising, upAtLeave, pruneApplied, 
                              pruneFetched, unfetched, todo, cur, nudges, 
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
                                      refused, dirty, logQ, fetchCount, 
                                      removals, removedAt, removedTo, listed, 
                                      listKnown, relisted, redrainReq, 
                                      certIssued, supDaemon, stopped, leaving, 
                                      retryGen, leaseReq, drained, lock, 
                                      cancelReg, cctx, cancelled, rmRec, 
                                      unpruned, res, out, mark, lrec, leaveD, 
                                      leaveEnd, settled, drainedGen, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      startDrainFailed, drainApplied, ackMade, 
                                      ackCounted, ackTimedOut, published, 
                                      leftAdvertising, upAtLeave, pruneApplied, 
                                      pruneFetched, unfetched, stack, svc, 
                                      todo, cur, early >>

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
                                      refused, dirty, logQ, fetchCount, 
                                      removals, removedAt, removedTo, listed, 
                                      listKnown, relisted, redrainReq, 
                                      certIssued, supDaemon, stopped, leaving, 
                                      retryGen, confReq, leaseReq, drained, 
                                      lock, cancelReg, cctx, cancelled, rmRec, 
                                      unpruned, gen, res, out, mark, lrec, 
                                      leaveD, leaveEnd, settled, drainedGen, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      startDrainFailed, drainApplied, ackMade, 
                                      ackCounted, ackTimedOut, published, 
                                      leftAdvertising, upAtLeave, pruneApplied, 
                                      pruneFetched, unfetched, stack, svc, 
                                      todo, cur, early >>

Readvertise_begin(self) == /\ pc[self] = "Readvertise_begin"
                           /\ lock = None
                           /\ IF drained /\ Checks
                                 THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                      /\ UNCHANGED << pending, waitOn, lock, 
                                                      cancelReg, rmRec >>
                                 ELSE /\ IF ~CallOK(self)
                                            THEN /\ pc' = [pc EXCEPT ![self] = "Confirm_idle"]
                                                 /\ UNCHANGED << pending, 
                                                                 waitOn, lock, 
                                                                 cancelReg, 
                                                                 rmRec >>
                                            ELSE /\ IF SvcOf(self) \notin prefs
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "Readvertise_gap"]
                                                            /\ UNCHANGED << pending, 
                                                                            waitOn, 
                                                                            lock, 
                                                                            cancelReg, 
                                                                            rmRec >>
                                                       ELSE /\ lock' = self
                                                            /\ cancelReg' = self
                                                            /\ IF RecordsAtSend(self)
                                                                  THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                                                    leaves |-> (prefs \ {SvcOf(self)}), start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                                                  ELSE /\ TRUE
                                                                       /\ rmRec' = rmRec
                                                            /\ LET r == [from |-> self, gen |-> gen[self],
                                                                         val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] IN
                                                                 /\ pending' = (pending \cup {r})
                                                                 /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                            /\ pc' = [pc EXCEPT ![self] = "Readvertise_removed"]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, refused, 
                                           dirty, logQ, fetchCount, removals, 
                                           removedAt, removedTo, listed, 
                                           listKnown, relisted, redrainReq, 
                                           certIssued, supDaemon, stopped, 
                                           leaving, retryGen, confReq, 
                                           leaseReq, drained, cctx, cancelled, 
                                           unpruned, gen, res, out, mark, lrec, 
                                           leaveD, leaveEnd, settled, 
                                           drainedGen, everInPrefs, 
                                           lockedDrained, addAfterDrained, 
                                           startedDrained, startDrainFailed, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, upAtLeave, 
                                           pruneApplied, pruneFetched, 
                                           unfetched, stack, svc, todo, cur, 
                                           nudges, cafter, early >>

Readvertise_removed(self) == /\ pc[self] = "Readvertise_removed"
                             /\ \/ /\ Applied(self)
                                   /\ IF Mutation = "record-on-return"
                                         THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                           leaves |-> prefs, start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                         ELSE /\ TRUE
                                              /\ rmRec' = rmRec
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
                                   /\ rmRec' = rmRec
                             /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                             daemonUp, ctxDone, crashes, 
                                             pending, refused, dirty, logQ, 
                                             fetchCount, removals, removedAt, 
                                             removedTo, listed, listKnown, 
                                             relisted, redrainReq, certIssued, 
                                             supDaemon, stopped, leaving, 
                                             retryGen, confReq, leaseReq, 
                                             drained, cancelled, unpruned, gen, 
                                             res, out, mark, lrec, leaveD, 
                                             leaveEnd, settled, drainedGen, 
                                             everInPrefs, lockedDrained, 
                                             addAfterDrained, startedDrained, 
                                             startDrainFailed, drainApplied, 
                                             ackMade, ackCounted, ackTimedOut, 
                                             published, leftAdvertising, 
                                             upAtLeave, pruneApplied, 
                                             pruneFetched, unfetched, stack, 
                                             svc, todo, cur, nudges, cafter, 
                                             early >>

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
                                         dirty, logQ, fetchCount, removals, 
                                         removedAt, removedTo, listed, 
                                         listKnown, relisted, redrainReq, 
                                         certIssued, supDaemon, stopped, 
                                         leaving, retryGen, confReq, leaseReq, 
                                         drained, cctx, cancelled, rmRec, 
                                         unpruned, gen, res, out, mark, lrec, 
                                         leaveD, leaveEnd, settled, drainedGen, 
                                         everInPrefs, startedDrained, 
                                         startDrainFailed, drainApplied, 
                                         ackMade, ackCounted, ackTimedOut, 
                                         published, leftAdvertising, upAtLeave, 
                                         pruneApplied, pruneFetched, unfetched, 
                                         stack, svc, todo, cur, nudges, cafter, 
                                         early >>

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
                                           dirty, logQ, fetchCount, removals, 
                                           removedAt, removedTo, listed, 
                                           listKnown, relisted, redrainReq, 
                                           certIssued, supDaemon, stopped, 
                                           leaving, retryGen, confReq, 
                                           leaseReq, drained, cancelled, rmRec, 
                                           unpruned, gen, res, out, mark, lrec, 
                                           leaveD, leaveEnd, settled, 
                                           drainedGen, everInPrefs, 
                                           addAfterDrained, startedDrained, 
                                           startDrainFailed, drainApplied, 
                                           ackMade, ackCounted, ackTimedOut, 
                                           published, leftAdvertising, 
                                           upAtLeave, pruneApplied, 
                                           pruneFetched, unfetched, stack, svc, 
                                           todo, cur, nudges, cafter, early >>

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
                                    dirty, logQ, fetchCount, removals, 
                                    removedAt, removedTo, listed, listKnown, 
                                    relisted, redrainReq, certIssued, 
                                    supDaemon, stopped, leaving, retryGen, 
                                    confReq, drained, lock, cancelReg, cctx, 
                                    cancelled, rmRec, unpruned, res, out, mark, 
                                    lrec, leaveD, leaveEnd, settled, 
                                    drainedGen, everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    startDrainFailed, drainApplied, ackMade, 
                                    ackCounted, ackTimedOut, published, 
                                    leftAdvertising, upAtLeave, pruneApplied, 
                                    pruneFetched, unfetched, stack, svc, todo, 
                                    cur, nudges, cafter, early >>

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
                                    dirty, logQ, fetchCount, removals, 
                                    removedAt, removedTo, listed, listKnown, 
                                    relisted, redrainReq, certIssued, 
                                    supDaemon, stopped, leaving, retryGen, 
                                    confReq, leaseReq, drained, lock, 
                                    cancelReg, cctx, cancelled, rmRec, 
                                    unpruned, gen, res, out, mark, lrec, 
                                    leaveD, leaveEnd, settled, drainedGen, 
                                    everInPrefs, lockedDrained, 
                                    addAfterDrained, startedDrained, 
                                    startDrainFailed, drainApplied, ackMade, 
                                    ackCounted, ackTimedOut, published, 
                                    leftAdvertising, upAtLeave, pruneApplied, 
                                    pruneFetched, unfetched, todo, cur, nudges, 
                                    cafter >>

Lease_unadvertise(self) == /\ pc[self] = "Lease_unadvertise"
                           /\ lock = None
                           /\ IF drained /\ Checks
                                 THEN /\ pc' = [pc EXCEPT ![self] = "Lease_idle"]
                                      /\ UNCHANGED << pending, waitOn, lock, 
                                                      cancelReg, rmRec >>
                                 ELSE /\ IF ~CallOK(self)
                                            THEN /\ pc' = [pc EXCEPT ![self] = "Lease_poll"]
                                                 /\ UNCHANGED << pending, 
                                                                 waitOn, lock, 
                                                                 cancelReg, 
                                                                 rmRec >>
                                            ELSE /\ IF SvcOf(self) \notin prefs
                                                       THEN /\ pc' = [pc EXCEPT ![self] = "Lease_request"]
                                                            /\ UNCHANGED << pending, 
                                                                            waitOn, 
                                                                            lock, 
                                                                            cancelReg, 
                                                                            rmRec >>
                                                       ELSE /\ lock' = self
                                                            /\ cancelReg' = self
                                                            /\ IF RecordsAtSend(self)
                                                                  THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                                                    leaves |-> (prefs \ {SvcOf(self)}), start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                                                  ELSE /\ TRUE
                                                                       /\ rmRec' = rmRec
                                                            /\ LET r == [from |-> self, gen |-> gen[self],
                                                                         val |-> prefs \ {SvcOf(self)}, kind |-> "remove"] IN
                                                                 /\ pending' = (pending \cup {r})
                                                                 /\ waitOn' = [waitOn EXCEPT ![self] = r]
                                                            /\ pc' = [pc EXCEPT ![self] = "Lease_unadvertiseSent"]
                           /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                           daemonUp, ctxDone, crashes, refused, 
                                           dirty, logQ, fetchCount, removals, 
                                           removedAt, removedTo, listed, 
                                           listKnown, relisted, redrainReq, 
                                           certIssued, supDaemon, stopped, 
                                           leaving, retryGen, confReq, 
                                           leaseReq, drained, cctx, cancelled, 
                                           unpruned, gen, res, out, mark, lrec, 
                                           leaveD, leaveEnd, settled, 
                                           drainedGen, everInPrefs, 
                                           lockedDrained, addAfterDrained, 
                                           startedDrained, startDrainFailed, 
                                           drainApplied, ackMade, ackCounted, 
                                           ackTimedOut, published, 
                                           leftAdvertising, upAtLeave, 
                                           pruneApplied, pruneFetched, 
                                           unfetched, stack, svc, todo, cur, 
                                           nudges, cafter, early >>

Lease_unadvertiseSent(self) == /\ pc[self] = "Lease_unadvertiseSent"
                               /\ \/ /\ Applied(self)
                                     /\ IF Mutation = "record-on-return"
                                           THEN /\ rmRec' = [set |-> TRUE, gen |-> (gen[self]), removals |-> removals[(gen[self])],
                                                             leaves |-> prefs, start |-> FALSE, fetches |-> fetchCount[(gen[self])]]
                                           ELSE /\ TRUE
                                                /\ rmRec' = rmRec
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
                                     /\ rmRec' = rmRec
                               /\ UNCHANGED << stateExists, prefs, daemonGen, 
                                               daemonUp, ctxDone, crashes, 
                                               pending, refused, dirty, logQ, 
                                               fetchCount, removals, removedAt, 
                                               removedTo, listed, listKnown, 
                                               relisted, redrainReq, 
                                               certIssued, supDaemon, stopped, 
                                               leaving, retryGen, confReq, 
                                               leaseReq, drained, cancelled, 
                                               unpruned, gen, res, out, mark, 
                                               lrec, leaveD, leaveEnd, settled, 
                                               drainedGen, everInPrefs, 
                                               lockedDrained, addAfterDrained, 
                                               startedDrained, 
                                               startDrainFailed, drainApplied, 
                                               ackMade, ackCounted, 
                                               ackTimedOut, published, 
                                               leftAdvertising, upAtLeave, 
                                               pruneApplied, pruneFetched, 
                                               unfetched, stack, svc, todo, 
                                               cur, nudges, cafter, early >>

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
                                       refused, dirty, logQ, fetchCount, 
                                       removals, removedAt, removedTo, listed, 
                                       listKnown, relisted, redrainReq, 
                                       certIssued, supDaemon, stopped, leaving, 
                                       retryGen, confReq, leaseReq, drained, 
                                       lock, cancelReg, cctx, cancelled, rmRec, 
                                       unpruned, gen, res, out, mark, lrec, 
                                       leaveD, leaveEnd, settled, drainedGen, 
                                       everInPrefs, lockedDrained, 
                                       addAfterDrained, startedDrained, 
                                       startDrainFailed, drainApplied, ackMade, 
                                       ackCounted, ackTimedOut, published, 
                                       leftAdvertising, upAtLeave, 
                                       pruneApplied, pruneFetched, unfetched, 
                                       todo, cur, nudges, cafter, early >>

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
                                               logQ, fetchCount, removals, 
                                               removedAt, removedTo, listed, 
                                               listKnown, relisted, redrainReq, 
                                               certIssued, supDaemon, stopped, 
                                               leaving, retryGen, confReq, 
                                               leaseReq, drained, lock, 
                                               cancelReg, cctx, cancelled, 
                                               rmRec, unpruned, gen, res, mark, 
                                               lrec, leaveD, leaveEnd, settled, 
                                               drainedGen, everInPrefs, 
                                               lockedDrained, addAfterDrained, 
                                               startedDrained, 
                                               startDrainFailed, drainApplied, 
                                               ackMade, ackCounted, 
                                               ackTimedOut, published, 
                                               leftAdvertising, upAtLeave, 
                                               pruneApplied, pruneFetched, 
                                               unfetched, stack, svc, todo, 
                                               cur, nudges, cafter >>

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
                                      refused, dirty, logQ, fetchCount, 
                                      removals, removedAt, removedTo, listed, 
                                      listKnown, relisted, redrainReq, 
                                      certIssued, supDaemon, stopped, leaving, 
                                      retryGen, confReq, leaseReq, drained, 
                                      lock, cancelReg, cctx, cancelled, rmRec, 
                                      unpruned, gen, res, mark, lrec, leaveD, 
                                      leaveEnd, settled, drainedGen, 
                                      everInPrefs, lockedDrained, 
                                      addAfterDrained, startedDrained, 
                                      startDrainFailed, drainApplied, ackMade, 
                                      ackCounted, ackTimedOut, published, 
                                      leftAdvertising, upAtLeave, pruneApplied, 
                                      pruneFetched, unfetched, stack, svc, 
                                      todo, cur, nudges, cafter >>

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
                               dirty, logQ, fetchCount, removals, removedAt, 
                               removedTo, listed, listKnown, relisted, 
                               redrainReq, certIssued, supDaemon, stopped, 
                               leaving, retryGen, confReq, leaseReq, drained, 
                               lock, cancelReg, cctx, cancelled, rmRec, 
                               unpruned, gen, res, out, mark, lrec, leaveD, 
                               leaveEnd, settled, drainedGen, everInPrefs, 
                               lockedDrained, addAfterDrained, startedDrained, 
                               startDrainFailed, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, upAtLeave, pruneApplied, 
                               pruneFetched, unfetched, svc, todo, cur, nudges, 
                               cafter, early >>

StopSignal == Stop_signal

Leave_begin == /\ pc["leave"] = "Leave_begin"
               /\ ~published \/ NeedsDrainRead
               /\ IF ~published
                     THEN /\ IF daemonUp /\ Mutation # "abort-first-start"
                                THEN /\ leaving' = TRUE
                                     /\ drained' = TRUE
                                     /\ IF cancelReg # None /\ Cancels
                                           THEN /\ cctx' = [cctx EXCEPT ![cancelReg] = TRUE]
                                                /\ cancelled' = TRUE
                                           ELSE /\ TRUE
                                                /\ UNCHANGED << cctx, 
                                                                cancelled >>
                                     /\ pc' = [pc EXCEPT !["leave"] = "Leave_started"]
                                     /\ UNCHANGED << daemonUp, ctxDone, 
                                                     pending, supDaemon, 
                                                     stopped, leaveEnd, 
                                                     leftAdvertising, 
                                                     upAtLeave >>
                                ELSE /\ leaveEnd' = "aborted"
                                     /\ leftAdvertising' = (daemonUp /\ prefs # {} /\ leaveEnd' # "timeout")
                                     /\ upAtLeave' = daemonUp
                                     /\ stopped' = TRUE
                                     /\ daemonUp' = FALSE
                                     /\ supDaemon' = 0
                                     /\ pending' = {}
                                     /\ ctxDone' = [g \in Gens |-> TRUE]
                                     /\ pc' = [pc EXCEPT !["leave"] = "Done"]
                                     /\ UNCHANGED << leaving, drained, cctx, 
                                                     cancelled >>
                          /\ stack' = stack
                     ELSE /\ IF ~NeedsDrain
                                THEN /\ leaveEnd' = "nodrain"
                                     /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                     /\ stack' = stack
                                ELSE /\ stack' = [stack EXCEPT !["leave"] = << [ procedure |->  "DrainProc",
                                                                                 pc        |->  "Wait_loop" ] >>
                                                                             \o stack["leave"]]
                                     /\ pc' = [pc EXCEPT !["leave"] = "Drain_begin"]
                                     /\ UNCHANGED leaveEnd
                          /\ UNCHANGED << daemonUp, ctxDone, pending, 
                                          supDaemon, stopped, leaving, drained, 
                                          cctx, cancelled, leftAdvertising, 
                                          upAtLeave >>
               /\ UNCHANGED << stateExists, prefs, daemonGen, crashes, waitOn, 
                               refused, dirty, logQ, fetchCount, removals, 
                               removedAt, removedTo, listed, listKnown, 
                               relisted, redrainReq, certIssued, retryGen, 
                               confReq, leaseReq, lock, cancelReg, rmRec, 
                               unpruned, gen, res, out, mark, lrec, leaveD, 
                               settled, drainedGen, everInPrefs, lockedDrained, 
                               addAfterDrained, startedDrained, 
                               startDrainFailed, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               pruneApplied, pruneFetched, unfetched, svc, 
                               todo, cur, nudges, cafter, early >>

Leave_started == /\ pc["leave"] = "Leave_started"
                 /\ published \/ ~daemonUp
                 /\ IF ~published
                       THEN /\ leaveEnd' = "aborted"
                            /\ leftAdvertising' = (daemonUp /\ prefs # {} /\ leaveEnd' # "timeout")
                            /\ upAtLeave' = daemonUp
                            /\ stopped' = TRUE
                            /\ daemonUp' = FALSE
                            /\ supDaemon' = 0
                            /\ pending' = {}
                            /\ ctxDone' = [g \in Gens |-> TRUE]
                            /\ pc' = [pc EXCEPT !["leave"] = "Done"]
                            /\ stack' = stack
                       ELSE /\ IF ~NeedsDrain
                                  THEN /\ leaveEnd' = "nodrain"
                                       /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                       /\ stack' = stack
                                  ELSE /\ stack' = [stack EXCEPT !["leave"] = << [ procedure |->  "DrainProc",
                                                                                   pc        |->  "Wait_loop" ] >>
                                                                               \o stack["leave"]]
                                       /\ pc' = [pc EXCEPT !["leave"] = "Drain_begin"]
                                       /\ UNCHANGED leaveEnd
                            /\ UNCHANGED << daemonUp, ctxDone, pending, 
                                            supDaemon, stopped, 
                                            leftAdvertising, upAtLeave >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, crashes, 
                                 waitOn, refused, dirty, logQ, fetchCount, 
                                 removals, removedAt, removedTo, listed, 
                                 listKnown, relisted, redrainReq, certIssued, 
                                 leaving, retryGen, confReq, leaseReq, drained, 
                                 lock, cancelReg, cctx, cancelled, rmRec, 
                                 unpruned, gen, res, out, mark, lrec, leaveD, 
                                 settled, drainedGen, everInPrefs, 
                                 lockedDrained, addAfterDrained, 
                                 startedDrained, startDrainFailed, 
                                 drainApplied, ackMade, ackCounted, 
                                 ackTimedOut, published, pruneApplied, 
                                 pruneFetched, unfetched, svc, todo, cur, 
                                 nudges, cafter, early >>

Wait_loop == /\ pc["leave"] = "Wait_loop"
             /\ lock = None
             /\ lrec' = rmRec
             /\ IF rmRec.set /\ ~ctxDone[rmRec.gen]
                   THEN /\ leaveD' = rmRec.gen
                        /\ pc' = [pc EXCEPT !["leave"] = "Wait_block"]
                        /\ UNCHANGED << leaveEnd, stack >>
                   ELSE /\ IF Mutation = "always-wait" /\ PublishedRuns
                              THEN /\ leaveD' = supDaemon
                                   /\ pc' = [pc EXCEPT !["leave"] = "Wait_block"]
                                   /\ UNCHANGED << leaveEnd, stack >>
                              ELSE /\ IF ~PublishedRuns /\ (rmRec.set \/ stateExists)
                                         /\ Mutation # "no-follow-restart"
                                         THEN /\ pc' = [pc EXCEPT !["leave"] = "Wait_restart"]
                                              /\ UNCHANGED << leaveEnd, stack >>
                                         ELSE /\ IF PublishedRuns /\ supDaemon # drainedGen
                                                    THEN /\ stack' = [stack EXCEPT !["leave"] = << [ procedure |->  "DrainProc",
                                                                                                     pc        |->  "Wait_loop" ] >>
                                                                                                 \o stack["leave"]]
                                                         /\ pc' = [pc EXCEPT !["leave"] = "Drain_begin"]
                                                         /\ UNCHANGED leaveEnd
                                                    ELSE /\ IF cancelled /\ ~settled /\ Mutation # "no-settle"
                                                               THEN /\ pc' = [pc EXCEPT !["leave"] = "Wait_settle"]
                                                                    /\ UNCHANGED leaveEnd
                                                               ELSE /\ leaveEnd' = IF rmRec.set THEN "stopped" ELSE "nothing"
                                                                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                                                         /\ stack' = stack
                                   /\ UNCHANGED leaveD
             /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                             crashes, pending, waitOn, refused, dirty, logQ, 
                             fetchCount, removals, removedAt, removedTo, 
                             listed, listKnown, relisted, redrainReq, 
                             certIssued, supDaemon, stopped, leaving, retryGen, 
                             confReq, leaseReq, drained, lock, cancelReg, cctx, 
                             cancelled, rmRec, unpruned, gen, res, out, mark, 
                             settled, drainedGen, everInPrefs, lockedDrained, 
                             addAfterDrained, startedDrained, startDrainFailed, 
                             drainApplied, ackMade, ackCounted, ackTimedOut, 
                             published, leftAdvertising, upAtLeave, 
                             pruneApplied, pruneFetched, unfetched, svc, todo, 
                             cur, nudges, cafter, early >>

Wait_block == /\ pc["leave"] = "Wait_block"
              /\ \/ /\ RemovalFetched(lrec, leaveD)
                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_floor"]
                    /\ UNCHANGED <<leaveEnd, ackTimedOut>>
                 \/ /\ rmRec # lrec
                    /\ pc' = [pc EXCEPT !["leave"] = "Wait_loop"]
                    /\ UNCHANGED <<leaveEnd, ackTimedOut>>
                 \/ /\ ctxDone[leaveD]
                    /\ IF Mutation = "no-follow-restart"
                          THEN /\ leaveEnd' = "stopped"
                               /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                          ELSE /\ pc' = [pc EXCEPT !["leave"] = "Wait_loop"]
                               /\ UNCHANGED leaveEnd
                    /\ UNCHANGED ackTimedOut
                 \/ /\ Mutation # "wait-no-timeout" /\ ~RemovalFetched(lrec, leaveD)
                    /\ leaveEnd' = "timeout"
                    /\ ackTimedOut' = ackCounted[leaveD]
                    /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, ctxDone, 
                              crashes, pending, waitOn, refused, dirty, logQ, 
                              fetchCount, removals, removedAt, removedTo, 
                              listed, listKnown, relisted, redrainReq, 
                              certIssued, supDaemon, stopped, leaving, 
                              retryGen, confReq, leaseReq, drained, lock, 
                              cancelReg, cctx, cancelled, rmRec, unpruned, gen, 
                              res, out, mark, lrec, leaveD, settled, 
                              drainedGen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, 
                              startDrainFailed, drainApplied, ackMade, 
                              ackCounted, published, leftAdvertising, 
                              upAtLeave, pruneApplied, pruneFetched, unfetched, 
                              stack, svc, todo, cur, nudges, cafter, early >>

Leave_floor == /\ pc["leave"] = "Leave_floor"
               /\ Settled
               /\ IF rmRec = lrec /\ RemovalFetched(lrec, leaveD)
                     THEN /\ leaveEnd' = "acked"
                          /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                     ELSE /\ pc' = [pc EXCEPT !["leave"] = "Wait_loop"]
                          /\ UNCHANGED leaveEnd
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, logQ, fetchCount, removals, removedAt, 
                               removedTo, listed, listKnown, relisted, 
                               redrainReq, certIssued, supDaemon, stopped, 
                               leaving, retryGen, confReq, leaseReq, drained, 
                               lock, cancelReg, cctx, cancelled, rmRec, 
                               unpruned, gen, res, out, mark, lrec, leaveD, 
                               settled, drainedGen, everInPrefs, lockedDrained, 
                               addAfterDrained, startedDrained, 
                               startDrainFailed, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, upAtLeave, pruneApplied, 
                               pruneFetched, unfetched, stack, svc, todo, cur, 
                               nudges, cafter, early >>

Wait_settle == /\ pc["leave"] = "Wait_settle"
               /\ Settled
               /\ settled' = TRUE
               /\ pc' = [pc EXCEPT !["leave"] = "Wait_loop"]
               /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                               ctxDone, crashes, pending, waitOn, refused, 
                               dirty, logQ, fetchCount, removals, removedAt, 
                               removedTo, listed, listKnown, relisted, 
                               redrainReq, certIssued, supDaemon, stopped, 
                               leaving, retryGen, confReq, leaseReq, drained, 
                               lock, cancelReg, cctx, cancelled, rmRec, 
                               unpruned, gen, res, out, mark, lrec, leaveD, 
                               leaveEnd, drainedGen, everInPrefs, 
                               lockedDrained, addAfterDrained, startedDrained, 
                               startDrainFailed, drainApplied, ackMade, 
                               ackCounted, ackTimedOut, published, 
                               leftAdvertising, upAtLeave, pruneApplied, 
                               pruneFetched, unfetched, stack, svc, todo, cur, 
                               nudges, cafter, early >>

Wait_restart == /\ pc["leave"] = "Wait_restart"
                /\ \/ /\ PublishedRuns \/ rmRec # lrec
                      /\ pc' = [pc EXCEPT !["leave"] = "Wait_loop"]
                      /\ UNCHANGED leaveEnd
                   \/ /\ Mutation # "wait-no-timeout"
                      /\ leaveEnd' = "timeout"
                      /\ pc' = [pc EXCEPT !["leave"] = "Leave_done"]
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, pending, waitOn, refused, 
                                dirty, logQ, fetchCount, removals, removedAt, 
                                removedTo, listed, listKnown, relisted, 
                                redrainReq, certIssued, supDaemon, stopped, 
                                leaving, retryGen, confReq, leaseReq, drained, 
                                lock, cancelReg, cctx, cancelled, rmRec, 
                                unpruned, gen, res, out, mark, lrec, leaveD, 
                                settled, drainedGen, everInPrefs, 
                                lockedDrained, addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Leave_done == /\ pc["leave"] = "Leave_done"
              /\ leftAdvertising' = (daemonUp /\ prefs # {} /\ leaveEnd # "timeout")
              /\ upAtLeave' = daemonUp
              /\ stopped' = TRUE
              /\ daemonUp' = FALSE
              /\ supDaemon' = 0
              /\ pending' = {}
              /\ ctxDone' = [g \in Gens |-> TRUE]
              /\ pc' = [pc EXCEPT !["leave"] = "Done"]
              /\ UNCHANGED << stateExists, prefs, daemonGen, crashes, waitOn, 
                              refused, dirty, logQ, fetchCount, removals, 
                              removedAt, removedTo, listed, listKnown, 
                              relisted, redrainReq, certIssued, leaving, 
                              retryGen, confReq, leaseReq, drained, lock, 
                              cancelReg, cctx, cancelled, rmRec, unpruned, gen, 
                              res, out, mark, lrec, leaveD, leaveEnd, settled, 
                              drainedGen, everInPrefs, lockedDrained, 
                              addAfterDrained, startedDrained, 
                              startDrainFailed, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, pruneApplied, 
                              pruneFetched, unfetched, stack, svc, todo, cur, 
                              nudges, cafter, early >>

Leave == Leave_begin \/ Leave_started \/ Wait_loop \/ Wait_block
            \/ Leave_floor \/ Wait_settle \/ Wait_restart \/ Leave_done

Redrain_idle == /\ pc["redrain"] = "Redrain_idle"
                /\ IF ~stopped
                      THEN /\ \E g \in Gens : redrainReq[g]
                           /\ \E g \in {x \in Gens : redrainReq[x]}:
                                /\ redrainReq' = [redrainReq EXCEPT ![g] = FALSE]
                                /\ gen' = [gen EXCEPT !["redrain"] = g]
                           /\ pc' = [pc EXCEPT !["redrain"] = "Redrain_lock"]
                      ELSE /\ pc' = [pc EXCEPT !["redrain"] = "Done"]
                           /\ UNCHANGED << redrainReq, gen >>
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, pending, waitOn, refused, 
                                dirty, logQ, fetchCount, removals, removedAt, 
                                removedTo, listed, listKnown, relisted, 
                                certIssued, supDaemon, stopped, leaving, 
                                retryGen, confReq, leaseReq, drained, lock, 
                                cancelReg, cctx, cancelled, rmRec, unpruned, 
                                res, out, mark, lrec, leaveD, leaveEnd, 
                                settled, drainedGen, everInPrefs, 
                                lockedDrained, addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Redrain_lock == /\ pc["redrain"] = "Redrain_lock"
                /\ lock = None
                /\ IF ~Live("redrain") \/ prefs = {}
                      THEN /\ pc' = [pc EXCEPT !["redrain"] = "Redrain_idle"]
                           /\ UNCHANGED << pending, waitOn, lock, rmRec >>
                      ELSE /\ lock' = "redrain"
                           /\ rmRec' = [set |-> TRUE, gen |-> (gen["redrain"]), removals |-> removals[(gen["redrain"])],
                                        leaves |-> ({}), start |-> FALSE, fetches |-> fetchCount[(gen["redrain"])]]
                           /\ LET r == [from |-> "redrain", gen |-> gen["redrain"], val |-> {},
                                        kind |-> "drain"] IN
                                /\ pending' = (pending \cup {r})
                                /\ waitOn' = [waitOn EXCEPT !["redrain"] = r]
                           /\ pc' = [pc EXCEPT !["redrain"] = "Redrain_sent"]
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, refused, dirty, logQ, 
                                fetchCount, removals, removedAt, removedTo, 
                                listed, listKnown, relisted, redrainReq, 
                                certIssued, supDaemon, stopped, leaving, 
                                retryGen, confReq, leaseReq, drained, 
                                cancelReg, cctx, cancelled, unpruned, gen, res, 
                                out, mark, lrec, leaveD, leaveEnd, settled, 
                                drainedGen, everInPrefs, lockedDrained, 
                                addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Redrain_sent == /\ pc["redrain"] = "Redrain_sent"
                /\ \/ /\ Applied("redrain")
                   \/ /\ ~Live("redrain")
                   \/ /\ TRUE
                /\ waitOn' = [waitOn EXCEPT !["redrain"] = NoReq]
                /\ lock' = None
                /\ pc' = [pc EXCEPT !["redrain"] = "Redrain_idle"]
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, pending, refused, dirty, 
                                logQ, fetchCount, removals, removedAt, 
                                removedTo, listed, listKnown, relisted, 
                                redrainReq, certIssued, supDaemon, stopped, 
                                leaving, retryGen, confReq, leaseReq, drained, 
                                cancelReg, cctx, cancelled, rmRec, unpruned, 
                                gen, res, out, mark, lrec, leaveD, leaveEnd, 
                                settled, drainedGen, everInPrefs, 
                                lockedDrained, addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Redrain == Redrain_idle \/ Redrain_lock \/ Redrain_sent

Tailscaled_answer == /\ pc["tailscaled"] = "Tailscaled_answer"
                     /\ IF ~stopped
                           THEN /\ \/ /\ \E r \in {q \in pending : daemonUp /\ q.gen = daemonGen}:
                                           /\ IF r.val # prefs
                                                 THEN /\ dirty' = TRUE
                                                      /\ logQ' = [logQ EXCEPT ![r.gen] =            Append(
                                                                                         IF r.val # {}
                                                                                         THEN [i \in DOMAIN logQ[r.gen] |->
                                                                                                 [logQ[r.gen][i] EXCEPT !.ack = FALSE]]
                                                                                         ELSE logQ[r.gen],
                                                                                         EditLine(r.val))]
                                                 ELSE /\ TRUE
                                                      /\ UNCHANGED << dirty, 
                                                                      logQ >>
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
                                                 ELSE /\ IF r.kind = "drain"
                                                            THEN /\ ackMade' = (ackMade \/ ~dirty')
                                                            ELSE /\ TRUE
                                                                 /\ UNCHANGED ackMade
                                                      /\ UNCHANGED ackCounted
                                      /\ UNCHANGED refused
                                   \/ /\ \E r \in {q \in pending : daemonUp /\ q.gen = daemonGen
                                                                 /\ q.kind = "add"
                                                                 /\ waitOn[q.from] = q}:
                                           /\ refused' = [refused EXCEPT ![r.from] = TRUE]
                                           /\ pending' = pending \ {r}
                                      /\ UNCHANGED <<prefs, dirty, logQ, everInPrefs, drainApplied, ackMade, ackCounted, pruneApplied, pruneFetched, unfetched>>
                                /\ pc' = [pc EXCEPT !["tailscaled"] = "Tailscaled_answer"]
                           ELSE /\ pc' = [pc EXCEPT !["tailscaled"] = "Done"]
                                /\ UNCHANGED << prefs, pending, refused, dirty, 
                                                logQ, everInPrefs, 
                                                drainApplied, ackMade, 
                                                ackCounted, pruneApplied, 
                                                pruneFetched, unfetched >>
                     /\ UNCHANGED << stateExists, daemonGen, daemonUp, ctxDone, 
                                     crashes, waitOn, fetchCount, removals, 
                                     removedAt, removedTo, listed, listKnown, 
                                     relisted, redrainReq, certIssued, 
                                     supDaemon, stopped, leaving, retryGen, 
                                     confReq, leaseReq, drained, lock, 
                                     cancelReg, cctx, cancelled, rmRec, 
                                     unpruned, gen, res, out, mark, lrec, 
                                     leaveD, leaveEnd, settled, drainedGen, 
                                     lockedDrained, addAfterDrained, 
                                     startedDrained, startDrainFailed, 
                                     ackTimedOut, published, leftAdvertising, 
                                     upAtLeave, stack, svc, todo, cur, nudges, 
                                     cafter, early >>

Tailscaled == Tailscaled_answer

Orphans_drop == /\ pc["orphans"] = "Orphans_drop"
                /\ IF ~stopped
                      THEN /\ \E r \in pending : r.kind = "add" /\ Orphan(r)
                           /\ \E r \in {q \in pending : q.kind = "add" /\ Orphan(q)}:
                                pending' = pending \ {r}
                           /\ pc' = [pc EXCEPT !["orphans"] = "Orphans_drop"]
                      ELSE /\ pc' = [pc EXCEPT !["orphans"] = "Done"]
                           /\ UNCHANGED pending
                /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                ctxDone, crashes, waitOn, refused, dirty, logQ, 
                                fetchCount, removals, removedAt, removedTo, 
                                listed, listKnown, relisted, redrainReq, 
                                certIssued, supDaemon, stopped, leaving, 
                                retryGen, confReq, leaseReq, drained, lock, 
                                cancelReg, cctx, cancelled, rmRec, unpruned, 
                                gen, res, out, mark, lrec, leaveD, leaveEnd, 
                                settled, drainedGen, everInPrefs, 
                                lockedDrained, addAfterDrained, startedDrained, 
                                startDrainFailed, drainApplied, ackMade, 
                                ackCounted, ackTimedOut, published, 
                                leftAdvertising, upAtLeave, pruneApplied, 
                                pruneFetched, unfetched, stack, svc, todo, cur, 
                                nudges, cafter, early >>

Orphans == Orphans_drop

Control_fetch == /\ pc["control"] = "Control_fetch"
                 /\ IF ~stopped
                       THEN /\ daemonUp /\ dirty
                            /\ dirty' = FALSE
                            /\ logQ' = [logQ EXCEPT ![daemonGen] = Append(logQ[daemonGen], FetchLine(Ack))]
                            /\ ackMade' = (ackMade \/ Ack)
                            /\ pruneFetched' = (pruneFetched \/ pruneApplied)
                            /\ unfetched' = {}
                            /\ pc' = [pc EXCEPT !["control"] = "Control_fetch"]
                       ELSE /\ pc' = [pc EXCEPT !["control"] = "Done"]
                            /\ UNCHANGED << dirty, logQ, ackMade, pruneFetched, 
                                            unfetched >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 fetchCount, removals, removedAt, removedTo, 
                                 listed, listKnown, relisted, redrainReq, 
                                 certIssued, supDaemon, stopped, leaving, 
                                 retryGen, confReq, leaseReq, drained, lock, 
                                 cancelReg, cctx, cancelled, rmRec, unpruned, 
                                 gen, res, out, mark, lrec, leaveD, leaveEnd, 
                                 settled, drainedGen, everInPrefs, 
                                 lockedDrained, addAfterDrained, 
                                 startedDrained, startDrainFailed, 
                                 drainApplied, ackCounted, ackTimedOut, 
                                 published, leftAdvertising, upAtLeave, 
                                 pruneApplied, stack, svc, todo, cur, nudges, 
                                 cafter, early >>

Control == Control_fetch

Counter_count == /\ pc["counter"] = "Counter_count"
                 /\ IF ~stopped
                       THEN /\ \E g \in {x \in Gens : logQ[x] # <<>> /\ (Reads(x) \/ RelistLine(x))}:
                                 LET e == Head(logQ[g]) IN
                                   /\ IF e.k = "fetch"
                                         THEN /\ fetchCount' = [fetchCount EXCEPT ![g] = fetchCount[g] + 1]
                                              /\ ackCounted' = [ackCounted EXCEPT ![g] = ackCounted[g] \/ e.ack]
                                              /\ UNCHANGED << removals, 
                                                              removedAt, 
                                                              removedTo, 
                                                              listed, 
                                                              listKnown, 
                                                              relisted, 
                                                              redrainReq >>
                                         ELSE /\ IF ~listKnown[g] \/ listed[g] \ e.val # {}
                                                    THEN /\ removals' = [removals EXCEPT ![g] = removals[g] + 1]
                                                         /\ removedAt' = [removedAt EXCEPT ![g] = fetchCount[g]]
                                                         /\ removedTo' = [removedTo EXCEPT ![g] = e.val]
                                                    ELSE /\ TRUE
                                                         /\ UNCHANGED << removals, 
                                                                         removedAt, 
                                                                         removedTo >>
                                              /\ listed' = [listed EXCEPT ![g] = e.val]
                                              /\ listKnown' = [listKnown EXCEPT ![g] = TRUE]
                                              /\ relisted' = [relisted EXCEPT ![g] = drained /\ e.val # {}]
                                              /\ IF drained /\ e.val # {} /\ ~ctxDone[g]
                                                    /\ Mutation # "no-redrain"
                                                    THEN /\ redrainReq' = [redrainReq EXCEPT ![g] = TRUE]
                                                    ELSE /\ TRUE
                                                         /\ UNCHANGED redrainReq
                                              /\ UNCHANGED << fetchCount, 
                                                              ackCounted >>
                                   /\ logQ' = [logQ EXCEPT ![g] = Tail(logQ[g])]
                            /\ pc' = [pc EXCEPT !["counter"] = "Counter_count"]
                       ELSE /\ pc' = [pc EXCEPT !["counter"] = "Done"]
                            /\ UNCHANGED << logQ, fetchCount, removals, 
                                            removedAt, removedTo, listed, 
                                            listKnown, relisted, redrainReq, 
                                            ackCounted >>
                 /\ UNCHANGED << stateExists, prefs, daemonGen, daemonUp, 
                                 ctxDone, crashes, pending, waitOn, refused, 
                                 dirty, certIssued, supDaemon, stopped, 
                                 leaving, retryGen, confReq, leaseReq, drained, 
                                 lock, cancelReg, cctx, cancelled, rmRec, 
                                 unpruned, gen, res, out, mark, lrec, leaveD, 
                                 leaveEnd, settled, drainedGen, everInPrefs, 
                                 lockedDrained, addAfterDrained, 
                                 startedDrained, startDrainFailed, 
                                 drainApplied, ackMade, ackTimedOut, published, 
                                 leftAdvertising, upAtLeave, pruneApplied, 
                                 pruneFetched, unfetched, stack, svc, todo, 
                                 cur, nudges, cafter, early >>

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
                              refused, logQ, fetchCount, removals, removedAt, 
                              removedTo, listed, listKnown, relisted, 
                              redrainReq, certIssued, supDaemon, stopped, 
                              leaving, retryGen, confReq, leaseReq, drained, 
                              lock, cancelReg, cctx, cancelled, rmRec, 
                              unpruned, gen, res, out, mark, lrec, leaveD, 
                              leaveEnd, settled, drainedGen, everInPrefs, 
                              lockedDrained, addAfterDrained, startedDrained, 
                              startDrainFailed, drainApplied, ackMade, 
                              ackCounted, ackTimedOut, published, 
                              leftAdvertising, upAtLeave, pruneApplied, 
                              pruneFetched, unfetched, stack, svc, todo, cur, 
                              nudges, cafter, early >>

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
                              crashes, pending, waitOn, refused, dirty, logQ, 
                              fetchCount, removals, removedAt, removedTo, 
                              listed, listKnown, relisted, redrainReq, 
                              supDaemon, stopped, leaving, retryGen, confReq, 
                              leaseReq, drained, lock, cancelReg, cctx, 
                              cancelled, rmRec, unpruned, gen, res, out, mark, 
                              lrec, leaveD, leaveEnd, settled, drainedGen, 
                              everInPrefs, lockedDrained, addAfterDrained, 
                              startedDrained, startDrainFailed, drainApplied, 
                              ackMade, ackCounted, ackTimedOut, published, 
                              leftAdvertising, upAtLeave, pruneApplied, 
                              pruneFetched, unfetched, stack, svc, todo, cur, 
                              nudges, cafter, early >>

Issuer == Cert_issue

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == Supervisor \/ Retry \/ StopSignal \/ Leave \/ Redrain \/ Tailscaled
           \/ Orphans \/ Control \/ Counter \/ Crash \/ Issuer
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
        /\ WF_vars(Redrain)
        /\ WF_vars(Orphans)
        /\ WF_vars(Counter)

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION

\* Once Leave stopped the supervisor, nothing the properties read changes:
\* the cfgs explore no step after it (ACTION_CONSTRAINT Running), which
\* leaves out the other processes' ways to their end.
Running == ~stopped

\* The state as far as the fetch counts matter (VIEW CountView, in the
\* cfgs that enable the confirmer, pruneServices or the lease holder): a
\* count read from daemon g's d.fetches is only ever compared with
\* d.fetches of the same daemon, which only grows. So what decides every
\* later step is whether a fetch was counted since the read, not the
\* numbers: states that agree on that are the same to every property.
\* (Not for count-before-edit, whose cfg uses no VIEW.)
Since(v, g) ==
    IF g = 0 THEN <<"raw", v>> ELSE <<"since", fetchCount[g] > v>>
CountView ==
    << stateExists, prefs, daemonGen, daemonUp, ctxDone, crashes, pending,
       waitOn, refused, dirty, logQ, [g \in Gens |-> fetchCount[g] > 0],
       removals, [g \in Gens |-> Since(removedAt[g], g)], removedTo, listed,
       listKnown, relisted, redrainReq, certIssued, supDaemon, stopped,
       leaving, retryGen,
       [s \in Services |-> [gen |-> confReq[s].gen,
                            after |-> Since(confReq[s].after, confReq[s].gen)]],
       leaseReq, drained, lock, cancelReg, cctx, cancelled,
       [rmRec EXCEPT !.fetches = Since(rmRec.fetches, rmRec.gen)],
       unpruned, gen, res, out,
       [a \in Actors |-> Since(mark[a], gen[a])],
       [lrec EXCEPT !.fetches = Since(lrec.fetches, lrec.gen)],
       leaveD, leaveEnd, settled, drainedGen, everInPrefs, lockedDrained, addAfterDrained,
       startedDrained, startDrainFailed, drainApplied, ackMade, ackCounted,
       ackTimedOut, published, leftAdvertising, upAtLeave, pruneApplied,
       pruneFetched, unfetched, pc, stack, svc, todo, cur, nudges,
       [c \in Confs |-> Since(cafter[c], gen[c])], early >>

\* L3: a confirmer re-advertises at most advertiseNudges times, and stops
\* (on a fetch, a drain, its daemon's end, or after the nudges). After the
\* translation: it reads the confirmers' own variable nudges
ConfirmerStops ==
    /\ [](\A c \in Confs : nudges[c] <= MaxNudges)
    /\ \A c \in Confs :
           pc[c] # "Confirm_idle" ~> pc[c] \in {"Confirm_idle", "Done"}
====
