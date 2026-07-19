package agent

import (
	"strings"
	"testing"
)

// TestFormatClaudeStreamEventMalformedContent covers the branch where
// the assistant envelope parses but its content array does not: the
// event is dropped rather than surfaced.
func TestFormatClaudeStreamEventMalformedContent(t *testing.T) {
	// content is an object, not the expected array of parts.
	line := `{"type":"assistant","message":{"content":{"unexpected":"shape"}}}`
	if got := FormatClaudeStreamEvent([]byte(line)); got != "" {
		t.Errorf("malformed content should drop; got %q", got)
	}
}

// TestFormatClaudeStreamEventUnknownPartType covers the final return:
// an assistant event whose only content part is a type we do not
// surface (e.g. tool_result) produces no line.
func TestFormatClaudeStreamEventUnknownPartType(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"redacted_thinking","data":"x"}]}}`
	if got := FormatClaudeStreamEvent([]byte(line)); got != "" {
		t.Errorf("unknown part type should drop; got %q", got)
	}
}

// TestFormatCodexStreamEventInvalidJSON covers the unmarshal-error
// branch: a non-JSON line is dropped.
func TestFormatCodexStreamEventInvalidJSON(t *testing.T) {
	if got := FormatCodexStreamEvent([]byte("not json at all")); got != "" {
		t.Errorf("invalid JSON should drop; got %q", got)
	}
}

// TestFormatCodexStreamEventToolCallArgsFallback covers the fallback
// chain in the tool_call/function_call case: when path and input yield
// nothing, the summary comes from args.
func TestFormatCodexStreamEventToolCallArgsFallback(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"tool_call","name":"grep","args":{"pattern":"TODO"}}}`
	got := FormatCodexStreamEvent([]byte(line))
	if got != "  → grep: TODO" {
		t.Errorf("args fallback: got %q", got)
	}
}

// TestFormatCodexStreamEventShellCommand covers the shell_command item
// kind, the alias of command_execution.
func TestFormatCodexStreamEventShellCommand(t *testing.T) {
	line := `{"type":"item.started","item":{"type":"shell_command","command":"go test ./..."}}`
	got := FormatCodexStreamEvent([]byte(line))
	if got != "  → shell: go test ./..." {
		t.Errorf("shell_command: got %q", got)
	}
}

// TestFormatCodexStreamEventReasoningSummaryKind covers the
// reasoning_summary item kind and its content-field fallback.
func TestFormatCodexStreamEventReasoningSummaryKind(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"reasoning_summary","content":"weighing two fixes"}}`
	got := FormatCodexStreamEvent([]byte(line))
	if got != "  thinking: weighing two fixes" {
		t.Errorf("reasoning_summary: got %q", got)
	}
}

// TestFormatCodexStreamEventReasoningEmpty covers the empty-preview
// return in the reasoning case: an event with no usable text is
// dropped.
func TestFormatCodexStreamEventReasoningEmpty(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"reasoning"}}`
	if got := FormatCodexStreamEvent([]byte(line)); got != "" {
		t.Errorf("empty reasoning should drop; got %q", got)
	}
}

// TestFormatCodexStreamEventUnknownItemType covers the final return:
// an item kind we do not surface produces no line.
func TestFormatCodexStreamEventUnknownItemType(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"todo_list"}}`
	if got := FormatCodexStreamEvent([]byte(line)); got != "" {
		t.Errorf("unknown item type should drop; got %q", got)
	}
}

// TestFormatCodexStreamEventAgentMessageContentField covers the
// content-field fallback for agent_message when text is empty.
func TestFormatCodexStreamEventAgentMessageContentField(t *testing.T) {
	line := `{"type":"item.completed","item":{"type":"agent_message","content":"final answer from content field"}}`
	got := FormatCodexStreamEvent([]byte(line))
	if !strings.Contains(got, "final answer from content field") {
		t.Errorf("content-field agent_message: got %q", got)
	}
}

// TestFormatAgentMessageLinesEmpty covers the len(out)==0 return:
// a body that is only blank lines and the boilerplate header yields no
// preview.
func TestFormatAgentMessageLinesEmpty(t *testing.T) {
	if got := formatAgentMessageLines("# Critic 1 - round 1 attacks\n\n   \n"); got != "" {
		t.Errorf("header-and-blank-only body should yield empty; got %q", got)
	}
}

// TestFormatAgentMessageLinesCap covers the preview-line cap break:
// more meaningful lines than agentMessagePreviewLines are truncated.
func TestFormatAgentMessageLinesCap(t *testing.T) {
	var b strings.Builder
	for i := 0; i < agentMessagePreviewLines+4; i++ {
		b.WriteString("claim line\n")
	}
	got := formatAgentMessageLines(b.String())
	if n := strings.Count(got, "  text: "); n != agentMessagePreviewLines {
		t.Errorf("preview should cap at %d lines; got %d", agentMessagePreviewLines, n)
	}
}
