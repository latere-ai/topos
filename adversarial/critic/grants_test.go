// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package critic_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos"
	nativecritic "latere.ai/x/topos/adversarial/critic"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

type criticGrantModel struct {
	t    *testing.T
	want []string
}

func (m criticGrantModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	names := make([]string, len(req.Tools))
	for i, tool := range req.Tools {
		names[i] = tool.Name
	}
	if !slices.Equal(names, m.want) {
		m.t.Errorf("critic tools = %v, want %v", names, m.want)
	}
	for _, message := range req.Messages {
		if message.Role != models.RoleTool {
			continue
		}
		for _, result := range message.ToolResults {
			if result.IsError == slices.Contains(m.want, result.CallID) {
				m.t.Errorf("critic dispatched tool outside its grant: %+v", result)
			}
		}
		return scriptedModel{text: "critique"}.Stream(context.Background(), req)
	}
	return &scriptStream{events: []models.Event{
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "bash", Name: "bash", Input: json.RawMessage(`{"command":"echo changed"}`)}},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "write_file", Name: "write_file", Input: json.RawMessage(`{"path":"x","content":"changed"}`)}},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "read_file", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}}, nil
}

type criticGrantSandbox struct {
	sandbox.Provider
	execs, writes, reads atomic.Int32
}

func (p *criticGrantSandbox) Exec(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
	p.execs.Add(1)
	return sandbox.ExecResult{Phase: "exited"}, nil
}

func (p *criticGrantSandbox) WriteFile(context.Context, string, string, []byte) error {
	p.writes.Add(1)
	return nil
}

func (p *criticGrantSandbox) ReadFile(context.Context, string, string) ([]byte, error) {
	p.reads.Add(1)
	return []byte("source"), nil
}

func TestCriticToolGrantsEnforced(t *testing.T) {
	for _, grants := range [][]string{nil, {}, {"read_file"}} {
		box := &criticGrantSandbox{Provider: local.New()}
		res := runOnce(t, nativecritic.Config{Sandbox: box, Tools: grants,
			Model: topos.ModelOptions{Client: criticGrantModel{t: t, want: grants}}}, securityInput())
		if res.Markdown != "critique" || box.execs.Load() != 0 || box.writes.Load() != 0 {
			t.Fatalf("critic result=%q exec=%d writes=%d", res.Markdown, box.execs.Load(), box.writes.Load())
		}
		if got := box.reads.Load(); got != int32(len(grants)) {
			t.Fatalf("reads = %d, want %d", got, len(grants))
		}
	}
}
