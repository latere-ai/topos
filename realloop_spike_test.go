package sdk

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/agents/internal/billing"
	"latere.ai/x/agents/internal/harness"
	"latere.ai/x/agents/internal/harness/hooks"
	"latere.ai/x/agents/internal/harness/tools"
	"latere.ai/x/agents/internal/models"
	"latere.ai/x/agents/internal/runtime/loop"
	"latere.ai/x/agents/internal/sandbox"
	"latere.ai/x/agents/internal/sandbox/local"
)

// This file is the M0.5 depth spike. M0 scripted the brain with a fake
// ModelProvider.Decide and proved only the orchestration plumbing. The open
// question the advisor flagged: does a delegation map onto the REAL agentic loop,
// where a model emits it as an interleaved tool-call inside loop.Run — not as one
// clean Action returned to the runner?
//
// This validates that it does, and in doing so shows the production mechanism is
// "register a `delegate` TOOL into the loop" (agents-as-tools), where the tool's
// Invoke performs the real attenuated spawn + (here) a nested peer loop, returning
// the peer's result back into the parent's transcript. That is a different control
// structure than M0's runner-orchestrated Decide/Action — the cheap rework this
// spike was meant to surface before M1–M5 build on the wrong seam.

// cannedStream is a models.Stream backed by a pre-built event slice.
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

// scriptModel replays one canned event-stream per turn.
type scriptModel struct {
	turns [][]models.Event
	i     atomic.Int32
}

func (m *scriptModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	n := int(m.i.Add(1)) - 1
	if n >= len(m.turns) {
		n = len(m.turns) - 1
	}
	return &cannedStream{events: m.turns[n]}, nil
}

func delegateTurn(peer, task string) []models.Event {
	input, _ := json.Marshal(map[string]string{"peer": peer, "task": task})
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

// delegateTool is the agents-as-tools handoff primitive: it looks up a peer in the
// directory, spawns it with attenuated authority, runs the peer as a nested loop,
// records the lineage, and returns the peer's output as the tool result.
type delegateTool struct {
	dir       []AgentSpec
	spawner   *harness.Spawner
	parent    harness.ParentContext
	entryID   string
	lineage   *Lineage
	peerReply string
}

func (d *delegateTool) Name() string { return "delegate" }

func (d *delegateTool) Def() models.ToolDef {
	return models.ToolDef{
		Name:        "delegate",
		Description: "Delegate a subtask to a peer agent from the directory.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"peer":{"type":"string"},"task":{"type":"string"}},"required":["peer","task"]}`),
	}
}

func (d *delegateTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.SandboxProvider, sandboxID string) (models.ToolResult, error) {
	var args struct {
		Peer string `json:"peer"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return models.ToolResult{IsError: true, Content: "bad delegate input"}, nil
	}
	peer, ok := findAgent(d.dir, args.Peer)
	if !ok {
		return models.ToolResult{IsError: true, Content: "unknown peer: " + args.Peer}, nil
	}

	// Real attenuated spawn (same primitive M0 used, now driven by the loop).
	child, err := d.spawner.Spawn(ctx, d.parent, harness.SpawnRequest{
		Label: peer.Name, Scopes: peer.Scopes, Tools: peer.Tools, Budget: d.parent.Budget,
	})
	if err != nil {
		return models.ToolResult{IsError: true, Content: "spawn failed: " + err.Error()}, nil
	}
	d.lineage.Nodes = append(d.lineage.Nodes, LineageNode{
		ID: child.ID, Name: peer.Name, Role: peer.Role, Status: "running", Grants: child.Perms.Tools,
	})
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: d.entryID, To: child.ID, Kind: "delegate"})

	// Run the peer as a real nested loop (its own session id = the deterministic
	// child id). It reuses the parent sandbox here; per-child sandboxes are M3.
	peerRes, err := loop.Run(ctx, loop.Config{
		Model:      &scriptModel{turns: [][]models.Event{endTurn(d.peerReply)}},
		Sandbox:    sb,
		SandboxID:  sandboxID,
		Tools:      tools.NewRegistry(),
		Bus:        hooks.New(),
		SessionID:  child.ID,
		AgentID:    peer.Name,
		UserPrompt: args.Task,
	})
	if err != nil {
		setStatus(d.lineage, child.ID, "failed")
		d.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "peer loop failed: " + err.Error()}, nil
	}
	setStatus(d.lineage, child.ID, "done")
	d.spawner.Stop(ctx, child)
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: child.ID, To: d.entryID, Kind: "deliver"})
	return models.ToolResult{Content: peerRes.FinalText}, nil
}

// TestRealLoopDrivesDelegateToolIntoSpawn is the load-bearing validation: a model
// driven through the REAL loop.Run emits a delegate tool-call, which the runner's
// delegate tool turns into the same attenuated spawn + lineage M0 built — plus a
// nested peer loop whose result flows back into the parent transcript.
func TestRealLoopDrivesDelegateToolIntoSpawn(t *testing.T) {
	p := local.New()
	ctx := context.Background()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	var events []hooks.EventName
	bus.Register("spy", nil, func(n hooks.EventName, _ any) hooks.Decision {
		events = append(events, n)
		return hooks.Allow()
	})

	dir := []AgentSpec{{
		Name: "reviewer", Role: "review", Description: "reviews diffs",
		Tools: []string{"write", "exec"}, Scopes: []string{"repo"},
	}}
	lineage := &Lineage{Nodes: []LineageNode{{ID: "spike/lead", Name: "lead", Role: "lead", Status: "running"}}}
	dt := &delegateTool{
		dir:     dir,
		spawner: harness.NewSpawner(bus),
		parent: harness.ParentContext{
			SessionID: "spike", AgentID: "lead",
			Perms:  harness.Permissions{Scopes: []string{"repo"}, Tools: []string{"read", "write"}},
			Budget: billing.Budget{LimitUSD: 5},
		},
		entryID:   "spike/lead",
		lineage:   lineage,
		peerReply: "looks good",
	}

	registry := tools.NewRegistry()
	registry.Register(dt)

	entry := &scriptModel{turns: [][]models.Event{
		delegateTurn("reviewer", "review the diff"),
		endTurn("done"),
	}}

	res, err := loop.Run(ctx, loop.Config{
		Model:      entry,
		Sandbox:    p,
		SandboxID:  sb.ID,
		Tools:      registry,
		Bus:        bus,
		SessionID:  "spike",
		AgentID:    "lead",
		UserPrompt: "ship the change",
	})
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}

	// The parent loop executed the delegate tool and finished.
	if res.ToolCallCount < 1 {
		t.Fatalf("ToolCallCount = %d, want >= 1", res.ToolCallCount)
	}
	if res.FinalText != "done" {
		t.Errorf("FinalText = %q, want done", res.FinalText)
	}

	// The handoff produced a real lineage: lead -> reviewer (delegate), reviewer -> lead (deliver).
	if len(lineage.Nodes) != 2 || lineage.Nodes[1].Name != "reviewer" || lineage.Nodes[1].Status != "done" {
		t.Fatalf("lineage nodes = %+v", lineage.Nodes)
	}
	if lineage.Nodes[1].ID != "spike/sub/reviewer" {
		t.Errorf("child id = %q, want spike/sub/reviewer", lineage.Nodes[1].ID)
	}
	// Attenuation held through the loop path: reviewer asked for {write,exec},
	// lead holds {read,write} -> granted {write}.
	if g := lineage.Nodes[1].Grants; len(g) != 1 || g[0] != "write" {
		t.Errorf("grants = %v, want [write]", g)
	}

	// The peer's reply flowed back into the parent transcript as a tool result.
	var sawPeerReply bool
	for _, m := range res.Transcript {
		for _, tr := range m.ToolResults {
			if strings.Contains(tr.Content, "looks good") {
				sawPeerReply = true
			}
		}
	}
	if !sawPeerReply {
		t.Error("peer reply 'looks good' did not flow back into the parent transcript")
	}

	// Real Subagent hook events fired for the delegated child.
	if !containsEvent(events, hooks.EventSubagentStart) || !containsEvent(events, hooks.EventSubagentStop) {
		t.Errorf("missing Subagent events, got %v", events)
	}
}
