// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command graph shows a multi-region run: one Graph that composes a dynamic
// region (an agent that may delegate) with a pinned region (a deterministic
// chain), wired by a data-flow edge so the first region's output seeds the
// second region's task. It plugs a scripted model into Options.Brain so the run
// is reproducible without API keys or services; a host wires its own
// models.Model the same way.
//
//	go run ./examples/graph
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"latere.ai/x/topos"
	"latere.ai/x/topos/models"
)

// echoModel finishes every turn (it never delegates) and returns its last user
// prompt prefixed with "handled:". That makes each region's Final a function of
// its input, so the graph edge's data flow — the plan region's Final becoming the
// ship region's task — is visible in the final output.
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
	r, err := topos.NewRunner(topos.Options{SessionID: "graph", Brain: echoModel{}})
	if err != nil {
		log.Fatalf("new runner: %v", err)
	}

	// Two regions, one graph. "plan" is dynamic (its entry could delegate over a
	// directory); "ship" is a pinned two-step chain. The edge plan->ship threads
	// plan's Final into ship's task. Each region runs in its own isolated sandbox;
	// composition across regions is text-only.
	g := topos.Graph{
		Regions: []topos.GraphRegion{
			{ID: "plan", Region: topos.Region{
				Autonomy: topos.Dynamic,
				Entry:    topos.AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read"}, Scopes: []string{"repo"}},
			}},
			{ID: "ship", Region: topos.Region{
				Autonomy: topos.Pinned,
				Entry:    topos.AgentSpec{Name: "impl", Role: "impl"},
				Peers:    []topos.AgentSpec{{Name: "commit", Role: "commit"}},
			}},
		},
		Edges: []topos.GraphEdge{{From: "plan", To: "ship"}},
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
	fmt.Println("edges:")
	for _, e := range res.Lineage.Edges {
		fmt.Printf("  %s -> %s (%s)\n", e.From, e.To, e.Kind)
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
