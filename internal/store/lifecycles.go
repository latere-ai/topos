// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"latere.ai/x/pkg/statemachine"
)

// The lifecycles the store keeps, each as a statemachine table. Every writer
// of a lifecycle holds to its table: a write that would take a record along a
// step the table lacks changes nothing and is refused with an error that
// wraps statemachine.ErrIllegalTransition. The tests drive the real writers
// through each table on Postgres, and formal/lifecycles renders the tables
// into the Lean and TLA+ models the lean and tla gates check.

// AgentState is the state of an agent.
type AgentState string

// The states of an agent, derived from agents.archived_at. Deleted is not a
// stored value: deleting removes the row, so the state is its absence.
const (
	AgentActive   AgentState = "active"
	AgentArchived AgentState = "archived"
	AgentDeleted  AgentState = "deleted"
)

// AgentTable is the lifecycle of an agent. PutVersion creates an active
// agent; Archive is the one writer of archived_at and files an active agent
// away, which keeps its id and its versions; DeleteAgent removes an agent in
// either state with its versions. An archived agent is never active again:
// applying its name again creates a new agent.
var AgentTable = statemachine.Must(statemachine.New("Agent", []AgentState{AgentActive}, map[AgentState][]AgentState{
	AgentActive:   {AgentArchived, AgentDeleted},
	AgentArchived: {AgentDeleted},
	AgentDeleted:  nil,
}))

// IdempotencyState is the state of an idempotency record.
type IdempotencyState string

// The states of a record, as idempotency_keys.done holds the first two.
// Removed is not a stored value: abandoning a record removes the row, and a
// reservation that expired is replaced by a new record that starts again.
const (
	IdempotencyReserved IdempotencyState = "reserved"
	IdempotencyDone     IdempotencyState = "done"
	IdempotencyRemoved  IdempotencyState = "removed"
)

// IdempotencyTable is the lifecycle of an idempotency record. Begin
// reserves a key; Finish stores the answer of the request that reserved it
// and moves a reserved record to done, and only that: an answer already
// stored is never overwritten, and a reservation that was replaced after it
// expired no longer exists for its holder, which is a step from removed.
// Abandon removes a record in either state, but only the reservation its
// holder took.
var IdempotencyTable = statemachine.Must(statemachine.New("Idempotency", []IdempotencyState{IdempotencyReserved}, map[IdempotencyState][]IdempotencyState{
	IdempotencyReserved: {IdempotencyDone, IdempotencyRemoved},
	IdempotencyDone:     {IdempotencyRemoved},
	IdempotencyRemoved:  nil,
}))

// FiringState is where a trigger's firing stands, in the lower-case spelling
// a table state takes. The stored outcome is a firing's Outcome, the empty
// string while a firing is claimed and not yet recorded; FiringStateOf and
// Outcome convert.
type FiringState string

// The states of a firing: claimed, and one for each stored outcome.
// OutcomeFiltered is counted and never stored, so it is no state.
const (
	FiringClaimed           FiringState = "claimed"
	FiringStarted           FiringState = "started"
	FiringContinued         FiringState = "continued"
	FiringHeld              FiringState = "held"
	FiringSkippedActive     FiringState = "skipped_active"
	FiringSkippedBusy       FiringState = "skipped_busy"
	FiringSkippedLate       FiringState = "skipped_late"
	FiringSkippedEnded      FiringState = "skipped_ended"
	FiringSkippedBudget     FiringState = "skipped_budget"
	FiringSkippedNothingNew FiringState = "skipped_nothing_new"
	FiringRefused           FiringState = "refused"
	FiringFailed            FiringState = "failed"
)

// FiringTable is the lifecycle of a trigger's firing, held in
// trigger_firings.outcome. ClaimFiring creates a claimed firing and takes a
// stored one again when Retaken says so, which moves failed, skipped_budget
// and refused firings back to claimed; RecordFiring is the one writer of the
// outcome and moves a claimed or held firing to a recorded one, from the
// outcome it names. A firing claimed again that was never recorded is the
// same state and no step.
var FiringTable = statemachine.Must(statemachine.New("Firing", []FiringState{FiringClaimed}, map[FiringState][]FiringState{
	FiringClaimed: {
		FiringStarted, FiringContinued, FiringHeld, FiringSkippedActive, FiringSkippedBusy, FiringSkippedLate,
		FiringSkippedEnded, FiringSkippedBudget, FiringSkippedNothingNew, FiringRefused, FiringFailed,
	},
	FiringHeld: {
		FiringStarted, FiringContinued, FiringSkippedActive, FiringSkippedBusy, FiringSkippedLate,
		FiringSkippedEnded, FiringSkippedBudget, FiringSkippedNothingNew, FiringRefused, FiringFailed,
	},
	FiringStarted:           nil,
	FiringContinued:         nil,
	FiringSkippedActive:     nil,
	FiringSkippedBusy:       nil,
	FiringSkippedLate:       nil,
	FiringSkippedEnded:      nil,
	FiringSkippedNothingNew: nil,
	FiringSkippedBudget:     {FiringClaimed},
	FiringRefused:           {FiringClaimed},
	FiringFailed:            {FiringClaimed},
}))

// FiringStateOf is the state of a stored outcome. An outcome no state names,
// OutcomeFiltered included, is reported false.
func FiringStateOf(outcome string) (FiringState, bool) {
	switch outcome {
	case "":
		return FiringClaimed, true
	case OutcomeStarted:
		return FiringStarted, true
	case OutcomeContinued:
		return FiringContinued, true
	case OutcomeHeld:
		return FiringHeld, true
	case OutcomeSkippedActive:
		return FiringSkippedActive, true
	case OutcomeSkippedBusy:
		return FiringSkippedBusy, true
	case OutcomeSkippedLate:
		return FiringSkippedLate, true
	case OutcomeSkippedEnded:
		return FiringSkippedEnded, true
	case OutcomeSkippedBudget:
		return FiringSkippedBudget, true
	case OutcomeSkippedNothingNew:
		return FiringSkippedNothingNew, true
	case OutcomeRefused:
		return FiringRefused, true
	case OutcomeFailed:
		return FiringFailed, true
	default:
		return FiringClaimed, false
	}
}

// Outcome is the stored outcome of a state: the empty string for a claimed
// firing.
func (s FiringState) Outcome() string {
	switch s {
	case FiringClaimed:
		return ""
	case FiringStarted:
		return OutcomeStarted
	case FiringContinued:
		return OutcomeContinued
	case FiringHeld:
		return OutcomeHeld
	case FiringSkippedActive:
		return OutcomeSkippedActive
	case FiringSkippedBusy:
		return OutcomeSkippedBusy
	case FiringSkippedLate:
		return OutcomeSkippedLate
	case FiringSkippedEnded:
		return OutcomeSkippedEnded
	case FiringSkippedBudget:
		return OutcomeSkippedBudget
	case FiringSkippedNothingNew:
		return OutcomeSkippedNothingNew
	case FiringRefused:
		return OutcomeRefused
	case FiringFailed:
		return OutcomeFailed
	default:
		return ""
	}
}

// RunState is whether a trigger run's session has ended.
type RunState string

// The states of a run, as trigger_runs.ended holds them.
const (
	RunOpen  RunState = "open"
	RunEnded RunState = "ended"
)

// RunTable is the lifecycle of a trigger run's ended flag. Recording a run
// starts it open, or ended when its firing started no session; SetRunCosts is
// the one writer of the flag, and a run whose cost was settled at the end of
// its session takes no further write.
var RunTable = statemachine.Must(statemachine.New("Run", []RunState{RunOpen, RunEnded}, map[RunState][]RunState{
	RunOpen:  {RunEnded},
	RunEnded: nil,
}))
