// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos_test

import (
	"context"
	"fmt"
	"io"

	"latere.ai/x/topos"
	"latere.ai/x/topos/models"
)

// Example shows the minimal Topos Runtime loop: build a Runner, hand it a
// Region with one agent, and run a task. It uses the deterministic ModelFake
// and the default local sandbox, so it needs no API keys and no services.
func Example() {
	r, err := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})
	if err != nil {
		panic(err)
	}

	res, err := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")
	if err != nil {
		panic(err)
	}

	fmt.Println(res.Final)
	// Output: Task completed: echoed your prompt in the sandbox.
}

// ExampleRunner_Run_lineage shows that every run yields a deterministic lineage
// graph: one node per agent that ran, each with a stable id, status, granted
// tools, and the sandbox it ran in.
func ExampleRunner_Run_lineage() {
	r, _ := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})

	res, _ := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")

	for _, n := range res.Lineage.Nodes {
		fmt.Printf("%s %s\n", n.ID, n.Status)
	}
	// Output: demo/solo done
}

// ExampleRunner_Run_delegation shows a dynamic region where the entry agent
// delegates to a peer. A scripted model is supplied via Options.Brain so the
// run is deterministic; a host would pass real ModelOptions instead. The
// resulting lineage records the delegate and deliver edges.
func ExampleRunner_Run_delegation() {
	r, _ := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Brain:     delegateOnceBrain{peer: "reviewer"},
	})

	res, _ := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Dynamic,
		Topology: topos.OrchestratorWorker,
		Entry:    topos.AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write"}, Scopes: []string{"repo"}},
		Peers: []topos.AgentSpec{{
			Name: "reviewer", Role: "review", Description: "reviews diffs",
			Tools: []string{"read"}, Scopes: []string{"repo"},
		}},
	}, "ship the change")

	fmt.Println(res.Final)
	for _, e := range res.Lineage.Edges {
		fmt.Printf("%s -> %s (%s)\n", e.From, e.To, e.Kind)
	}
	// Output:
	// done
	// demo/lead -> demo/sub/reviewer (delegate)
	// demo/sub/reviewer -> demo/lead (deliver)
}

// ExampleRunner_RunGraph composes two regions into one run: a dynamic planning
// region feeds a pinned shipping chain through a data-flow edge. Region ids
// namespace the node ids, and a region-to-region next edge records the flow. The
// deterministic fake model keeps the run reproducible with no services.
func ExampleRunner_RunGraph() {
	r, _ := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})

	g := topos.Graph{
		Regions: []topos.GraphRegion{
			{ID: "plan", Region: topos.Region{
				Autonomy: topos.Dynamic,
				Entry:    topos.AgentSpec{Name: "lead", Role: "lead"},
			}},
			{ID: "ship", Region: topos.Region{
				Autonomy: topos.Pinned,
				Entry:    topos.AgentSpec{Name: "impl", Role: "impl"},
			}},
		},
		Edges: []topos.GraphEdge{{From: "plan", To: "ship"}}, // plan's Final seeds ship's task
	}

	res, _ := r.RunGraph(context.Background(), g, "design the feature")

	for _, n := range res.Lineage.Nodes {
		fmt.Printf("%s %s\n", n.ID, n.Status)
	}
	for _, e := range res.Lineage.Edges {
		fmt.Printf("%s -> %s (%s)\n", e.From, e.To, e.Kind)
	}
	// Output:
	// demo/plan/lead done
	// demo/ship/impl done
	// demo/plan/lead -> demo/ship/impl (next)
}

// delegateOnceBrain is a deterministic models.Model: the entry agent delegates
// to the peer once, then everyone finishes.
type delegateOnceBrain struct{ peer string }

func (b delegateOnceBrain) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	for _, m := range req.Messages {
		if m.Role == "tool" { // the delegate returned; finish
			return &events{ev: end("done")}, nil
		}
	}
	for _, td := range req.Tools {
		if td.Name == "delegate" { // entry holds the delegate tool: hand off
			in := fmt.Appendf(nil, `{"peer":%q,"task":"review"}`, b.peer)
			return &events{ev: []models.Event{
				{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "c1", Name: "delegate", Input: in}},
				{Kind: models.KindDone, StopReason: models.StopToolUse},
			}}, nil
		}
	}
	return &events{ev: end("reviewed")}, nil // the peer finishes
}

func end(s string) []models.Event {
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: s},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}
}

type events struct {
	ev  []models.Event
	pos int
}

func (s *events) Recv() (models.Event, error) {
	if s.pos >= len(s.ev) {
		return models.Event{}, io.EOF
	}
	e := s.ev[s.pos]
	s.pos++
	return e, nil
}

func (s *events) Close() error { return nil }
