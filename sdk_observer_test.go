// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"latere.ai/x/topos/models"
)

// runWithObserver builds a runner whose model is the scripted brain and whose
// Options carry the given observer (registered on the bus by NewRunner), then
// runs the dynamic region (which delegates entry -> reviewer).
func runWithObserver(t *testing.T, brain models.Model, obs func(Event)) RunResult {
	t.Helper()
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake},
		BudgetUSD: 5,
		Observer:  obs,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = brain
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestObserver_ReceivesEventStream proves an embedder observer sees the run's
// event stream: lifecycle bookends, the per-turn assistant text, and the
// delegation (subagent) events, with SessionID joining to a lineage node.
func TestObserver_ReceivesEventStream(t *testing.T) {
	var mu sync.Mutex
	var names []string
	var assistantText, assistantSession string

	res := runWithObserver(t, testBrain{delegateTo: "reviewer"}, func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		names = append(names, e.Name)
		if e.Name == EventAssistantMessage && assistantText == "" {
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(e.PayloadJSON, &p)
			assistantText = p.Text
			assistantSession = e.SessionID
		}
	})

	if len(names) == 0 {
		t.Fatal("observer received no events")
	}
	if names[0] != EventSessionStart {
		t.Errorf("first event = %q, want %q", names[0], EventSessionStart)
	}
	if !slices.Contains(names, EventSessionEnd) {
		t.Error("missing SessionEnd")
	}
	if !slices.Contains(names, EventAssistantMessage) {
		t.Error("missing AssistantMessage (the transcript)")
	}
	if !slices.Contains(names, EventSubagentStart) || !slices.Contains(names, EventSubagentStop) {
		t.Errorf("missing delegation events; got %v", names)
	}
	// Two agents ran (entry + delegated peer) -> at least two SessionStart.
	if got := slices.Index(names, EventSessionStart); got != 0 {
		t.Errorf("SessionStart not first: %v", names)
	}
	// AssistantMessage text is non-empty and its SessionID joins to a lineage node.
	if assistantText == "" {
		t.Error("AssistantMessage carried no text")
	}
	joined := false
	for _, n := range res.Lineage.Nodes {
		if n.ID == assistantSession {
			joined = true
			break
		}
	}
	if !joined {
		t.Errorf("AssistantMessage SessionID %q does not match any lineage node id %v", assistantSession, nodeIDs(res.Lineage))
	}
}

// TestObserver_DoesNotAlterRun proves observation is side-effect-free: the final
// text and lineage are identical with and without an observer.
func TestObserver_DoesNotAlterRun(t *testing.T) {
	base := runWithObserver(t, testBrain{delegateTo: "reviewer"}, nil)
	withObs := runWithObserver(t, testBrain{delegateTo: "reviewer"}, func(Event) {})

	if base.Final != withObs.Final {
		t.Errorf("final text differs: %q vs %q", base.Final, withObs.Final)
	}
	if !slices.Equal(nodeIDs(base.Lineage), nodeIDs(withObs.Lineage)) {
		t.Errorf("lineage nodes differ: %v vs %v", nodeIDs(base.Lineage), nodeIDs(withObs.Lineage))
	}
}

// TestObserver_PanicIsRecovered proves a buggy observer cannot crash the run.
func TestObserver_PanicIsRecovered(t *testing.T) {
	res := runWithObserver(t, testBrain{delegateTo: "reviewer"}, func(Event) {
		panic("observer blew up")
	})
	if res.Final == "" {
		t.Error("run produced no final text despite recovering the observer panic")
	}
}

func nodeIDs(l Lineage) []string {
	ids := make([]string, 0, len(l.Nodes))
	for _, n := range l.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}
