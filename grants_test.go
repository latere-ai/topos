// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package topos

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

type grantSandbox struct {
	sandbox.Provider
	execs, writes atomic.Int32
}

func (p *grantSandbox) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	box, err := p.Provider.Create(ctx, opts)
	if err == nil {
		err = p.Provider.WriteFile(ctx, box.ID, "input.txt", []byte("readable"))
	}
	return box, err
}

func (p *grantSandbox) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	p.execs.Add(1)
	return p.Provider.Exec(ctx, id, opts)
}

func (p *grantSandbox) WriteFile(ctx context.Context, id, path string, data []byte) error {
	p.writes.Add(1)
	return p.Provider.WriteFile(ctx, id, path, data)
}

type grantModel struct {
	t       *testing.T
	want    map[string][]string
	seen    map[string][]string
	actions map[string][]models.ToolCall
}

func (m *grantModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	name := strings.Fields(req.System)[0]
	names := make([]string, len(req.Tools))
	for i, tool := range req.Tools {
		names[i] = tool.Name
	}
	m.seen[name] = names
	if !slices.Equal(names, m.want[name]) {
		m.t.Errorf("%s offered %v, want %v", name, names, m.want[name])
	}
	for _, message := range req.Messages {
		if message.Role != models.RoleTool {
			continue
		}
		for _, result := range message.ToolResults {
			allowed := slices.Contains(m.want[name], result.CallID)
			if result.IsError == allowed {
				m.t.Errorf("%s tool %s result = %+v, allowed=%v", name, result.CallID, result, allowed)
			}
			if result.CallID == "read_file" && allowed && !strings.Contains(result.Content, "readable") {
				m.t.Errorf("allowed read returned %q", result.Content)
			}
		}
		return &cannedStream{events: endTurn("checked")}, nil
	}
	var events []models.Event
	for _, call := range m.actions[name] {
		events = append(events, models.Event{Kind: models.KindToolCallDone, ToolCall: &call})
	}
	if len(events) == 0 {
		return &cannedStream{events: endTurn("checked")}, nil
	}
	events = append(events, models.Event{Kind: models.KindDone, StopReason: models.StopToolUse})
	return &cannedStream{events: events}, nil
}

func forcedCalls() []models.ToolCall {
	return []models.ToolCall{
		{ID: "read_file", Name: "read_file", Input: json.RawMessage(`{"path":"input.txt"}`)},
		{ID: "bash", Name: "bash", Input: json.RawMessage(`{"command":"echo changed > owned.txt"}`)},
		{ID: "write_file", Name: "write_file", Input: json.RawMessage(`{"path":"owned.txt","content":"changed"}`)},
		{ID: "edit_file", Name: "edit_file", Input: json.RawMessage(`{"path":"input.txt","old_string":"readable","new_string":"changed"}`)},
	}
}

func runGrantCase(t *testing.T, region Region, mdl *grantModel, maxDepth int) RunResult {
	t.Helper()
	box := &grantSandbox{Provider: local.New()}
	r, err := NewRunner(Options{SessionID: "grants", Model: ModelOptions{Client: mdl}, Sandbox: box, MaxHandoffDepth: maxDepth})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(t.Context(), region, "check grants")
	if err != nil {
		t.Fatal(err)
	}
	if box.execs.Load() != 0 || box.writes.Load() != 0 {
		t.Errorf("denied tools reached sandbox: exec=%d writes=%d", box.execs.Load(), box.writes.Load())
	}
	for _, node := range res.Trace.Nodes {
		if !slices.Equal(node.Grants, mdl.seen[node.Name]) {
			t.Errorf("%s trace grants %v differ from offered %v", node.Name, node.Grants, mdl.seen[node.Name])
		}
	}
	return res
}

func TestAgentGrantsEnforced(t *testing.T) {
	for _, autonomy := range []Autonomy{Pinned, Dynamic} {
		t.Run(string(autonomy), func(t *testing.T) {
			mdl := &grantModel{t: t, want: map[string][]string{"lead": {"read_file", "grep", "glob"}, "worker": {}},
				seen: map[string][]string{}, actions: map[string][]models.ToolCall{"lead": forcedCalls(), "worker": forcedCalls()}}
			region := Region{Autonomy: autonomy, Entry: AgentSpec{Name: "lead", SystemPrompt: "lead", Tools: []string{"read"}}}
			if autonomy == Pinned {
				region.Peers = []AgentSpec{{Name: "worker", SystemPrompt: "worker", Tools: []string{}}}
			}
			runGrantCase(t, region, mdl, 3)
		})
	}
}

func TestAgentNilGrantsPreserveBuiltins(t *testing.T) {
	for _, autonomy := range []Autonomy{Pinned, Dynamic} {
		t.Run(string(autonomy), func(t *testing.T) {
			mdl := &grantModel{t: t, seen: map[string][]string{},
				want: map[string][]string{"lead": {"bash", "read_file", "write_file", "edit_file", "grep", "glob"}}}
			runGrantCase(t, Region{Autonomy: autonomy, Entry: AgentSpec{Name: "lead", SystemPrompt: "lead"}}, mdl, 3)
		})
	}
}

func TestDelegatedGrantsCannotRegainAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer []string
		want []string
	}{
		{"inherit", nil, []string{"read_file"}},
		{"family intersection", []string{"read"}, []string{"read_file"}},
		{"disjoint", []string{"write"}, []string{}},
		{"empty", []string{}, []string{}},
		{"unknown", []string{"missing"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delegate := func(peer string) models.ToolCall {
				return models.ToolCall{ID: "delegate", Name: "delegate", Input: json.RawMessage(`{"peer":"` + peer + `","task":"check"}`)}
			}
			mdl := &grantModel{t: t, seen: map[string][]string{},
				want:    map[string][]string{"lead": {"read_file", "delegate"}, "worker": append(slices.Clone(tc.want), "delegate"), "leaf": tc.want},
				actions: map[string][]models.ToolCall{"lead": {delegate("worker")}, "worker": {delegate("leaf")}, "leaf": forcedCalls()}}
			region := Region{Autonomy: Dynamic, Topology: Mesh,
				Entry: AgentSpec{Name: "lead", SystemPrompt: "lead", Tools: []string{"read_file"}},
				Peers: []AgentSpec{{Name: "worker", SystemPrompt: "worker", Tools: tc.peer}, {Name: "leaf", SystemPrompt: "leaf"}}}
			res := runGrantCase(t, region, mdl, 2)
			if len(res.Trace.Nodes) != 3 {
				t.Fatalf("wanted three executed nodes, got %+v", res.Trace.Nodes)
			}
		})
	}
}
