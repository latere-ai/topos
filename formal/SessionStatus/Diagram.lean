-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

/-!
# The session status diagram

A session is `idle`, `running` or `ended`, and an idle or ended session
carries the reason it stopped (spec 004, `session/session.go`). A log
records each change as a `session.status` event. This file states which
status may follow which, and proves what the diagram promises on its own:
`ended` is the only status nothing follows, so in a log that keeps the
diagram an ended session stays ended.

The diagram of spec 004 lists `failed`, `canceled` and `expired` for
`running → ended`. Its table also names `completed` for a session that
`end_on_idle` ends, and the harness writes that status straight after the
turn's `running` (`harness/harness.go`, `turn.finish`), so the edge
`running → ended completed` is part of the relation here.
-/

namespace SessionStatus

/-- Why an idle session waits: the stop reasons of `session/session.go`. -/
inductive IdleReason where
  | endTurn | toolConfirmation | toolResult | question | budget
  | turnLimit | outputLimit | interrupted | error | machineWaiting
  deriving DecidableEq, Repr

/-- Why a session ended. -/
inductive EndReason where
  | completed | failed | canceled | expired
  deriving DecidableEq, Repr

/-- A session's status with its stop reason. Running has none, and an idle
session has none only before its first turn: no event writes `idle none`. -/
inductive Mark where
  | idle (reason : Option IdleReason)
  | running
  | ended (reason : EndReason)
  deriving DecidableEq, Repr

def isIdle : Mark → Bool
  | .idle _ => true
  | _ => false

def isEnded : Mark → Bool
  | .ended _ => true
  | _ => false

/-- `edge a b`: status `b` may follow status `a` (spec 004). -/
def edge : Mark → Mark → Bool
  | .idle _, .running => true              -- a runner claims pending input
  | .running, .running => true             -- a new runner claims after a lost lease
  | .running, .idle (some _) => true       -- the turn stops for a reason
  | .running, .ended _ => true             -- failed, canceled, expired; completed by end_on_idle
  | .idle _, .ended .completed => true
  | .idle _, .ended .canceled => true
  | .idle _, .ended .expired => true
  | _, _ => false

/-- The status a log leaves the session in. Logs are newest first, and a
session no status event has moved yet is idle with no reason. -/
def current : List Mark → Mark
  | [] => .idle none
  | m :: _ => m

/-- Every status of the log follows the one before it by an edge. -/
def valid : List Mark → Bool
  | [] => true
  | m :: rest => edge (current rest) m && valid rest

theorem valid_cons {m : Mark} {rest : List Mark} :
    valid (m :: rest) = true ↔ edge (current rest) m = true ∧ valid rest = true := by
  simp [valid]

/-- Nothing follows an ended session. -/
theorem edge_from_ended (r : EndReason) (m : Mark) : edge (.ended r) m = false := by
  cases m <;> rfl

/-- `ended` is the only status with no way out: idle and running sessions
can always be claimed. -/
theorem final_iff_ended (m : Mark) : (∀ m', edge m m' = false) ↔ isEnded m = true := by
  cases m with
  | idle r => exact ⟨fun h => absurd (h .running) (by simp [edge]), fun h => by simp [isEnded] at h⟩
  | running => exact ⟨fun h => absurd (h .running) (by simp [edge]), fun h => by simp [isEnded] at h⟩
  | ended r => exact ⟨fun _ => rfl, fun _ m' => edge_from_ended r m'⟩

/-- No status event of a valid log leaves a session idle with no reason. -/
theorem idle_has_a_reason : ∀ {log : List Mark}, valid log = true → .idle none ∉ log
  | [], _ => by simp
  | m :: rest, h => by
    rw [valid_cons] at h
    intro hm
    cases hm with
    | head => cases hc : current rest <;> simp [hc, edge] at h
    | tail _ hr => exact idle_has_a_reason h.2 hr

/-- In a valid log an ended status is the newest event: nothing was
appended after it. -/
theorem ended_is_last : ∀ {m : Mark} {rest : List Mark} {r : EndReason},
    valid (m :: rest) = true → .ended r ∉ rest
  | _, [], _, _ => by simp
  | m, p :: rest, r, h => by
    rw [valid_cons] at h
    intro hr
    cases hr with
    | head => simp [current, edge_from_ended] at h
    | tail _ hr => exact ended_is_last h.2 hr

/-- A valid log that ever ended leaves the session ended, with the reason
it ended for. -/
theorem stays_ended {log : List Mark} {r : EndReason}
    (h : valid log = true) (hr : .ended r ∈ log) : current log = .ended r := by
  cases log with
  | nil => simp at hr
  | cons m rest =>
    cases hr with
    | head => rfl
    | tail _ hr => exact absurd hr (ended_is_last h)

end SessionStatus
