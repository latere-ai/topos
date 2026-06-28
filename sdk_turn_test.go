// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/fake"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// countingBrain replies with the number of user messages it sees, so a test can
// prove that a turn seeded from a prior transcript actually carries the history.
type countingBrain struct{}

func (countingBrain) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	n := 0
	for _, m := range req.Messages {
		if m.Role == "user" {
			n++
		}
	}
	return &cannedStream{events: endTurn(fmt.Sprintf("saw %d", n))}, nil
}

// newSandbox creates a caller-owned local sandbox for a turn and returns it with
// a cleanup. This mirrors how a host owns a persistent workspace across turns.
func newSandbox(t *testing.T) (sandbox.Provider, string) {
	t.Helper()
	p := local.New()
	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Cleanup(func() { _ = p.Destroy(context.Background(), sb.ID) })
	return p, sb.ID
}

// TestTurnThreadsTranscriptAcrossTurns proves the core multi-turn contract: the
// transcript returned by one Turn, fed as the next turn's InitialTranscript,
// carries the full history to the model and grows.
func TestTurnThreadsTranscriptAcrossTurns(t *testing.T) {
	r := newTestRunner(t, countingBrain{})
	p, sbID := newSandbox(t)

	first, err := r.Turn(context.Background(), TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent", UserPrompt: "first",
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if first.Final != "saw 1" {
		t.Fatalf("turn 1 final = %q, want 'saw 1'", first.Final)
	}

	second, err := r.Turn(context.Background(), TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent",
		InitialTranscript: first.Transcript, UserPrompt: "second",
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if second.Final != "saw 2" {
		t.Fatalf("turn 2 final = %q, want 'saw 2' (history not threaded)", second.Final)
	}
	if second.Transcript[0].Role != "user" || second.Transcript[0].Content != "first" {
		t.Fatalf("turn 2 transcript did not begin with the seed: %+v", second.Transcript[0])
	}
	if len(second.Transcript) <= len(first.Transcript) {
		t.Fatalf("transcript did not grow: %d -> %d", len(first.Transcript), len(second.Transcript))
	}
}

// TestTurnDefaultsToBuiltinTools proves that a nil TurnInput.Tools falls back to
// the built-in tool set, so a turn can execute a bash tool call end-to-end
// against the caller-owned sandbox.
func TestTurnDefaultsToBuiltinTools(t *testing.T) {
	r := newTestRunner(t, fake.New()) // fake brain: bash call, then done
	p, sbID := newSandbox(t)

	res, err := r.Turn(context.Background(), TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent", UserPrompt: "echo hello",
	})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.ToolCalls < 1 {
		t.Fatalf("ToolCalls = %d, want >= 1 (builtin bash should have run)", res.ToolCalls)
	}
	if res.StopReason != models.StopEndTurn {
		t.Fatalf("StopReason = %q, want end_turn", res.StopReason)
	}
	hasTool := false
	for _, m := range res.Transcript {
		if m.Role == "tool" {
			hasTool = true
		}
	}
	if !hasTool {
		t.Fatal("no tool-role message in transcript")
	}
}

// TestTurnInterruptReturnsPartialNoError proves an interrupted turn is reported
// via Interrupted (with a nil error) and keeps the partial transcript.
func TestTurnInterruptReturnsPartialNoError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newTestRunner(t, &turnCancelBrain{cancel: cancel})
	p, sbID := newSandbox(t)

	res, err := r.Turn(ctx, TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent", UserPrompt: "long task",
	})
	if err != nil {
		t.Fatalf("interrupt must not be reported as an error, got: %v", err)
	}
	if !res.Interrupted {
		t.Fatal("Interrupted = false, want true")
	}
	assist, ok := lastAssistant(res.Transcript)
	if !ok || assist.Content != "partial" {
		t.Fatalf("partial assistant turn = %q (ok=%v), want 'partial'", assist.Content, ok)
	}
}

// TestTurnStreamFailureIsError proves a genuine infrastructure failure (model
// stream error) is returned as an error, not swallowed as an interrupt.
func TestTurnStreamFailureIsError(t *testing.T) {
	r := newTestRunner(t, &turnErrBrain{})
	p, sbID := newSandbox(t)

	_, err := r.Turn(context.Background(), TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent", UserPrompt: "go",
	})
	if err == nil {
		t.Fatal("want a non-nil error for a model stream failure")
	}
}

// TestTurnObserverSeesTokenDeltas proves the session Observer receives streamed
// token deltas during a Turn (token-by-token rendering for a host).
func TestTurnObserverSeesTokenDeltas(t *testing.T) {
	var mu sync.Mutex
	var deltas []string
	r, err := NewRunner(Options{
		SessionID: "obs-turn", Model: ModelOptions{Kind: ModelFake},
		Observer: func(e Event) {
			if e.Name == EventTextDelta {
				mu.Lock()
				deltas = append(deltas, e.SessionID)
				mu.Unlock()
			}
		},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	r.model = countingBrain{}
	p, sbID := newSandbox(t)

	if _, err := r.Turn(context.Background(), TurnInput{
		Sandbox: p, SandboxID: sbID, AgentID: "agent", UserPrompt: "hi",
	}); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deltas) == 0 {
		t.Fatal("observer received no token deltas")
	}
	// SessionID on a delta joins to the runner's session id.
	if deltas[0] != "obs-turn" {
		t.Fatalf("delta SessionID = %q, want the runner session id 'obs-turn'", deltas[0])
	}
}

// lastAssistant returns the last assistant message in the transcript.
func lastAssistant(transcript []models.Message) (models.Message, bool) {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == "assistant" {
			return transcript[i], true
		}
	}
	return models.Message{}, false
}

// turnCancelBrain streams a "partial" fragment, then cancels the context, so the
// loop observes cancellation mid-turn.
type turnCancelBrain struct{ cancel context.CancelFunc }

func (b *turnCancelBrain) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return &turnCancelStream{cancel: b.cancel}, nil
}

type turnCancelStream struct {
	cancel context.CancelFunc
	pos    int
}

func (s *turnCancelStream) Recv() (models.Event, error) {
	if s.pos == 0 {
		s.pos++
		s.cancel()
		return models.Event{Kind: models.KindTextDelta, TextDelta: "partial"}, nil
	}
	return models.Event{}, context.Canceled
}
func (s *turnCancelStream) Close() error { return nil }

// turnErrBrain fails the model stream outright.
type turnErrBrain struct{}

func (turnErrBrain) Stream(_ context.Context, _ models.Request) (models.Stream, error) {
	return nil, fmt.Errorf("model offline")
}
