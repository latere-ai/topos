// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package loop_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/harness/hooks"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runtime/loop"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
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
// and falls back to chat-only (no tools, ToolCallCount==0).
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

// --- helpers for the error/edge-path tests below ---

// toolCallModel emits a configurable set of tool calls on turn 1 (StopToolUse),
// then a terminal "done" turn on every subsequent call. It drives the loop's
// tool-dispatch paths with arbitrary tool names/inputs.
type toolCallModel struct {
	calls []models.ToolCall
	turn  atomic.Int32
}

func (m *toolCallModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	if m.turn.Add(1) != 1 {
		return &cannedStream{events: endTurnEvents()}, nil
	}
	evs := make([]models.Event, 0, len(m.calls)+2)
	evs = append(evs, models.Event{Kind: models.KindTextDelta, TextDelta: "working"})
	for i := range m.calls {
		tc := m.calls[i]
		evs = append(evs, models.Event{Kind: models.KindToolCallDone, ToolCall: &tc})
	}
	evs = append(evs, models.Event{Kind: models.KindDone, StopReason: models.StopToolUse})
	return &cannedStream{events: evs}, nil
}

// loopingToolModel always emits the same tool call with StopToolUse, never
// terminating on its own — used to exercise the MaxIterations cap.
type loopingToolModel struct{ call models.ToolCall }

func (m *loopingToolModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	tc := m.call
	return &cannedStream{events: []models.Event{
		{Kind: models.KindToolCallDone, ToolCall: &tc},
		{Kind: models.KindDone, StopReason: models.StopToolUse},
	}}, nil
}

// errStreamModel returns an error directly from Stream.
type errStreamModel struct{ err error }

func (m *errStreamModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return nil, m.err
}

// retryFailModel rejects tools with ErrToolsUnsupported, then fails the
// tool-less retry with a different error — exercising the retry-then-fail path.
type retryFailModel struct{ retryErr error }

func (m *retryFailModel) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	if len(req.Tools) > 0 {
		return nil, fmt.Errorf("retryFailModel: %w", models.ErrToolsUnsupported)
	}
	return nil, m.retryErr
}

// recvErrStream returns a non-EOF error from Recv.
type recvErrStream struct{ err error }

func (s *recvErrStream) Recv() (models.Event, error) { return models.Event{}, s.err }
func (s *recvErrStream) Close() error                { return nil }

type recvErrModel struct{ err error }

func (m *recvErrModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return &recvErrStream{err: m.err}, nil
}

// cancelStream cancels the run context while draining, so the loop observes
// cancellation at the top of the inner drain loop.
type cancelStream struct {
	cancel context.CancelFunc
	pos    int
}

func (s *cancelStream) Recv() (models.Event, error) {
	if s.pos == 0 {
		s.pos++
		s.cancel()
		return models.Event{Kind: models.KindTextDelta, TextDelta: "partial"}, nil
	}
	return models.Event{}, io.EOF
}
func (s *cancelStream) Close() error { return nil }

type cancelModel struct{ cancel context.CancelFunc }

func (m *cancelModel) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return &cancelStream{cancel: m.cancel}, nil
}

// stubTool is a registry tool with a fixed result/error and an optional record
// of the (normalised) input it was invoked with.
type stubTool struct {
	name     string
	res      models.ToolResult
	err      error
	gotInput *string
}

func (s *stubTool) Name() string { return s.name }
func (s *stubTool) Def() models.ToolDef {
	return models.ToolDef{Name: s.name, InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (s *stubTool) Invoke(_ context.Context, input json.RawMessage, _ sandbox.Provider, _ string) (models.ToolResult, error) {
	if s.gotInput != nil {
		*s.gotInput = string(input)
	}
	return s.res, s.err
}

func registryWith(ts ...tools.Tool) *tools.Registry {
	r := tools.NewRegistry()
	for _, t := range ts {
		r.Register(t)
	}
	return r
}

func baseCfg(model models.Model, reg *tools.Registry) loop.Config {
	return loop.Config{
		Model:      model,
		Tools:      reg,
		Bus:        hooks.New(),
		SessionID:  "edge-test",
		UserPrompt: "go",
	}
}

func TestLoopContextCancelledBeforeIteration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Run starts

	cfg := baseCfg(&fakeModel{}, tools.Builtins())
	_, err := loop.Run(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLoopModelStreamErrorIsUnrecoverable(t *testing.T) {
	sentinel := errors.New("boom: model offline")
	cfg := baseCfg(&errStreamModel{err: sentinel}, tools.Builtins())
	_, err := loop.Run(context.Background(), cfg)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap %v", err, sentinel)
	}
}

func TestLoopToolsUnsupportedRetryAlsoFails(t *testing.T) {
	retryErr := errors.New("retry also broke")
	cfg := baseCfg(&retryFailModel{retryErr: retryErr}, tools.Builtins())
	_, err := loop.Run(context.Background(), cfg)
	if !errors.Is(err, retryErr) {
		t.Fatalf("err = %v, want it to wrap the retry error", err)
	}
}

func TestLoopStreamRecvError(t *testing.T) {
	recvErr := errors.New("transport reset")
	cfg := baseCfg(&recvErrModel{err: recvErr}, tools.Builtins())
	_, err := loop.Run(context.Background(), cfg)
	if !errors.Is(err, recvErr) {
		t.Fatalf("err = %v, want it to wrap the recv error", err)
	}
}

func TestLoopContextCancelledDuringDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := baseCfg(&cancelModel{cancel: cancel}, tools.Builtins())
	_, err := loop.Run(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestLoopToolNotFoundInRegistry(t *testing.T) {
	// Model asks for "bash" but the registry is empty: the loop must record an
	// error tool result and continue to termination.
	model := &toolCallModel{calls: []models.ToolCall{
		{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)},
	}}
	cfg := baseCfg(model, tools.NewRegistry())
	result, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", result.ToolCallCount)
	}
	if !hasErrorResultContaining(result.Transcript, "not found") {
		t.Fatal("expected a 'not found' error tool result in the transcript")
	}
}

func TestLoopToolInvokeError(t *testing.T) {
	invokeErr := errors.New("sandbox exec failed")
	boom := &stubTool{name: "boom", err: invokeErr}
	model := &toolCallModel{calls: []models.ToolCall{
		{ID: "c1", Name: "boom", Input: json.RawMessage(`{}`)},
	}}
	cfg := baseCfg(model, registryWith(boom))

	var failures int
	cfg.Bus.Register("count-fail", []hooks.EventName{hooks.EventPostToolUseFailure},
		func(_ hooks.EventName, _ any) hooks.Decision { failures++; return hooks.Allow() })

	result, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasErrorResultContaining(result.Transcript, "invoke error") {
		t.Fatal("expected an 'invoke error' tool result in the transcript")
	}
	if failures != 1 {
		t.Fatalf("PostToolUseFailure fired %d times, want 1", failures)
	}
}

func TestLoopToolReportsErrorResultWithoutGoError(t *testing.T) {
	// A tool that returns IsError=true but a nil Go error: the loop must surface
	// the tool's own error content (the else branch), not wrap an invoke error.
	soft := &stubTool{name: "soft", res: models.ToolResult{IsError: true, Content: "domain failure"}}
	model := &toolCallModel{calls: []models.ToolCall{
		{ID: "c1", Name: "soft", Input: json.RawMessage(`{}`)},
	}}
	cfg := baseCfg(model, registryWith(soft))
	result, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !hasErrorResultContaining(result.Transcript, "domain failure") {
		t.Fatal("expected the tool's own error content in the transcript")
	}
}

func TestLoopNullToolInputNormalised(t *testing.T) {
	// A tool call with nil Input must reach the tool as "{}".
	var got string
	noop := &stubTool{name: "noop", res: models.ToolResult{Content: "ok"}, gotInput: &got}
	model := &toolCallModel{calls: []models.ToolCall{
		{ID: "c1", Name: "noop", Input: nil},
	}}
	cfg := baseCfg(model, registryWith(noop))
	if _, err := loop.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "{}" {
		t.Fatalf("tool received input %q, want %q", got, "{}")
	}
}

func TestLoopMaxIterationsCap(t *testing.T) {
	// A model that never stops requesting tools must be capped at MaxIterations.
	noop := &stubTool{name: "noop", res: models.ToolResult{Content: "again"}}
	model := &loopingToolModel{call: models.ToolCall{ID: "c", Name: "noop", Input: json.RawMessage(`{}`)}}
	cfg := baseCfg(model, registryWith(noop))
	result, err := loop.Run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ToolCallCount != loop.MaxIterations {
		t.Fatalf("ToolCallCount = %d, want MaxIterations (%d)", result.ToolCallCount, loop.MaxIterations)
	}
	if result.StopReason != models.StopToolUse {
		t.Fatalf("StopReason = %q, want tool_use (capped, not a natural stop)", result.StopReason)
	}
}

// hasErrorResultContaining reports whether the transcript holds a tool-role
// message with an error result whose content contains sub.
func hasErrorResultContaining(transcript []models.Message, sub string) bool {
	for _, msg := range transcript {
		if msg.Role != "tool" {
			continue
		}
		for _, tr := range msg.ToolResults {
			if tr.IsError && strings.Contains(tr.Content, sub) {
				return true
			}
		}
	}
	return false
}
