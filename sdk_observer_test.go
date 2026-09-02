// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

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

// runWithObserver builds a runner whose model is the scripted model and whose
// Options carry the given observer (registered on the bus by NewRunner), then
// runs the dynamic region (which delegates entry -> reviewer).
func runWithObserver(t *testing.T, mdl models.Model, obs func(Event)) RunResult {
	t.Helper()
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake, Model: "claude-opus-4-8"},
		BudgetUSD: 5,
		Observer:  obs,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = mdl
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// TestObserver_ReceivesEventStream proves an embedder observer sees the run's
// event stream: lifecycle bookends, the per-turn assistant text, and the
// delegation (subagent) events, with SessionID joining to a trace node.
func TestObserver_ReceivesEventStream(t *testing.T) {
	var mu sync.Mutex
	var names []string
	var assistantText, assistantSession string

	res := runWithObserver(t, testModel{delegateTo: "reviewer"}, func(e Event) {
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
	// AssistantMessage text is non-empty and its SessionID joins to a trace node.
	if assistantText == "" {
		t.Error("AssistantMessage carried no text")
	}
	joined := false
	for _, n := range res.Trace.Nodes {
		if n.ID == assistantSession {
			joined = true
			break
		}
	}
	if !joined {
		t.Errorf("AssistantMessage SessionID %q does not match any trace node id %v", assistantSession, nodeIDs(res.Trace))
	}
}

// TestObserver_DoesNotAlterRun proves observation is side-effect-free: the final
// text and trace are identical with and without an observer.
func TestObserver_DoesNotAlterRun(t *testing.T) {
	base := runWithObserver(t, testModel{delegateTo: "reviewer"}, nil)
	withObs := runWithObserver(t, testModel{delegateTo: "reviewer"}, func(Event) {})

	if base.Final != withObs.Final {
		t.Errorf("final text differs: %q vs %q", base.Final, withObs.Final)
	}
	if !slices.Equal(nodeIDs(base.Trace), nodeIDs(withObs.Trace)) {
		t.Errorf("trace nodes differ: %v vs %v", nodeIDs(base.Trace), nodeIDs(withObs.Trace))
	}
}

// TestObserver_PanicIsRecovered proves a buggy observer cannot crash the run.
func TestObserver_PanicIsRecovered(t *testing.T) {
	res := runWithObserver(t, testModel{delegateTo: "reviewer"}, func(Event) {
		panic("observer blew up")
	})
	if res.Final == "" {
		t.Error("run produced no final text despite recovering the observer panic")
	}
}

func nodeIDs(l Trace) []string {
	ids := make([]string, 0, len(l.Nodes))
	for _, n := range l.Nodes {
		ids = append(ids, n.ID)
	}
	return ids
}
