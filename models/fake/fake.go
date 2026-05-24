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
package fake

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"latere.ai/x/agents/internal/models"
)

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
		if msg.Role == "user" {
			userMsg = msg.Content
			break
		}
	}

	// Check if this is a follow-up turn (transcript includes a tool result).
	hasPriorTool := false
	for _, msg := range req.Messages {
		if msg.Role == "tool" {
			hasPriorTool = true
			break
		}
	}

	if hasPriorTool {
		return &cannedStream{events: []models.Event{
			{Kind: models.KindTextDelta, TextDelta: "Task completed: echoed your prompt in the sandbox."},
			{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 20, OutputTokens: 10}},
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
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 15, OutputTokens: 8}},
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
