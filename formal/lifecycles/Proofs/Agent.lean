import Machines.Agent

/-!
Claims about the agent table, proved over the table that
`internal/store.AgentTable` renders into `Machines/Agent.lean`.
-/

namespace Proofs.Agent
open Machines.Agent Statemachine

/-- A deleted agent stays deleted: nothing follows `deleted`. -/
theorem deleted_stays (s : State) (r : Reaches machine .deleted s) : s = .deleted :=
  machine.terminal_stays terminal_deleted r

/-- An archived agent is never active again. -/
theorem archived_never_active (s : State) (r : Reaches machine .archived s) :
    s ≠ .active := by
  have := machine.reaches_closed (I := [State.archived, .deleted]) (by decide) r (by decide)
  intro h; subst h; simp at this

/-- Only an active agent is archived: every predecessor of `archived` is
`active`. -/
theorem archived_only_from_active :
    ∀ s ∈ machine.states, State.archived ∈ machine.next s → s = .active := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Agent
