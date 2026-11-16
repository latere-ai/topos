import Machines.Run

/-!
Claims about the trigger run table, proved over the table that
`internal/store.RunTable` renders into `Machines/Run.lean`.
-/

namespace Proofs.Run
open Machines.Run Statemachine

/-- An ended run stays ended: nothing follows `ended`, so its settled cost
is never written over. -/
theorem ended_stays (s : State) (r : Reaches machine .ended s) : s = .ended :=
  machine.terminal_stays terminal_ended r

/-- Only an open run ends: every predecessor of `ended` is `open`. -/
theorem ended_only_from_open :
    ∀ s ∈ machine.states, State.ended ∈ machine.next s → s = .open := by
  decide

end Proofs.Run
