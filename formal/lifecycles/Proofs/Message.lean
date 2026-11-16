import Machines.Message

/-!
Claims about the waiting message table, proved over the table that
`session.MessageTable` renders into `Machines/Message.lean`.
-/

namespace Proofs.Message
open Machines.Message Statemachine

/-- A withdrawn message is never read: nothing follows `withdrawn`. -/
theorem withdrawn_never_read (s : State) (r : Reaches machine .withdrawn s) :
    s ≠ .read := by
  have := machine.terminal_stays terminal_withdrawn r
  subst this; decide

/-- A read message is never withdrawn: nothing follows `read`. -/
theorem read_never_withdrawn (s : State) (r : Reaches machine .read s) :
    s ≠ .withdrawn := by
  have := machine.terminal_stays terminal_read r
  subst this; decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Message
