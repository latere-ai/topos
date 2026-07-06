package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatClaudeStreamEvent turns one line of `claude --output-format
// stream-json` output into a one-line human-readable progress
// message, or returns "" when the event has nothing worth
// surfacing. The format is the same shape across critic and
// proposer drivers because claude emits identical event types in
// both --resume and --print modes.
//
// What we surface:
//
//	tool_use   → "  → <Tool>: <input summary>"
//	thinking   → "  thinking: <first line, 80 chars>"
//	text       → "  text: <first line, 80 chars>"
//
// What we drop: system init, user/tool_result events, the final
// result event (the orchestrator already prints its own
// "{role} done in Xs ..." line).
func FormatClaudeStreamEvent(line []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return ""
	}
	if ev.Type != "assistant" {
		return ""
	}
	var parts []struct {
		Type     string          `json:"type"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
	}
	if json.Unmarshal(ev.Message.Content, &parts) != nil {
		return ""
	}
	for _, p := range parts {
		switch p.Type {
		case "tool_use":
			return fmt.Sprintf("  → %s: %s", p.Name, summarizeToolInput(p.Input))
		case "thinking":
			pv := previewLine(p.Thinking, textPreviewWidth)
			if pv == "" {
				// Claude code occasionally emits a thinking block
				// with empty text (block-start markers, partials
				// without --include-partial-messages). Surfacing
				// these as "  thinking:" with no content is pure
				// noise; drop.
				return ""
			}
			return "  thinking: " + pv
		case "text":
			pv := previewLine(p.Text, textPreviewWidth)
			if pv == "" {
				return ""
			}
			return "  text: " + pv
		}
	}
	return ""
}

// summarizeToolInput pulls the most operator-useful field out of a
// tool_use input blob. Claude tools expose a small set of common
// keys (file_path / path / command / pattern); we surface the first
// one that's a non-empty string. Falls back to a short JSON
// preview when nothing matches.
func summarizeToolInput(input json.RawMessage) string {
	var generic map[string]any
	if json.Unmarshal(input, &generic) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "command", "pattern", "url", "query"} {
		if v, ok := generic[key].(string); ok && v != "" {
			return clip(v, summaryWidth)
		}
	}
	return clip(string(input), summaryWidth)
}

// summaryWidth is the column budget for tool/command/path
// previews. Wide enough that a typical absolute path or shell
// command fits in full.
const summaryWidth = 120

// textPreviewWidth is the column budget for prose-shaped previews:
// claude text/thinking blocks and codex agent_message /
// reasoning items. Wider than summaryWidth because a one-sentence
// preview is rarely useful at 120 chars when the agent is
// reasoning about a multi-clause claim.
const textPreviewWidth = 280

// previewLine returns the first line of s, ellipsized at width.
// Used for thinking/text events where the full body is usually
// multi-paragraph and would dominate the progress stream.
func previewLine(s string, width int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, width)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// FormatCodexStreamEvent is the codex counterpart of
// FormatClaudeStreamEvent. Codex emits item.started when a tool or
// command BEGINS and item.completed when it finishes. We surface
// item.started for tool/command kinds because that is the live
// signal an operator wants ("agent is now running X"), and skip
// item.completed for those same kinds to avoid duplicate lines.
// agent_message item.completed is the final answer the
// orchestrator already records, so we drop it too.
func FormatCodexStreamEvent(line []byte) string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string          `json:"type"`
			Name    string          `json:"name"`
			Command string          `json:"command"`
			Path    string          `json:"path"`
			Text    string          `json:"text"`
			Content string          `json:"content"`
			Summary string          `json:"summary"`
			Input   json.RawMessage `json:"input"`
			Args    json.RawMessage `json:"args"`
		} `json:"item"`
	}
	if err := json.Unmarshal(line, &ev); err != nil {
		return ""
	}
	// Live signal preferred (item.started); fall through to
	// item.completed for shapes that don't have a started variant
	// (some codex versions only emit completed).
	if ev.Type != "item.started" && ev.Type != "item.completed" {
		return ""
	}
	switch ev.Item.Type {
	case "tool_call", "function_call":
		summary := ev.Item.Path
		if summary == "" {
			summary = summarizeToolInput(ev.Item.Input)
		}
		if summary == "" {
			summary = summarizeToolInput(ev.Item.Args)
		}
		return fmt.Sprintf("  → %s: %s", firstNonEmpty(ev.Item.Name, "tool"), summary)
	case "command_execution", "shell_command":
		return "  → shell: " + clip(ev.Item.Command, summaryWidth)
	case "reasoning", "agent_reasoning", "reasoning_summary":
		// Reasoning models (o1/o3 family) emit summary-shaped
		// reasoning events. Surfacing them as "thinking:" lines
		// gives the operator a glimpse of the agent's plan during
		// long calls that would otherwise show no activity.
		text := firstNonEmpty(ev.Item.Summary, ev.Item.Text)
		text = firstNonEmpty(text, ev.Item.Content)
		pv := previewLine(text, textPreviewWidth)
		if pv == "" {
			return ""
		}
		return "  thinking: " + pv
	case "agent_message":
		// agent_message carries the model's prose - for codex
		// critics this is the full markdown attack doc emitted at
		// turn end. previewLine would cut at the first \n and
		// surface only the "# Critic 1 - round 1 attacks" boilerplate
		// header, hiding every claim below it. Walk the doc and
		// emit several meaningful lines so the user sees the
		// actual content the critic produced.
		if ev.Type != "item.completed" {
			return ""
		}
		text := firstNonEmpty(ev.Item.Text, ev.Item.Content)
		return formatAgentMessageLines(text)
	}
	return ""
}

// agentMessagePreviewLines caps how many lines of an agent_message
// body we surface. Five fits a typical critic-round preview (aspect
// line + one attack with claim/expected/repro markers) without
// dumping the entire markdown onto the progress stream.
const agentMessagePreviewLines = 5

// formatAgentMessageLines walks an agent_message body and emits up
// to agentMessagePreviewLines meaningful lines, each prefixed with
// "  text: ". Blank lines are skipped. The standard "# Critic ..."
// header is dropped because it carries no information beyond what
// the orchestrator's own progress line already shows. The result is
// a single string with embedded newlines so the caller's Fprintln
// produces a multi-line block.
func formatAgentMessageLines(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# Critic ") {
			continue
		}
		out = append(out, "  text: "+clip(line, textPreviewWidth))
		if len(out) >= agentMessagePreviewLines {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
