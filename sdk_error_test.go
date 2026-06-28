// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// failBrain rejects every request, so loop.Run returns an error. It drives the
// failure paths (entry/step/peer marked StatusFailed and the error bubbling up).
type failBrain struct{}

func (failBrain) Stream(context.Context, models.Request) (models.Stream, error) {
	return nil, errors.New("brain down")
}

// failCreateProvider is a sandbox provider whose Create always fails; every other
// Provider method is inherited from the embedded (nil) interface and is never
// reached on the create-failure path.
type failCreateProvider struct{ sandbox.Provider }

func (failCreateProvider) Create(context.Context, sandbox.CreateOptions) (sandbox.Sandbox, error) {
	return sandbox.Sandbox{}, errors.New("no capacity")
}

// readyAfterProvider wraps the local provider but reports "not running" for the
// first notReady HealthCheck calls, modelling a backend (like Cella) whose
// Create returns a sandbox still in the "creating" state.
type readyAfterProvider struct {
	*local.Provider
	mu       sync.Mutex
	notReady int
}

func (p *readyAfterProvider) HealthCheck(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.notReady > 0 {
		p.notReady--
		return errors.New("creating")
	}
	return p.Provider.HealthCheck(ctx, id)
}

func TestRunWaitsForSandboxToBecomeReady(t *testing.T) {
	withFastReadyPolling(t)
	p := &readyAfterProvider{Provider: local.New(), notReady: 3}
	r, err := NewRunner(Options{SessionID: "run-1", Model: ModelOptions{Kind: ModelFake}, Sandbox: p})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = testBrain{}
	// The run proceeds only after the sandbox reports running; a successful run
	// proves Run polled rather than using the still-"creating" box immediately.
	if _, err := r.Run(context.Background(), Region{Autonomy: Pinned, Entry: AgentSpec{Name: "solo", Role: "solo"}}, "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestRunFailsWhenSandboxNeverReady(t *testing.T) {
	withFastReadyPolling(t)
	p := &readyAfterProvider{Provider: local.New(), notReady: 1 << 30} // never ready
	r, err := NewRunner(Options{SessionID: "run-1", Model: ModelOptions{Kind: ModelFake}, Sandbox: p})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = testBrain{}
	_, err = r.Run(context.Background(), dynamicRegion(), "go")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v, want a sandbox-not-ready timeout", err)
	}
}

// withFastReadyPolling shrinks the readiness bounds for the duration of a test
// so the poll loop runs in milliseconds, and restores them afterward.
func withFastReadyPolling(t *testing.T) {
	t.Helper()
	origT, origI := readyTimeout, readyInterval
	readyTimeout, readyInterval = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { readyTimeout, readyInterval = origT, origI })
}

func TestOptionsBrainOverridesModel(t *testing.T) {
	// A custom models.Model passed via Options.Brain is used instead of the
	// built-in Model kind. ModelFake would not delegate (one node); the scripted
	// delegating brain produces a two-node lineage, proving Brain won.
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake},
		Brain:     testBrain{delegateTo: "reviewer"},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Lineage.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (Options.Brain not used?)", len(res.Lineage.Nodes))
	}
}

func TestRunUsesInjectedSandboxProvider(t *testing.T) {
	// A provider injected via Options.Sandbox is used for the run's sandbox: a
	// failing Create surfaces as the run's create error, proving the runner did
	// not silently fall back to sandbox/local.
	r, err := NewRunner(Options{SessionID: "run-1", Model: ModelOptions{Kind: ModelFake}, Sandbox: failCreateProvider{}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = testBrain{}
	_, err = r.Run(context.Background(), dynamicRegion(), "go")
	if err == nil || !strings.Contains(err.Error(), "create sandbox") {
		t.Fatalf("err = %v, want a create sandbox error from the injected provider", err)
	}
}

func TestRunDefaultsToLocalProvider(t *testing.T) {
	// With no Sandbox set, Run succeeds against the local temp-dir fallback.
	r, err := NewRunner(Options{SessionID: "run-1", Model: ModelOptions{Kind: ModelFake}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = testBrain{}
	res, err := r.Run(context.Background(), Region{Autonomy: Pinned, Entry: AgentSpec{Name: "solo", Role: "solo"}}, "go")
	if err != nil {
		t.Fatalf("Run with default provider: %v", err)
	}
	if res.Lineage.Nodes[0].Sandbox == "" {
		t.Error("expected the default local provider to assign a sandbox id")
	}
}

func TestRegionDirectoryReturnsPeerCards(t *testing.T) {
	region := dynamicRegion()
	cards := region.Directory()
	if len(cards) != 1 {
		t.Fatalf("cards = %+v, want 1", cards)
	}
	if cards[0].Name != "reviewer" || cards[0].Role != "review" || cards[0].Description != "reviews diffs" {
		t.Errorf("card = %+v, want reviewer/review/reviews diffs", cards[0])
	}
}

func TestRenderDirectoryEdgeCases(t *testing.T) {
	// An empty directory injects nothing.
	if got := renderDirectory(nil); got != "" {
		t.Errorf("renderDirectory(nil) = %q, want empty", got)
	}
	// A bare card (no role, no description) renders just its name line.
	got := renderDirectory([]PeerCard{{Name: "solo"}})
	if !strings.Contains(got, "- solo\n") {
		t.Errorf("bare card line missing in:\n%s", got)
	}
	if strings.Contains(got, "solo (") || strings.Contains(got, "solo:") {
		t.Errorf("bare card rendered role/description annotations:\n%s", got)
	}
}

func TestComposeSystemBranches(t *testing.T) {
	cases := []struct {
		base, dir, want string
	}{
		{"", "", ""},
		{"base only", "", "base only"},
		{"", "dir only", "dir only"},
		{"base", "dir", "base\n\ndir"},
	}
	for _, c := range cases {
		if got := composeSystem(c.base, c.dir); got != c.want {
			t.Errorf("composeSystem(%q,%q) = %q, want %q", c.base, c.dir, got, c.want)
		}
	}
}

func TestBuildModelDirectAndUnknown(t *testing.T) {
	// ModelDirect with a model id and a bearer source builds an adapter offline,
	// exercising the WithModel + WithBearerSource option branches.
	bearer := func(context.Context) (string, error) { return "tok", nil }
	m, err := buildModel(ModelOptions{
		Kind: ModelDirect, Provider: "anthropic", Model: "claude-sonnet-4-6",
		BaseURL: "http://localhost:8080/anthropic", BearerSource: bearer,
	})
	if err != nil {
		t.Fatalf("ModelDirect: %v", err)
	}
	if m == nil {
		t.Fatal("ModelDirect returned a nil model")
	}
	// ModelDirect with a non-anthropic provider is rejected.
	if _, err := buildModel(ModelOptions{Kind: ModelDirect, Provider: "openai"}); err == nil {
		t.Error("ModelDirect with unsupported provider: want error, got nil")
	}
	// An unknown kind is rejected.
	if _, err := buildModel(ModelOptions{Kind: "satellite"}); err == nil {
		t.Error("unknown kind: want error, got nil")
	}
}

func TestRunUnknownAutonomy(t *testing.T) {
	r := newTestRunner(t, testBrain{})
	_, err := r.Run(context.Background(), Region{Autonomy: "freeform", Entry: AgentSpec{Name: "x"}}, "go")
	if err == nil {
		t.Fatal("want error for unknown autonomy, got nil")
	}
	if !strings.Contains(err.Error(), "unknown autonomy") {
		t.Errorf("error = %v, want unknown autonomy", err)
	}
}

func TestSessionDefaultsWhenEmpty(t *testing.T) {
	// An empty SessionID falls back to the literal "session" prefix for node ids.
	r, err := NewRunner(Options{Model: ModelOptions{Kind: ModelFake}})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = testBrain{}
	res, err := r.Run(context.Background(), Region{
		Autonomy: Pinned, Entry: AgentSpec{Name: "solo", Role: "solo"},
	}, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Lineage.Nodes[0].ID != "session/solo" {
		t.Errorf("node id = %q, want session/solo", res.Lineage.Nodes[0].ID)
	}
}

func TestRunDynamicEntryFailureMarksFailed(t *testing.T) {
	r := newTestRunner(t, failBrain{})
	res, err := r.Run(context.Background(), dynamicRegion(), "go")
	if err == nil {
		t.Fatal("want error when the entry agent's loop fails, got nil")
	}
	if len(res.Lineage.Nodes) != 1 || res.Lineage.Nodes[0].Status != StatusFailed {
		t.Errorf("entry node = %+v, want a single StatusFailed node", res.Lineage.Nodes)
	}
}

func TestRunPinnedStepFailureMarksFailed(t *testing.T) {
	r := newTestRunner(t, failBrain{})
	res, err := r.Run(context.Background(), Region{
		Autonomy: Pinned,
		Entry:    AgentSpec{Name: "impl", Role: "impl"},
		Peers:    []AgentSpec{{Name: "test", Role: "test"}},
	}, "go")
	if err == nil {
		t.Fatal("want error when a pinned step fails, got nil")
	}
	// The chain stops at the first failing step: one node, marked failed, no edges.
	if len(res.Lineage.Nodes) != 1 || res.Lineage.Nodes[0].Status != StatusFailed {
		t.Errorf("nodes = %+v, want a single StatusFailed node", res.Lineage.Nodes)
	}
	if len(res.Lineage.Edges) != 0 {
		t.Errorf("edges = %+v, want none (chain stopped at step 1)", res.Lineage.Edges)
	}
}

// newDelegate builds a delegate tool wired to a freshly constructed lineage whose
// only node is the delegating entry, mirroring how runAgent registers it.
func newDelegate(r *Runner, parent harness.ParentContext, topo Topology) (*delegateTool, *Lineage) {
	entryID := "run-1/lead"
	lin := &Lineage{Nodes: []LineageNode{{ID: entryID, Name: "lead", Role: "lead", Status: StatusRunning}}}
	dir := []AgentSpec{{Name: "reviewer", Role: "review", Tools: []string{"x"}, Scopes: []string{"s"}}}
	return &delegateTool{
		runner: r, dir: dir, parent: parent, topology: topo,
		depth: 0, entryID: entryID, lineage: lin, path: "",
	}, lin
}

func entryParent() harness.ParentContext {
	return harness.ParentContext{
		SessionID: "run-1", AgentID: "lead",
		Perms: harness.Permissions{Scopes: []string{"s"}, Tools: []string{"x"}},
	}
}

func TestDelegateBadInput(t *testing.T) {
	r := newTestRunner(t, testBrain{})
	d, lin := newDelegate(r, entryParent(), OrchestratorWorker)
	res, err := d.Invoke(context.Background(), []byte("{not json"), local.New(), "box-0")
	if err != nil {
		t.Fatalf("Invoke returned a transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "bad delegate input") {
		t.Errorf("result = %+v, want IsError with bad delegate input", res)
	}
	if len(lin.Nodes) != 1 {
		t.Errorf("malformed input created lineage nodes: %+v", lin.Nodes)
	}
}

func TestDelegateSpawnDenied(t *testing.T) {
	// A sub-agent parent without a recursion grant cannot spawn: the delegate
	// surfaces the spawn refusal as a tool error and records no child node.
	r := newTestRunner(t, testBrain{})
	parent := entryParent()
	parent.IsSubAgent = true
	parent.Perms.AllowRecurse = false
	d, lin := newDelegate(r, parent, Mesh)
	res, err := d.Invoke(context.Background(), []byte(`{"peer":"reviewer","task":"go"}`), local.New(), "box-0")
	if err != nil {
		t.Fatalf("Invoke transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "spawn failed") {
		t.Errorf("result = %+v, want IsError with spawn failed", res)
	}
	if len(lin.Nodes) != 1 {
		t.Errorf("spawn refusal created a child node: %+v", lin.Nodes)
	}
}

func TestDelegateSandboxCreateFailure(t *testing.T) {
	// Spawn succeeds, but the per-child sandbox fails to provision: the child is
	// recorded StatusFailed and the delegate returns a tool error.
	r := newTestRunner(t, testBrain{})
	d, lin := newDelegate(r, entryParent(), OrchestratorWorker)
	res, err := d.Invoke(context.Background(), []byte(`{"peer":"reviewer","task":"go"}`), failCreateProvider{}, "box-0")
	if err != nil {
		t.Fatalf("Invoke transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "sandbox create failed") {
		t.Errorf("result = %+v, want IsError with sandbox create failed", res)
	}
	if len(lin.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want entry + failed child", lin.Nodes)
	}
	child := lin.Nodes[1]
	if child.ID != "run-1/sub/reviewer" || child.Status != StatusFailed || child.Sandbox != "" {
		t.Errorf("child node = %+v, want failed reviewer with no sandbox", child)
	}
	if len(lin.Edges) != 1 || lin.Edges[0].Kind != EdgeDelegate {
		t.Errorf("edges = %+v, want a single EdgeDelegate", lin.Edges)
	}
}

func TestDelegatePeerRunFailure(t *testing.T) {
	// Spawn and sandbox succeed, but the peer's loop fails: the delegate marks the
	// child StatusFailed and reports the run failure.
	r := newTestRunner(t, failBrain{})
	d, lin := newDelegate(r, entryParent(), OrchestratorWorker)
	res, err := d.Invoke(context.Background(), []byte(`{"peer":"reviewer","task":"go"}`), local.New(), "box-0")
	if err != nil {
		t.Fatalf("Invoke transport error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "peer run failed") {
		t.Errorf("result = %+v, want IsError with peer run failed", res)
	}
	if len(lin.Nodes) != 2 || lin.Nodes[1].Status != StatusFailed {
		t.Errorf("nodes = %+v, want entry + failed child", lin.Nodes)
	}
	// The child got its own sandbox before the loop failed.
	if lin.Nodes[1].Sandbox == "" {
		t.Errorf("failed child missing its sandbox id: %+v", lin.Nodes[1])
	}
}
