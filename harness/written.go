// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"regexp"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
)

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
