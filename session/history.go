// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/prompts"
)

// HistoryEntry is one event of a thread's log as the history tool reads
// it back (spec 084).
type HistoryEntry struct {
	Seq  uint64
	Time time.Time
	// Calls are the tool_use ids the entry belongs to: a call's own, its
	// result's and its answer's, and those an agent message carries that
	// no agent.tool_use records, such as a call refused before it was
	// decided.
	Calls []string
	// Head says what the event is and who wrote it. Body is what it held,
	// the text a query searches, empty for an event that holds nothing
	// beyond its head.
	Head, Body string
}

// The kinds of a history entry's head, which prompts.HistoryHead names.
const (
	historyMessage   = "message"
	historyAgent     = "agent"
	historyCall      = "call"
	historyResult    = "result"
	historyAnswer    = "answer"
	historyInterrupt = "interrupt"
	historyTitle     = "title"
	historyTask      = "task"
	historyRemoved   = "removed"
)

// historyShown are the event types the history tool reads back; every
// other event is left out, a summary among them.
var historyShown = map[Type]bool{
	TypeUserMessage: true, TypeUserInterrupt: true, TypeUserAnswer: true, TypeUserToolResult: true,
	TypeAgentMessage: true, TypeAgentToolUse: true, TypeToolResult: true, TypeTitleChanged: true,
	TypeSubagentStarted: true, TypeSubagentMessage: true,
}

// History is the thread's view of events below the sequence before, as
// the history tool reads it back (spec 084), in sequence order. It reads
// each event as it was written: a summary and a clearing change nothing
// it returns, and a summary itself is left out. It holds a person's
// message with the paths of its files, the agent's message without its
// reasoning, a call with its input, a result with its outcome and text,
// a picture named by the path it was read from, a person's answer to a
// question, an interrupt, a change of the title, and a spawned thread's
// task and the messages its parent sent it; every other event is left
// out. A redacted event of those is one entry that says it was removed,
// with nothing of what it held.
func History(events []Event, thread string, before uint64) ([]HistoryEntry, error) {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b Event) int { return cmp.Compare(a.Seq, b.Seq) })
	var view []Event
	for _, e := range evs {
		if inView(e, thread) && historyShown[e.Type] {
			view = append(view, e)
		}
	}
	// names are the tools of the view's calls, and recorded the calls an
	// agent.tool_use records, whose entry is that event's: read through the
	// whole view, so the call that reads is recorded though the read stops
	// before its agent.tool_use.
	names, recorded := map[string]string{}, map[string]bool{}
	for _, e := range view {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeAgentToolUse:
			var p AgentToolUse
			if err := e.Decode(&p); err != nil {
				return nil, err
			}
			names[p.ToolUseID], recorded[p.ToolUseID] = p.Name, true
		case TypeAgentMessage:
			var p AgentMessage
			if err := e.Decode(&p); err != nil {
				return nil, err
			}
			for _, b := range p.Message.Blocks {
				if b.Type == ir.BlockToolUse && b.ToolUse != nil {
					names[b.ToolUse.ID] = b.ToolUse.Name
				}
			}
		}
	}
	out := make([]HistoryEntry, 0, len(view))
	for _, e := range view {
		if e.Seq >= before {
			break
		}
		en, ok, err := historyEntry(e, names, recorded)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, en)
		}
	}
	return out, nil
}

// historyEntry is the entry of one event the history tool shows, and
// false for an agent message that holds nothing to show: no text, and
// only calls that their own entries show.
func historyEntry(e Event, names map[string]string, recorded map[string]bool) (HistoryEntry, bool, error) {
	en := HistoryEntry{Seq: e.Seq, Time: e.Time}
	if e.Redacted() {
		en.Head = historyHead(historyRemoved, Sender{}, "", "", "")
		return en, true, nil
	}
	switch e.Type {
	case TypeUserMessage:
		var p UserMessage
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Head, en.Body = historyHead(historyMessage, p.Sender, "", "", ""), textOf(p.Content)
		if len(p.Attachments) > 0 {
			paths := make([]string, len(p.Attachments))
			for i, a := range p.Attachments {
				paths[i] = a.Path
			}
			en.Body = joinLines(en.Body, prompts.Render(prompts.HistoryFiles, prompts.Data{"Paths": strings.Join(paths, ", ")}))
		}
	case TypeSubagentMessage:
		var p SubagentMessage
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Head, en.Body = historyHead(historyMessage, Sender{Name: p.FromName, Subject: p.From}, "", "", ""), textOf(p.Content)
	case TypeSubagentStarted:
		var p SubagentStarted
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Head, en.Body = historyHead(historyTask, Sender{}, "", "", ""), p.Task
	case TypeAgentMessage:
		var p AgentMessage
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		var body []string
		if t := textOf(p.Message.Blocks); t != "" {
			body = append(body, t)
		}
		// A call no agent.tool_use records is shown in the message that
		// made it, unless the message was cut off before it ran, as the
		// fold leaves such a call out.
		for _, b := range p.Message.Blocks {
			if b.Type != ir.BlockToolUse || b.ToolUse == nil || recorded[b.ToolUse.ID] || p.Truncated {
				continue
			}
			en.Calls = append(en.Calls, b.ToolUse.ID)
			body = append(body, joinLines(historyHead(historyCall, Sender{}, b.ToolUse.Name, b.ToolUse.ID, ""), string(b.ToolUse.Args)))
		}
		if len(body) == 0 {
			return en, false, nil
		}
		en.Head, en.Body = historyHead(historyAgent, Sender{}, "", "", ""), strings.Join(body, "\n")
	case TypeAgentToolUse:
		var p AgentToolUse
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Calls = []string{p.ToolUseID}
		en.Head, en.Body = historyHead(historyCall, Sender{}, p.Name, p.ToolUseID, ""), string(p.Input)
	case TypeToolResult:
		var p ToolResult
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Calls = []string{p.ToolUseID}
		en.Head = historyHead(historyResult, Sender{}, names[p.ToolUseID], p.ToolUseID, resultOutcome(p.Outcome, p.IsError))
		en.Body = resultText(p.Content, pictureFrom(p.Meta))
	case TypeUserToolResult:
		var p UserToolResult
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Calls = []string{p.ToolUseID}
		en.Head = historyHead(historyResult, Sender{}, names[p.ToolUseID], p.ToolUseID, resultOutcome("", p.IsError))
		en.Body = resultText(p.Content, "")
	case TypeUserAnswer:
		var p UserAnswer
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		answers := make([]prompts.Data, len(p.Answers))
		for i, a := range p.Answers {
			answers[i] = prompts.Data{"Number": i + 1, "Chosen": strings.Join(a.Selected, ", "), "Text": a.Text, "Left": a.Empty()}
		}
		en.Calls = []string{p.ToolUseID}
		en.Head = historyHead(historyAnswer, p.Sender, "", p.ToolUseID, "")
		en.Body = strings.TrimRight(prompts.Render(prompts.HistoryAnswers, prompts.Data{"Answers": answers}), "\n")
	case TypeUserInterrupt:
		var p UserInterrupt
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Head = historyHead(historyInterrupt, p.Sender, "", "", "")
	case TypeTitleChanged:
		var p TitleChanged
		if err := e.Decode(&p); err != nil {
			return en, false, err
		}
		en.Head, en.Body = historyHead(historyTitle, p.By, "", "", ""), p.New
	}
	return en, true, nil
}

// historyHead renders an entry's head of kind, with the values the kind
// names.
func historyHead(kind string, s Sender, tool, id, outcome string) string {
	return prompts.Render(prompts.HistoryHead, prompts.Data{"Kind": kind, "Name": s.Name, "Subject": s.Subject, "Tool": tool, "ID": id, "Outcome": outcome})
}

// textOf is the text of blocks' text blocks, one after another;
// reasoning, pictures and calls are left out.
func textOf(blocks []lux.Block) string {
	var out []string
	for _, b := range blocks {
		if b.Type == ir.BlockText && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return strings.Join(out, "\n")
}

// resultText is a result's text blocks with each picture named by path,
// the file it was read from, empty when the result names none.
func resultText(blocks []lux.Block, path string) string {
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case ir.BlockText:
			out = append(out, b.Text)
		case ir.BlockImage:
			out = append(out, prompts.Render(prompts.HistoryPicture, prompts.Data{"Path": path}))
		}
	}
	return strings.Join(out, "\n")
}

// pictureFrom is the path a result's meta names, the file a file tool
// read; empty for a meta that names none or that does not decode, whose
// picture is then shown without a path.
func pictureFrom(meta json.RawMessage) string {
	var m struct {
		Path string `json:"path"`
	}
	if len(meta) == 0 || json.Unmarshal(meta, &m) != nil {
		return ""
	}
	return m.Path
}

// resultOutcome is a result's outcome, or the one its error flag
// implies for a result that records none, as a client's does.
func resultOutcome(recorded string, isError bool) string {
	switch {
	case recorded != "":
		return recorded
	case isError:
		return "error"
	}
	return "ok"
}

// joinLines joins the parts that are not empty, one line after another.
func joinLines(parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "\n")
}
