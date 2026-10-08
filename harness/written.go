// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// textNext is what a step does after a response that wrote a tool call as
// text (spec 062).
type textNext int

const (
	// textRemind sends the step's request again with the reminder.
	textRemind textNext = iota
	// textMoved sends the step's request, built again, on the model the
	// turn moved to.
	textMoved
	// textAnswer keeps the response as the step's answer.
	textAnswer
)

// The actions a line of Config.Log names (spec 062).
const (
	actionReminded = "reminded"
	actionMoved    = "moved"
	actionKept     = "kept"
)

// writtenAsText decides what a step does about a response that wrote a
// call of the offered tool into its text instead of calling it (spec
// 062), reminder being what the step's request carried, nil for none.
//
//   - A request that carried no reminder: the response is recorded as a
//     model.request with OutcomeToolAsText and no agent.message, so its
//     cost is spent and the fold never sees it, the Observer discards the
//     step's output, and the request is sent again with the reminder.
//   - A reminded request on the session's own thread that may move, and
//     has not moved for this in the turn: Config.Failover is asked with
//     FailedToolAsText, standing on the model the turn runs. A move
//     records the response with OutcomeToolAsText and the change with
//     session.ReasonToolAsText in one batch, and the step's request is
//     built again for the model named.
//   - Otherwise, a router that names no other model among them: the
//     response is the step's answer, as it was before this rule.
//
// Each decision is one line of Config.Log; none holds a word of the
// response. Any error is a failure to record, which stops the turn.
func (t *turn) writtenAsText(ctx context.Context, res models.Result, si sendInfo, tool string, reminder *session.Reminder) (textNext, error) {
	model := t.h.c.Connection.Model
	if reminder == nil {
		mr, err := t.modelRequest(ctx, res, si, session.OutcomeToolAsText)
		if err != nil {
			return 0, err
		}
		if err := t.commit(ctx, mr); err != nil {
			return 0, err
		}
		if o := t.h.c.Observer; o != nil {
			o.OnReset(t.thread, t.num, t.step)
		}
		t.logText(ctx, slog.LevelInfo, model, tool, actionReminded)
		return textRemind, nil
	}
	if why := t.unmovable(); why != "" {
		t.logText(ctx, slog.LevelWarn, model, tool, actionKept, slog.String("why", why))
		return textAnswer, nil
	}
	detail := fmt.Sprintf("a call of %s was written as text, again after a reminder", tool)
	err := t.move(ctx, FailedToolAsText, detail, session.ReasonToolAsText, detail, func() (session.Event, error) {
		return t.modelRequest(ctx, res, si, session.OutcomeToolAsText)
	})
	var stay *stayed
	switch {
	case err == nil:
		t.movedOffText = true
		t.logText(ctx, slog.LevelInfo, model, tool, actionMoved, slog.String("to", t.model.Name))
		return textMoved, nil
	case errors.As(err, &stay):
		t.logText(ctx, slog.LevelWarn, model, tool, actionKept, slog.String("why", stay.why))
		return textAnswer, nil
	}
	return 0, err
}

// unmovable is why the turn cannot move off a model that wrote a tool call
// as text, "" when it can: on a thread, on a model named itself, with no
// router, after one such move in the turn, or out of moves.
func (t *turn) unmovable() string {
	switch {
	case t.thread != "":
		return "a thread's turn does not move"
	case t.model.Via == "" || t.model.Name == "":
		return "the session runs a model named itself"
	case t.h.c.Failover == nil:
		return "no router is asked"
	case t.movedOffText:
		return "the turn moved off a model for this once already"
	case !t.switchable():
		return "the turn ran out of moves"
	}
	return ""
}

// logText writes one line of Config.Log about a response that wrote a call
// of tool as text on model, and the action the turn took.
func (t *turn) logText(ctx context.Context, level slog.Level, model, tool, action string, attrs ...slog.Attr) {
	t.h.c.Log.LogAttrs(ctx, level, "a response wrote a tool call as text", append([]slog.Attr{
		slog.String("session", t.s.ID), slog.String("thread", t.thread), slog.Int("turn", t.num), slog.Int("step", t.step),
		slog.String("model", model), slog.String("tool", tool), slog.String("action", action),
	}, attrs...)...)
}

// offered are the names of the tools req offers.
func offered(req *ir.Request) []string {
	names := make([]string, len(req.Tools))
	for i, tool := range req.Tools {
		names[i] = tool.Name
	}
	return names
}

// remind is req with the text of r after its fold (spec 062): a text
// block added to its last message when that is the user's, else a user
// message of its own, as a compaction's summary request adds its ask. The
// messages before it are unchanged, so the prefix a provider cached for
// the request without it still serves this one.
func remind(req ir.Request, r session.Reminder) (ir.Request, error) {
	text, err := reminderText(r)
	if err != nil {
		return ir.Request{}, err
	}
	block := ir.Block{Type: ir.BlockText, Text: text}
	msgs := slices.Clone(req.Messages)
	if n := len(msgs); n > 0 && msgs[n-1].Role == ir.RoleUser {
		msgs[n-1].Blocks = append(slices.Clip(msgs[n-1].Blocks), block)
	} else {
		msgs = append(msgs, ir.Message{Role: ir.RoleUser, Blocks: []ir.Block{block}})
	}
	req.Messages = msgs
	return req, nil
}

// reminderText renders the reminder r names. A prompt this build does not
// hold, or one that names no tool, cannot be rendered, and a replay skips
// the request that carried it.
func reminderText(r session.Reminder) (string, error) {
	if prompts.Name(r.Prompt) != prompts.ReminderToolAsText || r.Tool == "" {
		return "", fmt.Errorf("%w: the request carried the reminder %q of the tool %q, which this build does not render", models.ErrNotRebuilt, r.Prompt, r.Tool)
	}
	return prompts.Render(prompts.ReminderToolAsText, prompts.Data{"Tool": r.Tool}), nil
}

// callWrappers are the elements a model wraps a call in when it writes the
// call into its answer in another model's call format (spec 062): the
// Hermes and Qwen tool_call, an Anthropic-style invoke, and the
// function_call and tool_use some fine-tunes print. Each counts only when
// it names an offered tool.
var callWrappers = []string{"tool_call", "tool_use", "function_call", "invoke"}

// writtenCall is the offered tool a response wrote a call of into its
// text instead of calling it (spec 062), "" for none. Only a response that
// ended on its own, end_turn or stop_sequence, and that calls no tool is
// read, and only its text blocks, with every fenced code block and inline
// code span taken out first. Its text writes a call of an offered tool
// when it holds, outside code, either
//
//   - an element named after the tool, opened as <name> or <name attrs>
//     and closed later by </name>, or written self-closing as <name/>;
//   - a call wrapper (callWrappers) whose content, up to its closing tag
//     or the end of the text, names the tool as name="tool" or
//     "name": "tool", or Llama's <function=tool>.
//
// A tool named in prose, an element opened and never closed, and markup
// named after no offered tool, such as <plan> where no tool is named plan,
// write no call. When several calls are written, the first is named.
func writtenCall(res models.Result, offered []string) string {
	if len(offered) == 0 || (res.StopReason != ir.StopEndTurn && res.StopReason != ir.StopStopSequence) {
		return ""
	}
	var b strings.Builder
	for _, blk := range res.Message.Blocks {
		switch blk.Type {
		case ir.BlockToolUse:
			return ""
		case ir.BlockText:
			b.WriteString(blk.Text)
			b.WriteByte('\n')
		}
	}
	return markupCall(withoutCode(b.String()), offered)
}

// markupCall is the first offered tool text writes a call of, by the two
// shapes writtenCall names, "" for none.
func markupCall(text string, offered []string) string {
	if !strings.Contains(text, "<") {
		return ""
	}
	at, name := len(text), ""
	for _, tool := range offered {
		if i := element(text, tool); i >= 0 && i < at {
			at, name = i, tool
		}
		if i := wrapped(text, tool); i >= 0 && i < at {
			at, name = i, tool
		}
	}
	return name
}

// element is where text opens an element named tag that it closes later,
// or writes self-closing, -1 for none.
func element(text, tag string) int {
	open := regexp.MustCompile(`<` + regexp.QuoteMeta(tag) + `(?:\s[^<>]*?)?(/?)>`)
	closing := regexp.MustCompile(`</` + regexp.QuoteMeta(tag) + `\s*>`)
	for _, m := range open.FindAllStringSubmatchIndex(text, -1) {
		if m[3] > m[2] || closing.MatchString(text[m[1]:]) {
			return m[0]
		}
	}
	return -1
}

// wrapped is where text opens a call wrapper that names tool, -1 for
// none: one of callWrappers whose content, to its closing tag or the end
// of the text, names it as an attribute or a JSON member, or Llama's
// <function=tool>.
func wrapped(text, tool string) int {
	q := regexp.QuoteMeta(tool)
	if loc := regexp.MustCompile(`<function=` + q + `\s*>`).FindStringIndex(text); loc != nil {
		return loc[0]
	}
	names := regexp.MustCompile(`name\s*=\s*["']` + q + `["']|"name"\s*:\s*"` + q + `"`)
	first := -1
	for _, w := range callWrappers {
		open := regexp.MustCompile(`<` + w + `(?:\s[^<>]*)?>`)
		closing := "</" + w + ">"
		for _, m := range open.FindAllStringIndex(text, -1) {
			end := len(text)
			if i := strings.Index(text[m[1]:], closing); i >= 0 {
				end = m[1] + i
			}
			// The open tag's attributes and the content both name a call: an
			// invoke names its tool as an attribute, a tool_call in its JSON.
			if names.MatchString(text[m[0]:end]) && (first < 0 || m[0] < first) {
				first = m[0]
			}
		}
	}
	return first
}

// withoutCode is text with every fenced code block and inline code span
// taken out, as Markdown reads them: a fence is a line that opens with
// three or more backticks or tildes, after any indentation, closed by a
// line of the same character at least as long, or by the end of the text;
// a span is a run of backticks closed by the next run of the same length
// in its paragraph. Each is replaced by a line break or a space, so the
// text on either side does not join. Any indentation, where Markdown
// allows three spaces, reads a fence nested in a list as one.
func withoutCode(text string) string {
	var prose strings.Builder
	fence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		lead := strings.TrimLeft(line, " \t")
		if fence != "" {
			if run := fenceRun(lead); run != "" && run[0] == fence[0] && len(run) >= len(fence) && strings.TrimSpace(lead[len(run):]) == "" {
				fence = ""
			}
			prose.WriteByte('\n')
			continue
		}
		if run := fenceRun(lead); run != "" {
			fence = run
			prose.WriteByte('\n')
			continue
		}
		prose.WriteString(line)
	}
	return withoutSpans(prose.String())
}

// fenceRun is the run of three or more backticks or tildes a line opens
// a fence with, "" for none. A backtick run followed by another backtick
// on its line opens no fence: Markdown reads it as a code span.
func fenceRun(line string) string {
	if line == "" || (line[0] != '`' && line[0] != '~') {
		return ""
	}
	n := len(line) - len(strings.TrimLeft(line, line[:1]))
	if n < 3 || (line[0] == '`' && strings.Contains(line[n:], "`")) {
		return ""
	}
	return line[:n]
}

// paragraphBreak is a blank line, where a code span ends unclosed.
var paragraphBreak = regexp.MustCompile(`\n[ \t]*\n`)

// withoutSpans is text with each inline code span replaced by a space: a
// run of backticks opens a span that the next run of the same length in
// its paragraph closes; a run that nothing closes is kept as it is.
func withoutSpans(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '`' {
			out.WriteByte(text[i])
			i++
			continue
		}
		n := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
		run := text[i : i+n]
		rest := text[i+n:]
		if loc := paragraphBreak.FindStringIndex(rest); loc != nil {
			rest = rest[:loc[0]]
		}
		end := closingRun(rest, n)
		if end < 0 {
			out.WriteString(run)
			i += n
			continue
		}
		out.WriteByte(' ')
		i += n + end + n
	}
	return out.String()
}

// closingRun is where in text a run of exactly n backticks starts, -1 for
// none.
func closingRun(text string, n int) int {
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		m := len(text[i:]) - len(strings.TrimLeft(text[i:], "`"))
		if m == n {
			return i
		}
		i += m
	}
	return -1
}
