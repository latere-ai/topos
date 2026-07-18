// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command authoredgraph shows the authoring flow: decode a persisted, JSON-tagged
// agent graph (latere.ai/x/topos/graph), lower it to a runnable
// latere.ai/x/topos.Graph with ToRuntime, and run it. The authored form declares
// each region's coordination with one field (sequence | lead | mesh) instead of
// the runtime's autonomy+topology pair; ToRuntime encodes that mapping. A scripted
// model is plugged into Options.Brain so the run is reproducible without API keys.
//
//	go run ./examples/authoredgraph
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"

	"latere.ai/x/topos"
	"latere.ai/x/topos/graph"
	"latere.ai/x/topos/models"
)

// persisted is an authored graph exactly as a host would store it: a "plan"
// region whose lead may delegate (coordination "lead") feeding a "ship" region
// that runs a fixed chain (coordination "sequence"), wired by a data-flow edge.
const persisted = `{
  "regions": [
    {"id": "plan", "coordination": "lead",
     "entry": {"name": "lead", "role": "lead", "tools": ["read"], "scopes": ["repo"]},
     "peers": [{"name": "reviewer", "role": "review", "description": "reviews the plan"}]},
    {"id": "ship", "coordination": "sequence",
     "entry": {"name": "impl", "role": "impl"},
     "peers": [{"name": "commit", "role": "commit"}]}
  ],
  "edges": [{"from": "plan", "to": "ship"}]
}`

// echoModel finishes every turn (it never delegates) and returns its last user
// prompt prefixed with "handled:", so each region's Final is a function of its
// input and the plan->ship edge's data flow is visible in the final output.
type echoModel struct{}

func (echoModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	prompt := ""
	for _, msg := range req.Messages {
		if msg.Role == "user" {
			prompt = msg.Content
		}
	}
	return &cannedStream{events: []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "handled:" + prompt},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}}, nil
}

func main() {
	// Decode the persisted authored graph, then lower it to the runtime shape.
	// ToRuntime validates the authored fields and the structure, and maps each
	// coordination to autonomy+topology (lead -> dynamic/orchestrator-worker,
	// sequence -> pinned).
	var authored graph.Graph
	if err := json.Unmarshal([]byte(persisted), &authored); err != nil {
		log.Fatalf("decode authored graph: %v", err)
	}
	g, err := authored.ToRuntime()
	if err != nil {
		log.Fatalf("lower to runtime: %v", err)
	}

	r, err := topos.NewRunner(topos.Options{SessionID: "authored", Brain: echoModel{}})
	if err != nil {
		log.Fatalf("new runner: %v", err)
	}
	res, err := r.RunGraph(context.Background(), g, "design the feature")
	if err != nil {
		log.Fatalf("run graph: %v", err)
	}

	fmt.Println("final:", res.Final)
	fmt.Println("nodes:")
	for _, n := range res.Lineage.Nodes {
		fmt.Printf("  %s  role=%s  status=%s\n", n.ID, n.Role, n.Status)
	}
}

type cannedStream struct {
	events []models.Event
	pos    int
}

func (s *cannedStream) Recv() (models.Event, error) {
	if s.pos >= len(s.events) {
		return models.Event{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *cannedStream) Close() error { return nil }
