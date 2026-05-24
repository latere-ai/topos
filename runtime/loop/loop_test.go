package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/agents/internal/harness/hooks"
	"latere.ai/x/agents/internal/harness/tools"
	"latere.ai/x/agents/internal/models"
	"latere.ai/x/agents/internal/runtime/loop"
	"latere.ai/x/agents/internal/sandbox"
	"latere.ai/x/agents/internal/sandbox/local"
)

// fakeModel produces a canned two-turn response:
//   - Turn 1: emit a bash tool call carrying {"command":"echo <prompt>"}, StopReason=tool_use.
//   - Turn 2: emit text "done", StopReason=end_turn.
type fakeModel struct {
	prompt string
	turn   atomic.Int32
}

func (f *fakeModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	t := f.turn.Add(1)
	switch t {
	case 1:
		// Extract prompt from first user message.
		prompt := req.Messages[0].Content
		if f.prompt != "" {
			prompt = f.prompt
		}
		return &cannedStream{events: bashTurnEvents(prompt)}, nil
	default:
		return &cannedStream{events: endTurnEvents()}, nil
	}
}

// bashTurnEvents builds the event stream for a turn that emits a bash tool call.
func bashTurnEvents(prompt string) []models.Event {
	input, _ := json.Marshal(map[string]string{"command": "echo " + prompt})
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "running"},
		{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{
			ID:    "call_1",
			Name:  "bash",
			Input: input,
		}},
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 10, OutputTokens: 5}},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}
}

// endTurnEvents builds the event stream for a terminal turn.
func endTurnEvents() []models.Event {
	return []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "done"},
		{Kind: models.KindUsage, Usage: &models.Usage{InputTokens: 5, OutputTokens: 3}},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}
}

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

func TestLoopRunsBashToolAndTerminates(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	registry := tools.Builtins()
	model := &fakeModel{prompt: "hello"}

	cfg := loop.Config{
		Model:        model,
		Sandbox:      p,
		SandboxID:    sb.ID,
		Tools:        registry,
		Bus:          bus,
		SessionID:    "test-session",
		AgentID:      "test-agent",
		SystemPrompt: "You are a helpful assistant.",
		UserPrompt:   "hello",
	}

	result, err := loop.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Assert bash ran and output captured.
	if result.ToolCallCount < 1 {
		t.Fatalf("tool_calls = %d, want >= 1", result.ToolCallCount)
	}
	if result.StopReason != models.StopEndTurn {
		t.Fatalf("stop_reason = %q, want end_turn", result.StopReason)
	}
	if result.FinalText != "done" {
		t.Fatalf("final_text = %q, want 'done'", result.FinalText)
	}
	if result.TotalUsage.InputTokens == 0 {
		t.Fatal("expected non-zero input token usage")
	}
	// Tool-capable path must not set the chat-only flag.
	if result.ChatOnly {
		t.Fatal("ChatOnly = true, want false for a tool-capable model")
	}

	// Assert the transcript includes a tool result message.
	hasToolResult := false
	for _, msg := range result.Transcript {
		if msg.Role == "tool" && len(msg.ToolResults) > 0 {
			hasToolResult = true
			tr := msg.ToolResults[0]
			if !strings.Contains(tr.Content, "hello") {
				t.Fatalf("tool result content = %q, want 'hello'", tr.Content)
			}
		}
	}
	if !hasToolResult {
		t.Fatal("no tool-role message found in transcript")
	}
}

func TestLoopSessionEndFiresExactlyOnce(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	sessionEndCount := 0
	bus.Register("counter", []hooks.EventName{hooks.EventSessionEnd}, func(_ hooks.EventName, _ any) hooks.Decision {
		sessionEndCount++
		return hooks.Allow()
	})

	model := &fakeModel{}
	cfg := loop.Config{
		Model:      model,
		Sandbox:    p,
		SandboxID:  sb.ID,
		Tools:      tools.Builtins(),
		Bus:        bus,
		SessionID:  "sess-end-test",
		UserPrompt: "hi",
	}

	if _, err := loop.Run(ctx, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if sessionEndCount != 1 {
		t.Fatalf("SessionEnd fired %d times, want 1", sessionEndCount)
	}
}

func TestLoopHookDenyToolCall(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	bus.Register("deny-all-bash", []hooks.EventName{hooks.EventPreToolUse}, func(_ hooks.EventName, _ any) hooks.Decision {
		return hooks.Deny("not allowed in test")
	})

	model := &fakeModel{prompt: "hi"}
	cfg := loop.Config{
		Model:      model,
		Sandbox:    p,
		SandboxID:  sb.ID,
		Tools:      tools.Builtins(),
		Bus:        bus,
		SessionID:  "deny-test",
		UserPrompt: "hi",
	}

	result, err := loop.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Tool was counted even though it was denied (attempt = counted).
	if result.ToolCallCount < 1 {
		t.Fatalf("tool call count = %d, want >= 1", result.ToolCallCount)
	}

	// The tool result should be an error (denial).
	hasErrorResult := false
	for _, msg := range result.Transcript {
		if msg.Role == "tool" {
			for _, tr := range msg.ToolResults {
				if tr.IsError && strings.Contains(tr.Content, "denied") {
					hasErrorResult = true
				}
			}
		}
	}
	if !hasErrorResult {
		t.Fatal("expected a denied tool result in transcript")
	}
}

// chatOnlyModel returns ErrToolsUnsupported when the request contains tools;
// otherwise it returns a simple text stream that terminates with end_turn.
// This exercises the loop's chat-only fallback path.
type chatOnlyModel struct {
	streamCalls atomic.Int32 // total Stream invocations (including the retry)
}

func (m *chatOnlyModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	m.streamCalls.Add(1)
	if len(req.Tools) > 0 {
		return nil, fmt.Errorf("chatOnlyModel: %w", models.ErrToolsUnsupported)
	}
	return &cannedStream{events: endTurnEvents()}, nil
}

// TestLoopChatOnlyFallback verifies that when the model returns
// ErrToolsUnsupported the loop retries without tools, completes successfully,
// and sets ChatOnly=true with ToolCallCount==0.
func TestLoopChatOnlyFallback(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	model := &chatOnlyModel{}

	cfg := loop.Config{
		Model:        model,
		Sandbox:      p,
		SandboxID:    sb.ID,
		Tools:        tools.Builtins(),
		Bus:          bus,
		SessionID:    "chat-only-test",
		AgentID:      "test-agent",
		SystemPrompt: "You are a helpful assistant.",
		UserPrompt:   "hello",
	}

	result, err := loop.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !result.ChatOnly {
		t.Fatal("ChatOnly = false, want true after ErrToolsUnsupported fallback")
	}
	if result.ToolCallCount != 0 {
		t.Fatalf("ToolCallCount = %d, want 0 in chat-only mode", result.ToolCallCount)
	}
	if result.StopReason != models.StopEndTurn {
		t.Fatalf("StopReason = %q, want end_turn", result.StopReason)
	}
	if result.FinalText != "done" {
		t.Fatalf("FinalText = %q, want %q", result.FinalText, "done")
	}
	// Stream must have been called twice: once with tools (rejected) and once
	// without tools (succeeded).
	if got := model.streamCalls.Load(); got != 2 {
		t.Fatalf("streamCalls = %d, want 2 (initial + retry)", got)
	}
}

func TestLoopAllEventsRecorded(t *testing.T) {
	p := local.New()
	ctx := context.Background()

	sb, _ := p.Create(ctx, sandbox.CreateOptions{})
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	bus := hooks.New()
	model := &fakeModel{}

	cfg := loop.Config{
		Model:      model,
		Sandbox:    p,
		SandboxID:  sb.ID,
		Tools:      tools.Builtins(),
		Bus:        bus,
		SessionID:  "events-test",
		UserPrompt: "hi",
	}

	if _, err := loop.Run(ctx, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}

	log := bus.EventLog()
	eventNames := make(map[hooks.EventName]int, len(log))
	for _, entry := range log {
		eventNames[entry.EventName]++
	}

	required := []hooks.EventName{
		hooks.EventSessionStart,
		hooks.EventUserPromptSubmit,
		hooks.EventPreToolUse,
		hooks.EventStop,
		hooks.EventSessionEnd,
	}
	for _, name := range required {
		if eventNames[name] == 0 {
			t.Errorf("event %q not recorded in bus log", name)
		}
	}
}
