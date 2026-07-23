// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package fake provides a deterministic models.Model implementation for local
// development and tests. It requires no external services.
//
// Behaviour:
//   - Turn 1: emits a bash tool call carrying {"command":"echo <first-user-message>"},
//     StopReason=tool_use.
//   - Turn 2+: emits "done" text + StopReason=end_turn.
//
// This produces a minimal but complete autonomous run: the agentic loop
// dispatches the bash tool call, the sandbox echoes the prompt, and the model
// terminates. Users can observe the round-trip without any API keys.
//
// Every turn reports a cost of zero. The fake reaches no provider and is billed
// by no one, so zero is its real price, not an unknown one — reporting it keeps
// the zero-config path priceable under a spend cap without needing a rate-card
// entry for a model that has no rate.
package fake

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"latere.ai/x/topos/models"
)

// freeCost is the cost every fake turn reports: a real zero, distinct from an
// unreported cost, so a metered run prices the fake instead of failing closed.
var freeCost int64

// Model is the deterministic fake model.
type Model struct{}

// New returns a fake Model.
func New() *Model { return &Model{} }

// Stream returns a canned stream. Turn 1 emits a bash tool call echoing the
// user's prompt; subsequent turns return end_turn.
func (m *Model) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	// Find the first user message to echo.
	userMsg := ""
	for _, msg := range req.Messages {
		if msg.Role == models.RoleUser {
			userMsg = msg.Content
			break
		}
	}

	// Check if this is a follow-up turn (transcript includes a tool result).
	hasPriorTool := false
	for _, msg := range req.Messages {
		if msg.Role == models.RoleTool {
			hasPriorTool = true
			break
		}
	}

	if hasPriorTool {
		return &cannedStream{events: []models.Event{
			{Kind: models.KindTextDelta, TextDelta: "Task completed: echoed your prompt in the sandbox."},
			{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 20, OutputTokens: 10, CostUSDMicro: &freeCost}},
			{Kind: models.KindDone, StopReason: models.StopEndTurn},
		}}, nil
	}

	input, _ := json.Marshal(map[string]string{"command": "echo " + userMsg})
	return &cannedStream{events: []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "I will run your prompt in the sandbox."},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{
			ID:    "fake_call_1",
			Name:  "bash",
			Input: input,
		}},
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 15, OutputTokens: 8, CostUSDMicro: &freeCost}},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}}, nil
}

type cannedStream struct {
	mu     sync.Mutex
	events []models.Event
	pos    int
}

func (s *cannedStream) Recv() (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pos >= len(s.events) {
		return models.Event{}, io.EOF
	}
	ev := s.events[s.pos]
	s.pos++
	return ev, nil
}

func (s *cannedStream) Close() error { return nil }
