import Machines.Archive

/-!
Claims about the session archive table, proved over the table that
`session.ArchiveTable` renders into `Machines/Archive.lean`.
-/

namespace Proofs.Archive
open Machines.Archive Statemachine

/-- Neither state is final: an archived session can be returned and an
active one filed away. -/
theorem neither_state_is_final :
    ¬ machine.Terminal .active ∧ ¬ machine.Terminal .archived := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Archive
