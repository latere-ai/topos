import Machines.Firing

/-!
Claims about the trigger firing table, proved over the table that
`internal/store.FiringTable` renders into `Machines/Firing.lean`.
-/

namespace Proofs.Firing
open Machines.Firing Statemachine

/-- A firing that started a session is final: nothing follows `started`, so
a retaken firing never starts a second session. -/
theorem started_stays (s : State) (r : Reaches machine .started s) : s = .started :=
  machine.terminal_stays terminal_started r

/-- A firing that continued a session is final. -/
theorem continued_stays (s : State) (r : Reaches machine .continued s) : s = .continued :=
  machine.terminal_stays terminal_continued r

/-- Only a failed, refused or budget-skipped firing is claimed again: every
predecessor of `claimed` is one of the three. -/
theorem claimed_only_after_a_retryable_outcome :
    ∀ s ∈ machine.states, State.claimed ∈ machine.next s →
      s = .failed ∨ s = .refused ∨ s = .skipped_budget := by
  decide

/-- Only a claimed firing is held. -/
theorem held_only_from_claimed :
    ∀ s ∈ machine.states, State.held ∈ machine.next s → s = .claimed := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Firing
