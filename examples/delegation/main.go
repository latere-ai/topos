// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command delegation shows a dynamic region where the entry agent hands a
// subtask to a peer through the delegate tool, and the deterministic lineage
// graph that results. It plugs a scripted model into Options.Brain so the run
// is reproducible without API keys or services, which is also how a host wires
// its own models.Model in place of the built-in Lux/Direct/Fake kinds.
//
//	go run ./examples/delegation
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"latere.ai/x/topos"
	"latere.ai/x/topos/models"
)

// scriptedModel is a minimal deterministic models.Model. The entry agent (which
// holds the delegate tool) hands off to a peer once; the peer, and the entry
// after the peer returns, simply finish. Any models.Model (Lux, a provider
// adapter) plugs in the same way via Options.Brain.
type scriptedModel struct{ peer string }

func (m scriptedModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	// A prior tool result in the transcript means the delegate already
	// returned: finish the turn.
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			return canned(text("done", models.StopEndTurn)), nil
		}
	}
	// Holding the delegate tool (the entry agent): delegate to the peer.
	for _, td := range req.Tools {
		if td.Name == "delegate" {
			input := fmt.Appendf(nil, `{"peer":%q,"task":"review the change"}`, m.peer)
			return canned([]models.Event{
				{Kind: models.KindTextDelta, TextDelta: "delegating to " + m.peer},
				{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "call_1", Name: "delegate", Input: input}},
				{Kind: models.KindDone, StopReason: models.StopToolUse},
			}), nil
		}
	}
	// A peer (no delegate tool): finish.
	return canned(text("looks good", models.StopEndTurn)), nil
}

func text(s string, stop models.StopReason) []models.Event {
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: s},
		{Kind: models.KindDone, StopReason: stop},
	}
}

func main() {
	// Options.Brain plugs the scripted model straight in. A host would instead
	// pass real ModelOptions (ModelLux / ModelDirect) and leave Brain nil.
	r, err := topos.NewRunner(topos.Options{
		SessionID: "deleg",
		Brain:     scriptedModel{peer: "reviewer"},
	})
	if err != nil {
		log.Fatalf("new runner: %v", err)
	}

	// Dynamic + OrchestratorWorker (the default topology): the entry agent gets
	// a directory of peers and a delegate tool, and hands work off; only the
	// entry delegates, so this is a single hop. Grants attenuate: the peer can
	// hold no more than the entry's tools and scopes. (Switch to topos.Mesh to
	// let peers delegate too, bounded by Options.MaxHandoffDepth.)
	region := topos.Region{
		Autonomy: topos.Dynamic,
		Topology: topos.OrchestratorWorker,
		Entry:    topos.AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write"}, Scopes: []string{"repo"}},
		Peers: []topos.AgentSpec{{
			Name: "reviewer", Role: "review", Description: "reviews diffs",
			Tools: []string{"read"}, Scopes: []string{"repo"},
		}},
	}

	res, err := r.Run(context.Background(), region, "ship the change")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println("final:", res.Final)
	fmt.Println("nodes:")
	for _, n := range res.Lineage.Nodes {
		fmt.Printf("  %s  role=%s  grants=%v\n", n.ID, n.Role, n.Grants)
	}
	fmt.Println("edges:")
	for _, e := range res.Lineage.Edges {
		fmt.Printf("  %s -> %s (%s)\n", e.From, e.To, e.Kind)
	}
}

// canned adapts a fixed slice of events to a models.Stream.
func canned(events []models.Event) models.Stream { return &cannedStream{events: events} }

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
