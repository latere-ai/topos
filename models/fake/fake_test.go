package fake_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"latere.ai/x/agents/internal/models"
	fakemodelimpl "latere.ai/x/agents/internal/models/fake"
)

// TestFakeModel_Turn1_ToolCall verifies the fake model emits a bash tool call
// on the first turn (no prior tool results in transcript).
func TestFakeModel_Turn1_ToolCall(t *testing.T) {
	m := fakemodelimpl.New()
	req := models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "say hello"},
		},
	}
	stream, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var gotToolCall bool
	var stopReason models.StopReason
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Kind == models.KindToolCallDone {
			gotToolCall = true
			if ev.ToolCall == nil || ev.ToolCall.Name != "bash" {
				t.Fatalf("expected bash tool call, got %+v", ev.ToolCall)
			}
		}
		if ev.Kind == models.KindDone {
			stopReason = ev.StopReason
		}
	}
	if !gotToolCall {
		t.Fatal("expected tool call on turn 1")
	}
	if stopReason != models.StopToolUse {
		t.Fatalf("stop_reason = %q, want %q", stopReason, models.StopToolUse)
	}
}

// TestFakeModel_Turn2_EndTurn verifies the fake model emits end_turn text
// when a prior tool result is in the transcript.
func TestFakeModel_Turn2_EndTurn(t *testing.T) {
	m := fakemodelimpl.New()
	req := models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "say hello"},
			{Role: "tool", Content: "hello"},
		},
	}
	stream, err := m.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var stopReason models.StopReason
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Kind == models.KindTextDelta {
			text.WriteString(ev.TextDelta)
		}
		if ev.Kind == models.KindDone {
			stopReason = ev.StopReason
		}
	}
	if !strings.Contains(text.String(), "Task completed") {
		t.Fatalf("output = %q, want to contain 'Task completed'", text.String())
	}
	if stopReason != models.StopEndTurn {
		t.Fatalf("stop_reason = %q, want %q", stopReason, models.StopEndTurn)
	}
}

// TestFakeModel_ImplementsInterface is a compile-time check.
var _ models.Model = (*fakemodelimpl.Model)(nil)
