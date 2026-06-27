package sdk

import (
	"context"
	"reflect"
	"testing"

	"latere.ai/x/agents/internal/harness/hooks"
)

// scripted is a deterministic brain: it delegates to "reviewer" whenever a
// directory is present, and otherwise finishes. This is the fake ModelProvider
// the spike relies on for reproducibility.
type scripted struct{ sawPeers []PeerCard }

func (s *scripted) Decide(_ context.Context, t Turn) (Action, error) {
	if len(t.Peers) > 0 {
		s.sawPeers = t.Peers
		return Action{Kind: ActionDelegate, Delegate: &DelegateAction{Peer: "reviewer", Task: "review the diff"}}, nil
	}
	return Action{Kind: ActionFinal, FinalText: "done: " + t.Agent}, nil
}

func dynamicRegion() Region {
	return Region{
		Autonomy: Dynamic,
		Entry:    AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write", "exec"}, Scopes: []string{"repo"}},
		Peers: []AgentSpec{{
			Name: "reviewer", Role: "review", Description: "reviews diffs",
			Tools: []string{"write", "exec", "deploy"}, Scopes: []string{"repo", "prod"},
		}},
	}
}

func TestDynamicDelegateBuildsLineage(t *testing.T) {
	r := NewRunner(Options{SessionID: "run-1", Provider: &scripted{}, BudgetUSD: 5})

	// Spy on the hook bus to prove real Subagent events fire (white-box).
	var events []hooks.EventName
	r.bus.Register("spy", nil, func(n hooks.EventName, _ any) hooks.Decision {
		events = append(events, n)
		return hooks.Allow()
	})

	res, err := r.Run(context.Background(), dynamicRegion())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Final != "done: reviewer" {
		t.Errorf("final = %q, want %q", res.Final, "done: reviewer")
	}

	// Two nodes with deterministic ids.
	wantIDs := []string{"run-1/lead", "run-1/sub/reviewer"}
	gotIDs := []string{res.Lineage.Nodes[0].ID, res.Lineage.Nodes[1].ID}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Errorf("node ids = %v, want %v", gotIDs, wantIDs)
	}
	for _, n := range res.Lineage.Nodes {
		if n.Status != "done" {
			t.Errorf("node %s status = %q, want done", n.ID, n.Status)
		}
	}

	// Delegate then deliver edges.
	wantEdges := []LineageEdge{
		{From: "run-1/lead", To: "run-1/sub/reviewer", Kind: "delegate"},
		{From: "run-1/sub/reviewer", To: "run-1/lead", Kind: "deliver"},
	}
	if !reflect.DeepEqual(res.Lineage.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", res.Lineage.Edges, wantEdges)
	}

	// Real hook-bus events under the session.
	if !containsEvent(events, hooks.EventSubagentStart) || !containsEvent(events, hooks.EventSubagentStop) {
		t.Errorf("missing Subagent events, got %v", events)
	}
}

func TestDynamicInjectsDirectory(t *testing.T) {
	p := &scripted{}
	r := NewRunner(Options{SessionID: "run-1", Provider: p, BudgetUSD: 5})
	if _, err := r.Run(context.Background(), dynamicRegion()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.sawPeers) != 1 || p.sawPeers[0].Name != "reviewer" || p.sawPeers[0].Description != "reviews diffs" {
		t.Errorf("entry did not see the directory: %+v", p.sawPeers)
	}
}

func TestDelegateAttenuatesPeerTools(t *testing.T) {
	r := NewRunner(Options{SessionID: "run-1", Provider: &scripted{}, BudgetUSD: 5})
	res, err := r.Run(context.Background(), dynamicRegion())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// reviewer requested {write,exec,deploy}; entry holds {read,write,exec};
	// the granted set is the intersection — "deploy" is dropped, "read" never offered.
	grants := res.Lineage.Nodes[1].Grants
	if has(grants, "deploy") || has(grants, "read") || !has(grants, "write") || !has(grants, "exec") || len(grants) != 2 {
		t.Errorf("attenuated grants = %v, want exactly [write exec]", grants)
	}
}

func TestDynamicRunIsReproducible(t *testing.T) {
	run := func() Lineage {
		r := NewRunner(Options{SessionID: "run-1", Provider: &scripted{}, BudgetUSD: 5})
		res, err := r.Run(context.Background(), dynamicRegion())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return res.Lineage
	}
	if a, b := run(), run(); !reflect.DeepEqual(a, b) {
		t.Errorf("lineage not reproducible:\n a=%+v\n b=%+v", a, b)
	}
}

func TestPinnedRegionRunsDeterministicChain(t *testing.T) {
	r := NewRunner(Options{SessionID: "run-1", Provider: &scripted{}, BudgetUSD: 5})
	res, err := r.Run(context.Background(), Region{
		Autonomy: Pinned,
		Entry:    AgentSpec{Name: "impl", Role: "impl"},
		Peers:    []AgentSpec{{Name: "test", Role: "test"}, {Name: "commit", Role: "commit"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantIDs := []string{"run-1/impl", "run-1/test", "run-1/commit"}
	for i, n := range res.Lineage.Nodes {
		if n.ID != wantIDs[i] || n.Status != "done" {
			t.Errorf("node %d = %+v, want id %s done", i, n, wantIDs[i])
		}
	}
	wantEdges := []LineageEdge{
		{From: "run-1/impl", To: "run-1/test", Kind: "next"},
		{From: "run-1/test", To: "run-1/commit", Kind: "next"},
	}
	if !reflect.DeepEqual(res.Lineage.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", res.Lineage.Edges, wantEdges)
	}
}

func containsEvent(in []hooks.EventName, want hooks.EventName) bool {
	for _, e := range in {
		if e == want {
			return true
		}
	}
	return false
}

func has(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}
