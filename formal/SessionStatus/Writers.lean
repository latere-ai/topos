-- SPDX-FileCopyrightText: 2026 Latere AI
-- SPDX-License-Identifier: Apache-2.0

import SessionStatus.Diagram

/-!
# The writers of a session's status

Four writers append `session.status` events to one session's log:

* a runner, which claims the session (`running`) and ends each turn it
  drives (`runner/runner.go`, `harness/harness.go`);
* the end route, `POST /sessions/{id}/end` (`internal/server/sessions.go`);
* the reaper, which ends an idle session past its `expires_at`
  (`internal/server/reap.go`).

The store checks no status. It takes a batch only when the batch names
the log's last sequence (`after_seq`), and, for a runner's fenced append,
only while the runner still holds the session's lease. A server append
names no lease, so a runner's lease never keeps the end route or the
reaper out.

Every step below is one atomic store operation. A read followed by an
append that names the sequence the read saw is one step, since the store
refuses the append if anything landed in between; a read whose result is
acted on by a later, separate append is its own step. That split is where
the races live.

`shipped` holds the checks the writers make today, and `fixed` the two
changes this model was written to check. The theorems at the end show:

* with `shipped`, an interleaving reaches a log that breaks the diagram,
  in two ways, and each change alone leaves one of them;
* with `fixed`, every interleaving of any number of runners, end requests
  and reaper passes keeps the diagram, so an ended session stays ended.

The status events are the log here: other events change the log's last
sequence too, which only makes more appends fail and be tried again, so
leaving them out allows more interleavings, never fewer.
-/

namespace SessionStatus

/-- A runner of the pool, by index. -/
abbrev Runner := Nat

/-- Where one runner is in driving the session (`Runner.drive`). -/
inductive Phase where
  /-- Holds no lease. -/
  | free
  /-- Holds the lease and reads the session: `drive`'s `Get`. -/
  | checking
  /-- Is about to append `running`: `Runner.running`. -/
  | claiming
  /-- Appended `running`; the harness drives the turn. -/
  | turn
  /-- Appended the turn's idle status and still holds the lease, as while
  a title is asked (`afterTurn`). -/
  | after
  deriving DecidableEq, Repr

/-- The reasons the end route takes. -/
inductive Asked where
  | completed | canceled
  deriving DecidableEq, Repr

def Asked.reason : Asked → EndReason
  | .completed => .completed
  | .canceled => .canceled

/-- One session, its lease, and how far each writer has got. -/
structure World where
  /-- The status events, newest first. -/
  log : List Mark
  /-- The runner that holds the lease. A lease lapses when it is not
  renewed, and the store then refuses its holder's fenced appends. -/
  holder : Option Runner
  phase : Runner → Phase
  /-- How many status events each runner had read when it last read or
  appended: the runner's `Log.last`. -/
  seen : Runner → Nat
  /-- End requests that passed their read of the session and have not
  appended yet. -/
  ends : List Asked

def World.init : World where
  log := []
  holder := none
  phase := fun _ => .free
  seen := fun _ => 0
  ends := []

/-- What others appended since runner `r` last read the log, newest first:
the events `Log.append` reads before it appends. -/
def World.foreign (w : World) (r : Runner) : List Mark :=
  w.log.take (w.log.length - w.seen r)

def World.setPhase (w : World) (r : Runner) (p : Phase) : World :=
  { w with phase := fun x => if x = r then p else w.phase x }

/-- Runner `r` has read the whole log. -/
def World.read (w : World) (r : Runner) : World :=
  { w with seen := fun x => if x = r then w.log.length else w.seen x }

/-- What each writer checks as its append lands. -/
structure Guards where
  /-- `Runner.running`, given what others appended since the runner last
  read the log. -/
  claim : List Mark → Bool
  /-- The end route, given the session's status as its append lands. -/
  endRoute : Mark → Bool

/-- The checks today. `Log.append` appends after whatever it reads, and
the end route's `appendBatch` reads the session again under its append but
refuses only an ended one; its own check that the session is idle comes
from an earlier read. -/
def shipped : Guards where
  claim := fun _ => true
  endRoute := fun m => !isEnded m

/-- The checks with both changes. A claim appends nothing after another
writer's `ended`, and the end route checks that the session is idle on the
read its append follows. -/
def fixed : Guards where
  claim := fun foreign => !foreign.any isEnded
  endRoute := isIdle

/-- Only the claim changed. -/
def claimFixedOnly : Guards := { shipped with claim := fixed.claim }

/-- Only the end route changed. -/
def endFixedOnly : Guards := { shipped with endRoute := fixed.endRoute }

/-- One atomic operation of one writer. -/
inductive Step (g : Guards) : World → World → Prop where
  /-- `Queue.take`: a free runner takes the free lease. -/
  | acquire (w : World) (r : Runner) :
      w.holder = none → w.phase r = .free →
      Step g w { w.setPhase r .checking with holder := some r }
  /-- `drive` reads the session, refuses an ended one, and starts its
  `Log` at the sequence it read. -/
  | check (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .checking → isEnded (current w.log) = false →
      Step g w ((w.setPhase r .claiming).read r)
  | checkEnded (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .checking → isEnded (current w.log) = true →
      Step g w { w.setPhase r .free with holder := none }
  /-- `Runner.running` appends `running` after what others appended. -/
  | claim (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .claiming → g.claim (w.foreign r) = true →
      Step g w (({ w with log := .running :: w.log }.setPhase r .turn).read r)
  | claimRefused (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .claiming → g.claim (w.foreign r) = false →
      Step g w { w.setPhase r .free with holder := none }
  /-- `turn.finish` stops the turn idle. -/
  | finishIdle (w : World) (r : Runner) (why : IdleReason) :
      w.holder = some r → w.phase r = .turn →
      Step g w (({ w with log := .idle (some why) :: w.log }.setPhase r .after).read r)
  /-- `turn.finish` ends the session when `end_on_idle` applies, and
  `drive` returns. -/
  | finishEnded (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .turn →
      Step g w { { w with log := .ended .completed :: w.log }.setPhase r .free with holder := none }
  /-- Input arrived during the turn or after it: `drive` claims again. -/
  | again (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .after →
      Step g w (w.setPhase r .claiming)
  /-- Nothing is pending: `drive` returns and the lease is released. -/
  | done (w : World) (r : Runner) :
      w.holder = some r → w.phase r = .after →
      Step g w { w.setPhase r .free with holder := none }
  /-- The runner's next append fails with `ErrLeaseLost`, and it stops. -/
  | fenced (w : World) (r : Runner) :
      w.holder ≠ some r → w.phase r ≠ .free →
      Step g w (w.setPhase r .free)
  /-- The lease lapses. -/
  | lapse (w : World) :
      Step g w { w with holder := none }
  /-- The end route reads an idle session and passes `endable`, before
  and after it asks the authorizer. -/
  | endRead (w : World) (a : Asked) :
      isIdle (current w.log) = true →
      Step g w { w with ends := a :: w.ends }
  /-- The end route appends `ended`. -/
  | endAppend (w : World) (a : Asked) :
      a ∈ w.ends → g.endRoute (current w.log) = true →
      Step g w { w with log := .ended a.reason :: w.log, ends := w.ends.erase a }
  | endRefused (w : World) (a : Asked) :
      a ∈ w.ends → g.endRoute (current w.log) = false →
      Step g w { w with ends := w.ends.erase a }
  /-- The reaper lists idle sessions and appends `ended expired` after the
  sequence its list read, so its check and its append are one step. -/
  | reap (w : World) :
      isIdle (current w.log) = true →
      Step g w { w with log := .ended .expired :: w.log }

/-- The worlds some interleaving reaches from a new session. -/
inductive Reachable (g : Guards) : World → Prop where
  | init : Reachable g World.init
  | step {w w' : World} : Reachable g w → Step g w w' → Reachable g w'

/-! ## Today's checks break the diagram -/

/-- A runner reads an idle session, an end lands, and the runner's claim
appends `running` after it: the ended session runs again. -/
theorem shipped_claim_after_end :
    ∃ w, Reachable shipped w ∧ valid w.log = false ∧ w.log = [.running, .ended .canceled] := by
  have h1 := Reachable.step (Reachable.init (g := shipped)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.endRead _ .canceled rfl)
  have h4 := Reachable.step h3 (Step.endAppend _ .canceled (List.Mem.head _) rfl)
  have h5 := Reachable.step h4 (Step.claim _ 0 rfl rfl rfl)
  exact ⟨_, h5, rfl, rfl⟩

/-- The end route reads an idle session, a runner claims it, and the end
lands on the running session; the runner's turn then stops idle after the
`ended`. -/
theorem shipped_end_after_claim :
    ∃ w, Reachable shipped w ∧ valid w.log = false ∧
      w.log = [.idle (some .endTurn), .ended .canceled, .running] := by
  have h1 := Reachable.step (Reachable.init (g := shipped)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.endRead _ .canceled rfl)
  have h4 := Reachable.step h3 (Step.claim _ 0 rfl rfl rfl)
  have h5 := Reachable.step h4 (Step.endAppend _ .canceled (List.Mem.head _) rfl)
  have h6 := Reachable.step h5 (Step.finishIdle _ 0 .endTurn rfl rfl)
  exact ⟨_, h6, rfl, rfl⟩

/-- Fixing only the claim leaves the end route's race. -/
theorem claim_fix_alone_breaks :
    ∃ w, Reachable claimFixedOnly w ∧ valid w.log = false := by
  have h1 := Reachable.step (Reachable.init (g := claimFixedOnly)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.endRead _ .canceled rfl)
  have h4 := Reachable.step h3 (Step.claim _ 0 rfl rfl rfl)
  have h5 := Reachable.step h4 (Step.endAppend _ .canceled (List.Mem.head _) rfl)
  have h6 := Reachable.step h5 (Step.finishIdle _ 0 .endTurn rfl rfl)
  exact ⟨_, h6, rfl⟩

/-- Fixing only the end route leaves the claim's race. -/
theorem end_fix_alone_breaks :
    ∃ w, Reachable endFixedOnly w ∧ valid w.log = false := by
  have h1 := Reachable.step (Reachable.init (g := endFixedOnly)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.endRead _ .canceled rfl)
  have h4 := Reachable.step h3 (Step.endAppend _ .canceled (List.Mem.head _) rfl)
  have h5 := Reachable.step h4 (Step.claim _ 0 rfl rfl rfl)
  exact ⟨_, h5, rfl⟩

/-! ## The fixed checks keep it -/

/-- What holds in every world the fixed writers reach. -/
structure Inv (w : World) : Prop where
  valid : valid w.log = true
  seen_le : ∀ r, w.seen r ≤ w.log.length
  /-- A runner driving a turn under its lease drives a running session. -/
  turn : ∀ r, w.holder = some r → w.phase r = .turn → current w.log = .running
  /-- A runner about to claim read a session that had not ended. -/
  read : ∀ r, w.holder = some r → (w.phase r = .claiming ∨ w.phase r = .after) →
    isEnded (current (w.log.drop (w.log.length - w.seen r))) = false

theorem inv_init : Inv World.init where
  valid := rfl
  seen_le := fun _ => Nat.le_refl 0
  turn := fun _ h => by simp [World.init] at h
  read := fun _ h => by simp [World.init] at h

/-- What a runner read stays what it read when another writer appends. -/
theorem drop_cons_seen {m : Mark} {log : List Mark} {s : Nat} (h : s ≤ log.length) :
    (m :: log).drop ((m :: log).length - s) = log.drop (log.length - s) := by
  have : log.length + 1 - s = (log.length - s) + 1 := by omega
  simp only [List.length_cons, this, List.drop_succ_cons]

/-- A claim that met no `ended` among what others appended, over a read
that met none, claims a session that has not ended. -/
theorem not_ended_of_claim {log : List Mark} {k : Nat}
    (hg : (log.take k).any isEnded = false) (hr : isEnded (current (log.drop k)) = false) :
    isEnded (current log) = false := by
  cases k with
  | zero => simpa using hr
  | succ k =>
    cases log with
    | nil => rfl
    | cons m rest =>
      simp only [List.take_succ_cons, List.any_cons, Bool.or_eq_false_iff] at hg
      exact hg.1

theorem edge_claim {m : Mark} (h : isEnded m = false) : edge m .running = true := by
  cases m <;> simp_all [isEnded, edge]

theorem edge_end {m : Mark} (h : isIdle m = true) (a : Asked) : edge m (.ended a.reason) = true := by
  cases m <;> cases a <;> simp_all [isIdle, edge, Asked.reason]

theorem edge_expire {m : Mark} (h : isIdle m = true) : edge m (.ended .expired) = true := by
  cases m <;> simp_all [isIdle, edge]

theorem idle_not_running {m : Mark} (h : isIdle m = true) : m ≠ .running := by
  cases m <;> simp_all [isIdle]

/-- One step of a fixed writer keeps the invariant. -/
theorem inv_step {w w' : World} (h : Inv w) (s : Step fixed w w') : Inv w' := by
  cases s with
  | acquire r hh hp =>
    refine ⟨h.valid, h.seen_le, ?_, ?_⟩ <;>
      intro x hx <;> simp only [World.setPhase, Option.some.injEq] at hx ⊢ <;> subst hx <;> simp
  | check r hh hp he =>
    refine ⟨h.valid, ?_, ?_, ?_⟩
    · intro x; simp only [World.read, World.setPhase]; split
      · exact Nat.le_refl _
      · exact h.seen_le x
    · intro x hx; simp only [World.read, World.setPhase] at hx ⊢; rw [hh] at hx
      cases hx; simp
    · intro x hx _; simp only [World.read, World.setPhase] at hx ⊢; rw [hh] at hx
      cases hx; simpa using he
  | checkEnded r =>
    exact ⟨h.valid, h.seen_le, fun _ hx => by simp at hx, fun _ hx => by simp at hx⟩
  | claim r hh hp hg =>
    have hr := h.read r hh (Or.inl hp)
    have hn := not_ended_of_claim (by simpa [fixed, World.foreign] using hg) hr
    refine ⟨?_, ?_, ?_, ?_⟩
    · simp only [World.read, World.setPhase, valid_cons]; exact ⟨edge_claim hn, h.valid⟩
    · intro x; simp only [World.read, World.setPhase]; split
      · exact Nat.le_refl _
      · exact Nat.le_succ_of_le (h.seen_le x)
    · intro _ _ _; rfl
    · intro x hx hp'; simp only [World.read, World.setPhase] at hx hp' ⊢; rw [hh] at hx
      cases hx; simp at hp'
  | claimRefused r =>
    exact ⟨h.valid, h.seen_le, fun _ hx => by simp at hx, fun _ hx => by simp at hx⟩
  | finishIdle r why hh hp =>
    have hc := h.turn r hh hp
    refine ⟨?_, ?_, ?_, ?_⟩
    · simp only [World.read, World.setPhase, valid_cons, hc]; exact ⟨rfl, h.valid⟩
    · intro x; simp only [World.read, World.setPhase]; split
      · exact Nat.le_refl _
      · exact Nat.le_succ_of_le (h.seen_le x)
    · intro x hx hp'; simp only [World.read, World.setPhase] at hx hp'; rw [hh] at hx
      cases hx; simp at hp'
    · intro x hx _; simp only [World.read, World.setPhase] at hx ⊢; rw [hh] at hx
      cases hx; simp [current, isEnded]
  | finishEnded r hh hp =>
    have hc := h.turn r hh hp
    refine ⟨?_, ?_, fun _ hx => by simp at hx, fun _ hx => by simp at hx⟩
    · simp only [World.setPhase, valid_cons, hc]; exact ⟨rfl, h.valid⟩
    · intro x; exact Nat.le_succ_of_le (h.seen_le x)
  | again r hh hp =>
    refine ⟨h.valid, h.seen_le, ?_, ?_⟩
    · intro x hx hp'; simp only [World.setPhase] at hx hp'; rw [hh] at hx
      cases hx; simp at hp'
    · intro x hx _; simp only [World.setPhase] at hx ⊢; rw [hh] at hx
      cases hx; exact h.read _ hh (Or.inr hp)
  | done r =>
    exact ⟨h.valid, h.seen_le, fun _ hx => by simp at hx, fun _ hx => by simp at hx⟩
  | fenced r hh _ =>
    refine ⟨h.valid, h.seen_le, ?_, ?_⟩
    · intro x hx hp'; simp only [World.setPhase] at hx hp'
      have : x ≠ r := fun e => hh (e ▸ hx)
      simp only [this, ite_false] at hp'; exact h.turn x hx hp'
    · intro x hx hp'; simp only [World.setPhase] at hx hp'
      have : x ≠ r := fun e => hh (e ▸ hx)
      simp only [this, ite_false] at hp'; exact h.read x hx hp'
  | lapse =>
    exact ⟨h.valid, h.seen_le, fun _ hx => by simp at hx, fun _ hx => by simp at hx⟩
  | endRead a _ =>
    exact ⟨h.valid, h.seen_le, h.turn, h.read⟩
  | endAppend a _ hg =>
    have hi : isIdle (current w.log) = true := by simpa [fixed] using hg
    refine ⟨?_, ?_, ?_, ?_⟩
    · simp only [valid_cons]; exact ⟨edge_end hi a, h.valid⟩
    · intro x; exact Nat.le_succ_of_le (h.seen_le x)
    · intro x hx hp; exact absurd (h.turn x hx hp) (idle_not_running hi)
    · intro x hx hp; simp only at hx hp ⊢
      rw [drop_cons_seen (h.seen_le x)]; exact h.read x hx hp
  | endRefused a _ _ =>
    exact ⟨h.valid, h.seen_le, h.turn, h.read⟩
  | reap hi =>
    refine ⟨?_, ?_, ?_, ?_⟩
    · simp only [valid_cons]; exact ⟨edge_expire hi, h.valid⟩
    · intro x; exact Nat.le_succ_of_le (h.seen_le x)
    · intro x hx hp; exact absurd (h.turn x hx hp) (idle_not_running hi)
    · intro x hx hp; simp only at hx hp ⊢
      rw [drop_cons_seen (h.seen_le x)]; exact h.read x hx hp

theorem fixed_inv {w : World} (h : Reachable fixed w) : Inv w := by
  induction h with
  | init => exact inv_init
  | step _ s ih => exact inv_step ih s

/-- With both changes, every interleaving of runners, end requests and
reaper passes keeps the diagram. -/
theorem fixed_keeps_the_diagram {w : World} (h : Reachable fixed w) : valid w.log = true :=
  (fixed_inv h).valid

/-! ## The fixed writers still do their work

The invariant would hold trivially if the fixed checks refused every
append. These worlds show they refuse none of the ones the diagram allows. -/

/-- A turn runs, stops idle, and a person ends the session. -/
theorem fixed_turn_then_end :
    ∃ w, Reachable fixed w ∧ w.log = [.ended .canceled, .idle (some .endTurn), .running] := by
  have h1 := Reachable.step (Reachable.init (g := fixed)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.claim _ 0 rfl rfl rfl)
  have h4 := Reachable.step h3 (Step.finishIdle _ 0 .endTurn rfl rfl)
  have h5 := Reachable.step h4 (Step.done _ 0 rfl rfl)
  have h6 := Reachable.step h5 (Step.endRead _ .canceled rfl)
  have h7 := Reachable.step h6 (Step.endAppend _ .canceled (List.Mem.head _) rfl)
  exact ⟨_, h7, rfl⟩

/-- Input that arrives during a turn starts the next one under the same
lease. -/
theorem fixed_claims_again :
    ∃ w, Reachable fixed w ∧ w.log = [.running, .idle (some .endTurn), .running] := by
  have h1 := Reachable.step (Reachable.init (g := fixed)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.claim _ 0 rfl rfl rfl)
  have h4 := Reachable.step h3 (Step.finishIdle _ 0 .endTurn rfl rfl)
  have h5 := Reachable.step h4 (Step.again _ 0 rfl rfl)
  have h6 := Reachable.step h5 (Step.claim _ 0 rfl rfl rfl)
  exact ⟨_, h6, rfl⟩

/-- A runner's lease lapses mid-turn, a second runner claims the running
session and finishes the turn, and the first runner's append is fenced. -/
theorem fixed_hands_off_a_lapsed_lease :
    ∃ w, Reachable fixed w ∧ w.log = [.idle (some .endTurn), .running, .running] ∧
      w.phase 0 = .free := by
  have h1 := Reachable.step (Reachable.init (g := fixed)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.claim _ 0 rfl rfl rfl)
  have h4 := Reachable.step h3 (Step.lapse _)
  have h5 := Reachable.step h4 (Step.acquire _ 1 rfl rfl)
  have h6 := Reachable.step h5 (Step.check _ 1 rfl rfl rfl)
  have h7 := Reachable.step h6 (Step.claim _ 1 rfl rfl rfl)
  have h8 := Reachable.step h7 (Step.fenced _ 0 (by simp [World.setPhase, World.read]) (by simp [World.setPhase, World.read]))
  have h9 := Reachable.step h8 (Step.finishIdle _ 1 .endTurn rfl rfl)
  exact ⟨_, h9, rfl, rfl⟩

/-- `end_on_idle` ends the session with its turn. -/
theorem fixed_ends_on_idle :
    ∃ w, Reachable fixed w ∧ w.log = [.ended .completed, .running] := by
  have h1 := Reachable.step (Reachable.init (g := fixed)) (Step.acquire World.init 0 rfl rfl)
  have h2 := Reachable.step h1 (Step.check _ 0 rfl rfl rfl)
  have h3 := Reachable.step h2 (Step.claim _ 0 rfl rfl rfl)
  have h4 := Reachable.step h3 (Step.finishEnded _ 0 rfl rfl)
  exact ⟨_, h4, rfl⟩

/-- The reaper ends an idle session that expired. -/
theorem fixed_reaps :
    ∃ w, Reachable fixed w ∧ w.log = [.ended .expired] :=
  ⟨_, Reachable.step (Reachable.init (g := fixed)) (Step.reap World.init rfl), rfl⟩

/-- With both changes, a session that ended stays ended, with the reason
it ended for. -/
theorem fixed_stays_ended {w : World} {r : EndReason}
    (h : Reachable fixed w) (hr : .ended r ∈ w.log) : current w.log = .ended r :=
  stays_ended (fixed_keeps_the_diagram h) hr

end SessionStatus
