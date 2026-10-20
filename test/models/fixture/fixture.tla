---- MODULE fixture ----
\* A counter two processes increment under a lock; the runner's self-test.
\* The mutation "skip" takes no lock, so both can pass the check before either
\* increments and the counter exceeds its bound.
EXTENDS Naturals

CONSTANT Mutation

(*--algorithm fixture
variables x = 0, lock = FALSE;

define
    Bounded == x <= 3
end define;

process p \in {1, 2}
variable n = 0;
begin
Loop:
    while n < 2 do
Acquire:
        await Mutation = "skip" \/ ~lock;
        lock := TRUE;
Check:
        if x < 3 then
Inc:
            x := x + 1;
        end if;
Release:
        lock := FALSE;
        n := n + 1;
    end while;
end process;
end algorithm; *)
\* BEGIN TRANSLATION
VARIABLES x, lock, pc

(* define statement *)
Bounded == x <= 3

VARIABLE n

vars == << x, lock, pc, n >>

ProcSet == ({1, 2})

Init == (* Global variables *)
        /\ x = 0
        /\ lock = FALSE
        (* Process p *)
        /\ n = [self \in {1, 2} |-> 0]
        /\ pc = [self \in ProcSet |-> "Loop"]

Loop(self) == /\ pc[self] = "Loop"
              /\ IF n[self] < 2
                    THEN /\ pc' = [pc EXCEPT ![self] = "Acquire"]
                    ELSE /\ pc' = [pc EXCEPT ![self] = "Done"]
              /\ UNCHANGED << x, lock, n >>

Acquire(self) == /\ pc[self] = "Acquire"
                 /\ Mutation = "skip" \/ ~lock
                 /\ lock' = TRUE
                 /\ pc' = [pc EXCEPT ![self] = "Check"]
                 /\ UNCHANGED << x, n >>

Check(self) == /\ pc[self] = "Check"
               /\ IF x < 3
                     THEN /\ pc' = [pc EXCEPT ![self] = "Inc"]
                     ELSE /\ pc' = [pc EXCEPT ![self] = "Release"]
               /\ UNCHANGED << x, lock, n >>

Inc(self) == /\ pc[self] = "Inc"
             /\ x' = x + 1
             /\ pc' = [pc EXCEPT ![self] = "Release"]
             /\ UNCHANGED << lock, n >>

Release(self) == /\ pc[self] = "Release"
                 /\ lock' = FALSE
                 /\ n' = [n EXCEPT ![self] = n[self] + 1]
                 /\ pc' = [pc EXCEPT ![self] = "Loop"]
                 /\ x' = x

p(self) == Loop(self) \/ Acquire(self) \/ Check(self) \/ Inc(self)
              \/ Release(self)

(* Allow infinite stuttering to prevent deadlock on termination. *)
Terminating == /\ \A self \in ProcSet: pc[self] = "Done"
               /\ UNCHANGED vars

Next == (\E self \in {1, 2}: p(self))
           \/ Terminating

Spec == Init /\ [][Next]_vars

Termination == <>(\A self \in ProcSet: pc[self] = "Done")

\* END TRANSLATION
====
