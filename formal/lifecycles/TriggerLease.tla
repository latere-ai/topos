----------------------------- MODULE TriggerLease -----------------------------
(***************************************************************************)
(* The lease that serializes a trigger's firings across replicas           *)
(* (LeaseTrigger, and the renewal in triggers.Engine.locked). A replica    *)
(* takes the lease when it is free, lapsed or its own, sets it Lease       *)
(* seconds ahead, works while it holds it, renews every Renew seconds, and *)
(* releases it when the work ends. A renewal can fail, as a store error    *)
(* does, and the work goes on: the engine logs the failure and keeps       *)
(* working. Time is in whole seconds and every replica reads one clock.    *)
(* Lease and Renew are LeaseTTL and a quarter of it, rendered from the Go  *)
(* constants into TriggerLeaseConstants.tla by TestTriggerLeaseConstants.  *)
(***************************************************************************)
EXTENDS Integers, TriggerLeaseConstants

CONSTANTS Replicas, Horizon, MaxFail

None == "none"

VARIABLES
    clock,    \* seconds since the start
    holder,   \* the replica the lease row names, or None
    expires,  \* the clock at which the lease lapses
    working,  \* replica -> TRUE while it works under the lease
    due,      \* replica -> the clock its next renewal is due at
    fails     \* replica -> consecutive failed renewals

vars == <<clock, holder, expires, working, due, fails>>

Init == /\ clock = 0
        /\ holder = None
        /\ expires = 0
        /\ working = [r \in Replicas |-> FALSE]
        /\ due = [r \in Replicas |-> 0]
        /\ fails = [r \in Replicas |-> 0]

Takes(r) == holder = r \/ holder = None \/ expires <= clock

Acquire(r) ==
    /\ ~working[r]
    /\ Takes(r)
    /\ holder' = r
    /\ expires' = clock + Lease
    /\ working' = [working EXCEPT ![r] = TRUE]
    /\ due' = [due EXCEPT ![r] = clock + Renew]
    /\ fails' = [fails EXCEPT ![r] = 0]
    /\ UNCHANGED clock

\* A renewal attempt: it succeeds when the lease is the replica's or lapsed,
\* and may fail with a store error while fewer than MaxFail in a row did.
Extend(r) ==
    /\ working[r]
    /\ clock >= due[r]
    /\ \/ /\ Takes(r)
          /\ holder' = r
          /\ expires' = clock + Lease
          /\ fails' = [fails EXCEPT ![r] = 0]
       \/ /\ fails[r] < MaxFail
          /\ fails' = [fails EXCEPT ![r] = fails[r] + 1]
          /\ UNCHANGED <<holder, expires>>
    /\ due' = [due EXCEPT ![r] = clock + Renew]
    /\ UNCHANGED <<clock, working>>

\* The work ends and the replica releases the lease it holds.
Finish(r) ==
    /\ working[r]
    /\ working' = [working EXCEPT ![r] = FALSE]
    /\ IF holder = r THEN holder' = None ELSE UNCHANGED holder
    /\ UNCHANGED <<clock, expires, due, fails>>

\* A second passes, unless a renewal is due.
Tick ==
    /\ clock < Horizon
    /\ \A r \in Replicas : working[r] => clock < due[r]
    /\ clock' = clock + 1
    /\ UNCHANGED <<holder, expires, working, due, fails>>

Stop == clock = Horizon /\ UNCHANGED vars

Next == Tick \/ Stop \/ \E r \in Replicas : Acquire(r) \/ Extend(r) \/ Finish(r)

Spec == Init /\ [][Next]_vars

\* No two replicas work under the lease at once.
Exclusive == \A r, s \in Replicas : (working[r] /\ working[s]) => r = s

TypeOK == /\ clock \in 0..Horizon
          /\ holder \in Replicas \cup {None}

\* Failed renewals in a row the lease survives: the next attempt after them
\* still runs before the lease lapses. TriggerLease.cfg uses it.
SafeFails == (Lease - 1) \div Renew - 1

\* One more than that: the lease lapses while the first replica still works.
\* TriggerLease.overlong.cfg uses it.
OverlongFails == SafeFails + 1
=============================================================================
