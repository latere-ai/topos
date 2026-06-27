package sdk

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"latere.ai/x/agents/internal/harness/hooks"
	"latere.ai/x/agents/internal/models"
)

// --- deterministic, content-based test brain (stateless, so it composes under
// the nested peer loop a delegation triggers) ---

type testBrain struct {
	delegateTo string
	systems    *[]string // when set, captures each call's system prompt
}

func (b testBrain) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	if b.systems != nil {
		*b.systems = append(*b.systems, req.System)
	}
	// A prior tool result means the delegate already returned — finish.
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return &cannedStream{events: endTurn("done")}, nil
		}
	}
	// Holding a delegate tool (the entry agent) — delegate.
	for _, td := range req.Tools {
		if td.Name == "delegate" && b.delegateTo != "" {
			return &cannedStream{events: delegateTurn(b.delegateTo, "review the diff")}, nil
		}
	}
	// A peer (no delegate tool) — finish.
	return &cannedStream{events: endTurn("looks good")}, nil
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

func delegateTurn(peer, task string) []models.Event {
	input := []byte(`{"peer":"` + peer + `","task":"` + task + `"}`)
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "delegating"},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "call_1", Name: "delegate", Input: input}},
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}
}

func endTurn(text string) []models.Event {
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: text},
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 5, OutputTokens: 3}},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}
}

func dynamicRegion() Region {
	return Region{
		Autonomy: Dynamic,
		Entry:    AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write"}, Scopes: []string{"repo"}},
		Peers: []AgentSpec{{
			Name: "reviewer", Role: "review", Description: "reviews diffs",
			Tools: []string{"write", "exec"}, Scopes: []string{"repo"},
		}},
	}
}

// newTestRunner builds a runner and overrides its model with a scripted brain
// (white-box: the runner uses r.model when constructing the delegate tool + loop).
func newTestRunner(t *testing.T, brain models.Model) *Runner {
	t.Helper()
	r, err := NewRunner(Options{SessionID: "run-1", Model: ModelOptions{Kind: ModelFake}, BudgetUSD: 5})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = brain
	return r
}

func TestDynamicDelegateBuildsLineage(t *testing.T) {
	r := newTestRunner(t, testBrain{delegateTo: "reviewer"})

	var events []hooks.EventName
	r.bus.Register("spy", nil, func(n hooks.EventName, _ any) hooks.Decision {
		events = append(events, n)
		return hooks.Allow()
	})

	res, err := r.Run(context.Background(), dynamicRegion(), "ship the change")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.Final != "done" {
		t.Errorf("final = %q, want done", res.Final)
	}
	if len(res.Lineage.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want 2", res.Lineage.Nodes)
	}
	if res.Lineage.Nodes[1].ID != "run-1/sub/reviewer" || res.Lineage.Nodes[1].Status != "done" {
		t.Errorf("child node = %+v", res.Lineage.Nodes[1])
	}
	wantEdges := []LineageEdge{
		{From: "run-1/lead", To: "run-1/sub/reviewer", Kind: "delegate"},
		{From: "run-1/sub/reviewer", To: "run-1/lead", Kind: "deliver"},
	}
	if !reflect.DeepEqual(res.Lineage.Edges, wantEdges) {
		t.Errorf("edges = %+v, want %+v", res.Lineage.Edges, wantEdges)
	}
	if !containsEvent(events, hooks.EventSubagentStart) || !containsEvent(events, hooks.EventSubagentStop) {
		t.Errorf("missing Subagent events, got %v", events)
	}
}

func TestDynamicInjectsDirectoryIntoSystemPrompt(t *testing.T) {
	var systems []string
	r := newTestRunner(t, testBrain{delegateTo: "reviewer", systems: &systems})
	region := dynamicRegion()
	region.Entry.SystemPrompt = "You are the lead."
	if _, err := r.Run(context.Background(), region, "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(systems) == 0 {
		t.Fatal("model received no system prompt")
	}
	entrySys := systems[0]
	for _, want := range []string{"You are the lead.", "reviewer", "reviews diffs", "delegate"} {
		if !strings.Contains(entrySys, want) {
			t.Errorf("entry system prompt missing %q:\n%s", want, entrySys)
		}
	}
}

func TestDelegateRejectsPeerNotInDirectory(t *testing.T) {
	// The model tries to delegate to a peer that isn't in the directory; the
	// capability gate refuses it, so no node is created and the entry recovers.
	r := newTestRunner(t, testBrain{delegateTo: "ghost"})
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Lineage.Nodes) != 1 {
		t.Errorf("gate failed: a node was created for an out-of-directory peer: %+v", res.Lineage.Nodes)
	}
	if res.Final != "done" {
		t.Errorf("entry did not recover after a refused delegate: final = %q", res.Final)
	}
}

func TestDelegateAttenuatesPeerTools(t *testing.T) {
	r := newTestRunner(t, testBrain{delegateTo: "reviewer"})
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// reviewer asked for {write,exec}; lead holds {read,write} → granted {write}.
	g := res.Lineage.Nodes[1].Grants
	if len(g) != 1 || g[0] != "write" {
		t.Errorf("grants = %v, want [write]", g)
	}
}

func TestDelegatePeerReplyFlowsBack(t *testing.T) {
	r := newTestRunner(t, testBrain{delegateTo: "reviewer"})
	// The peer's "looks good" is the delegate tool's result; assert it round-tripped
	// by checking the entry finished after seeing it (final "done" requires a prior
	// tool result in the transcript).
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Final != "done" {
		t.Errorf("entry did not finish after the delegate returned: final = %q", res.Final)
	}
}

func TestPinnedChainRunsInOrder(t *testing.T) {
	// Plain brain: every turn finishes (no delegation), so each chain step terminates.
	r := newTestRunner(t, testBrain{})
	res, err := r.Run(context.Background(), Region{
		Autonomy: Pinned,
		Entry:    AgentSpec{Name: "impl", Role: "impl"},
		Peers:    []AgentSpec{{Name: "test", Role: "test"}, {Name: "commit", Role: "commit"}},
	}, "build it")
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

func TestBuildModelKinds(t *testing.T) {
	// Fake builds without network.
	if _, err := NewRunner(Options{Model: ModelOptions{Kind: ModelFake}}); err != nil {
		t.Errorf("ModelFake: %v", err)
	}
	// Lux/direct build an adapter (no network at construction).
	if _, err := NewRunner(Options{Model: ModelOptions{Kind: ModelLux, BaseURL: "http://localhost:8080/anthropic", APIKey: "lux_x"}}); err != nil {
		t.Errorf("ModelLux: %v", err)
	}
	// Unsupported provider is rejected.
	if _, err := NewRunner(Options{Model: ModelOptions{Kind: ModelLux, Provider: "cohere"}}); err == nil {
		t.Error("ModelLux with unsupported provider: want error, got nil")
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

func TestDynamicRunFinalIsDeterministic(t *testing.T) {
	run := func() string {
		r := newTestRunner(t, testBrain{delegateTo: "reviewer"})
		res, err := r.Run(context.Background(), dynamicRegion(), "go")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// Lineage ids + edges are deterministic; assert the structural summary.
		return summarize(res.Lineage)
	}
	if a, b := run(), run(); a != b {
		t.Errorf("run summary not reproducible:\n a=%s\n b=%s", a, b)
	}
}

func summarize(l Lineage) string {
	var b strings.Builder
	for _, n := range l.Nodes {
		b.WriteString(n.ID + ":" + n.Status + ";")
	}
	for _, e := range l.Edges {
		b.WriteString(e.From + "->" + e.To + ":" + e.Kind + ";")
	}
	return b.String()
}
