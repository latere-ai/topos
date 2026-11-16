import Machines.Session

/-!
Claims about the session status table, proved over the table that
`session.StatusTable` renders into `Machines/Session.lean`. The hand-written
model one directory up, `SessionStatus`, proves the same diagram holds under
the concurrent writers of a session's log.
-/

namespace Proofs.Session
open Machines.Session Statemachine

/-- An ended session stays ended: nothing follows `ended`. -/
theorem ended_stays_ended (s : State) (r : Reaches machine .ended s) : s = .ended :=
  machine.terminal_stays terminal_ended r

/-- A session that ended is never running again. -/
theorem ended_never_running (s : State) (r : Reaches machine .ended s) :
    s ≠ .running := by
  have := machine.terminal_stays terminal_ended r
  subst this; decide

/-- Only an idle session is claimed: every predecessor of `running` is
`idle`. -/
theorem running_only_from_idle :
    ∀ s ∈ machine.states, State.running ∈ machine.next s → s = .idle := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Session
