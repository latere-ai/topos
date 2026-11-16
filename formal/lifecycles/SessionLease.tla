----------------------------- MODULE SessionLease -----------------------------
(***************************************************************************)
(* The lease of a session (internal/store/postgres Acquire, Renew and the  *)
(* fenced append). A runner takes the lease when no one holds it or it     *)
(* lapsed, which advances the session's generation, renews it every Renew  *)
(* seconds, and appends the session's log until a renewal tells it the     *)
(* lease is lost. A renewal may run late. The store takes an append only   *)
(* from the generation the lease holds while the lease is live. Time is in *)
(* whole seconds and every replica reads the one database clock. Lease and *)
(* Renew are DefaultLeaseTTL and its quarter, rendered from the Go         *)
(* constants into SessionLeaseConstants.tla by TestSessionLeaseConstants.  *)
(***************************************************************************)
EXTENDS Integers, SessionLeaseConstants

CONSTANTS Replicas, Horizon, Fenced

None == "none"

VARIABLES
    clock,    \* seconds since the start
    holder,   \* the replica the lease row names, or None
    gen,      \* the row's generation
    expires,  \* the clock at which the lease lapses
    mine,     \* replica -> the generation it believes it holds, or 0
    due,      \* replica -> the clock its next renewal is due at
    landed    \* the generation of the last append the store took

vars == <<clock, holder, gen, expires, mine, due, landed>>

Init == /\ clock = 0
        /\ holder = None
        /\ gen = 0
        /\ expires = 0
        /\ mine = [r \in Replicas |-> 0]
        /\ due = [r \in Replicas |-> 0]
        /\ landed = 0

Free == holder = None \/ expires < clock

\* Acquire: one statement that takes a free or lapsed lease and advances the
\* generation.
Acquire(r) ==
    /\ mine[r] = 0
    /\ Free
    /\ holder' = r
    /\ gen' = gen + 1
    /\ expires' = clock + Lease
    /\ mine' = [mine EXCEPT ![r] = gen + 1]
    /\ due' = [due EXCEPT ![r] = clock + Renew]
    /\ UNCHANGED <<clock, landed>>

\* Extend: extends the lease when the row still names the runner's
\* generation, and otherwise ends the runner's belief.
Extend(r) ==
    /\ mine[r] > 0
    /\ clock >= due[r]
    /\ IF gen = mine[r]
         THEN /\ expires' = clock + Lease
              /\ UNCHANGED <<holder, gen, mine>>
         ELSE /\ mine' = [mine EXCEPT ![r] = 0]
              /\ UNCHANGED <<holder, gen, expires>>
    /\ due' = [due EXCEPT ![r] = clock + Renew]
    /\ UNCHANGED <<clock, landed>>

\* Append: a runner that believes it holds a generation appends. The store
\* takes the append only from the generation the lease holds, while the lease
\* is live, when Fenced.
Append(r) ==
    /\ mine[r] > 0
    /\ \/ ~Fenced
       \/ mine[r] = gen /\ expires >= clock
    /\ landed' = mine[r]
    /\ UNCHANGED <<clock, holder, gen, expires, mine, due>>

\* A second passes. A renewal runs late by as long as it likes: the fence, not
\* the timing, is what the model relies on.
Tick ==
    /\ clock < Horizon
    /\ clock' = clock + 1
    /\ UNCHANGED <<holder, gen, expires, mine, due, landed>>

Stop == clock = Horizon /\ UNCHANGED vars

Next == Tick \/ Stop \/ \E r \in Replicas : Acquire(r) \/ Extend(r) \/ Append(r)

Spec == Init /\ [][Next]_vars

\* The log never takes an append from an older generation than one it took.
MonotoneLanded == [][landed' >= landed]_vars

TypeOK == /\ clock \in 0..Horizon
          /\ holder \in Replicas \cup {None}
=============================================================================
