import Machines.Idempotency

/-!
Claims about the idempotency record table, proved over the table that
`internal/store.IdempotencyTable` renders into `Machines/Idempotency.lean`.
-/

namespace Proofs.Idempotency
open Machines.Idempotency Statemachine

/-- A stored answer is never a reservation again, so it is never
overwritten by a second answer: `done` has no step to `reserved` or to
itself. -/
theorem done_never_reserved (s : State) (r : Reaches machine .done s) :
    s ≠ .reserved := by
  have := machine.reaches_closed (I := [State.done, .removed]) (by decide) r (by decide)
  intro h; subst h; simp at this

/-- Only a reserved record is finished: every predecessor of `done` is
`reserved`. -/
theorem done_only_from_reserved :
    ∀ s ∈ machine.states, State.done ∈ machine.next s → s = .reserved := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Idempotency
