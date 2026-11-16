// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"flag"
	"slices"
	"testing"

	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/topos/session"
)

var update = flag.Bool("update", false, "rewrite the formal models under formal/lifecycles")

// TestFormalModels holds formal/lifecycles, which the lean and tla gates
// check, to the lifecycle tables. Rerun with -update after changing one.
func TestFormalModels(t *testing.T) {
	statemachinetest.Models(t, "../../formal/lifecycles", *update,
		session.StatusTable.Graph(), session.ArchiveTable.Graph(), session.MessageTable.Graph(),
		AgentTable.Graph(), IdempotencyTable.Graph(), FiringTable.Graph(), RunTable.Graph())
}

// TestEveryStatusIsInItsTable keeps each status type and its table to one
// set of states, so a state added to one is added to the other.
func TestEveryStatusIsInItsTable(t *testing.T) {
	check(t, "Session", session.StatusTable.States(), session.StatusIdle, session.StatusRunning, session.StatusEnded)
	check(t, "Archive", session.ArchiveTable.States(), session.ArchiveActive, session.ArchiveArchived)
	check(t, "Message", session.MessageTable.States(), session.MessageWaiting, session.MessageRead, session.MessageWithdrawn)
	check(t, "Agent", AgentTable.States(), AgentActive, AgentArchived, AgentDeleted)
	check(t, "Idempotency", IdempotencyTable.States(), IdempotencyReserved, IdempotencyDone, IdempotencyRemoved)
	check(t, "Firing", FiringTable.States(), FiringClaimed, FiringStarted, FiringContinued, FiringHeld, FiringSkippedActive,
		FiringSkippedBusy, FiringSkippedLate, FiringSkippedEnded, FiringSkippedBudget, FiringSkippedNothingNew, FiringRefused, FiringFailed)
	check(t, "Run", RunTable.States(), RunOpen, RunEnded)
}

func check[S ~string](t *testing.T, name string, states []S, consts ...S) {
	t.Helper()
	want := slices.Sorted(slices.Values(consts))
	if !slices.Equal(states, want) {
		t.Errorf("%s table states %v, constants %v", name, states, want)
	}
}

// TestFiringStatesAreTheStoredOutcomes: every state converts to the stored
// outcome it names and back, and OutcomeFiltered, which is counted and
// never stored, is no state.
func TestFiringStatesAreTheStoredOutcomes(t *testing.T) {
	for _, s := range FiringTable.States() {
		got, ok := FiringStateOf(s.Outcome())
		if !ok || got != s {
			t.Errorf("state %q: outcome %q reads back as %q, %v", s, s.Outcome(), got, ok)
		}
	}
	if _, ok := FiringStateOf(OutcomeFiltered); ok {
		t.Error("OutcomeFiltered is a state")
	}
	if _, ok := FiringStateOf("bogus"); ok {
		t.Error(`"bogus" is a state`)
	}
	if err := CheckOutcomeStep("", "bogus"); err == nil {
		t.Error("a step to an outcome no state names was allowed")
	}
	if err := CheckOutcomeStep("", OutcomeStarted); err != nil {
		t.Errorf("claimed to started: %v", err)
	}
	if err := CheckOutcomeStep(OutcomeStarted, OutcomeRefused); err == nil {
		t.Error("started to refused was allowed")
	}
}
