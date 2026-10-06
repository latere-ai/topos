// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"fmt"
	"slices"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/prompts"
)

// Kinds of a system Part.
const (
	PartContext      = "context"
	PartInstructions = "instructions"
	PartSkills       = "skills"
	PartMemory       = "memory"
)

// Part is one system part of a transcript. The fold is pure, so an
// instruction part names its blob and the harness reads and renders it.
type Part struct {
	Kind         string          `json:"kind"`
	Context      string          `json:"context,omitempty"`
	Instructions *Instructions   `json:"instructions,omitempty"`
	Skills       []Skill         `json:"skills,omitempty"`
	Memory       *MemoryAttached `json:"memory,omitempty"`
}

// Transcript is the fold of a thread: the system parts, the Lux wire
// messages, the tool_use ids of the last step with no result, and the
// event types this build does not know.
type Transcript struct {
	System   []Part        `json:"system"`
	Messages []lux.Message `json:"messages"`
	Open     []string      `json:"open"`
	Unknown  []string      `json:"unknown"`
}

// Check returns ErrSchemaTooNew when the transcript met event types this
// build does not know. A runner calls it before it continues a session.
func (t Transcript) Check() error {
	if len(t.Unknown) > 0 {
		return fmt.Errorf("%w: unknown event types %v", ErrSchemaTooNew, t.Unknown)
	}
	return nil
}

// summaryRange is the effective range of one summary compaction.
type summaryRange struct {
	from, to uint64
	summary  string
	emitted  bool
}

// item is one rendered message, or the slot of a step's tool results.
type item struct {
	role    ir.Role
	blocks  []lux.Block
	results []string // tool_use ids, for a results slot
}

// Fold renders the transcript of one thread from a session's events
// (spec 004). thread is "" for the session's own thread. The fold is
// pure: the same events give byte-identical output.
func Fold(events []Event, thread string) (Transcript, error) {
	return fold(events, thread, false)
}

// FoldOmittingRedacted renders the transcript with every redacted event
// left out instead of refused. It exists for the one request that
// summarizes a range holding a redaction (spec 010), so the summary is
// written from what remains and the removed value never reaches a model.
func FoldOmittingRedacted(events []Event, thread string) (Transcript, error) {
	return fold(events, thread, true)
}

func fold(events []Event, thread string, omitRedacted bool) (Transcript, error) {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b Event) int { return cmp.Compare(a.Seq, b.Seq) })

	t := Transcript{System: []Part{}, Messages: []lux.Message{}, Open: []string{}, Unknown: []string{}}
	unknown := map[Type]bool{}
	for _, e := range evs {
		if !Known[e.Type] && !unknown[e.Type] {
			unknown[e.Type] = true
			t.Unknown = append(t.Unknown, string(e.Type))
		}
	}

	view := make([]Event, 0, len(evs))
	for _, e := range evs {
		if inView(e, thread) {
			view = append(view, e)
		}
	}

	var err error
	if t.System, err = systemParts(view); err != nil {
		return Transcript{}, err
	}
	ranges, cleared, err := compactions(view)
	if err != nil {
		return Transcript{}, err
	}

	approved := map[string]bool{}
	for _, e := range view {
		if e.Type == TypeAgentToolUse && !e.Redacted() {
			var p AgentToolUse
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			approved[p.ToolUseID] = true
		}
	}

	// An approval's answer names its host from its request, and an allow
	// reads as reachable once the runner widened the network for it, which
	// it records before the answer (spec 052).
	approvalHosts, widened := map[string]string{}, map[string]bool{}
	for _, e := range evs {
		if e.Type != TypeNetworkChanged || e.Redacted() {
			continue
		}
		var p NetworkChanged
		if err := e.Decode(&p); err != nil {
			return Transcript{}, err
		}
		if p.ApprovalID != "" {
			widened[p.ApprovalID] = true
		}
	}

	var items []item
	results := map[string]lux.Block{}
	lastStep := -1
	subjects := map[string]bool{}
	multi := false
	noteSender := func(s Sender) {
		if s.Subject != "" && !subjects[s.Subject] {
			subjects[s.Subject] = true
			multi = len(subjects) > 1
		}
	}
	user := func(blocks ...lux.Block) { items = append(items, item{role: ir.RoleUser, blocks: blocks}) }

	for _, e := range view {
		emitBefore(ranges, e.Seq, user)
		if r := covering(ranges, e.Seq); r != nil {
			if !r.emitted {
				r.emitted = true
				user(summary(r.summary))
			}
			continue
		}
		if e.Redacted() {
			if visible(e) && !omitRedacted {
				return Transcript{}, fmt.Errorf("%w: event %s (seq %d)", ErrRedactionUncompacted, e.ID, e.Seq)
			}
			continue
		}
		switch e.Type {
		case TypeUserMessage:
			var p UserMessage
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
			blocks := slices.Clone(p.Content)
			if multi {
				blocks = append([]lux.Block{text(prompts.Render(prompts.TranscriptSender, sender(p.Sender)))}, blocks...)
			}
			// The files a message carries are named to the model by the
			// paths the runner writes them at (spec 015).
			if len(p.Attachments) > 0 {
				blocks = append(blocks, text(prompts.Render(prompts.TranscriptAttachments, prompts.Data{"Attachments": p.Attachments})))
			}
			user(blocks...)
		case TypeUserInterrupt:
			var p UserInterrupt
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
			user(text(prompts.Render(prompts.TranscriptInterrupt, sender(p.Sender))))
		case TypeUserToolConfirmation:
			var p UserToolConfirmation
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
		case TypeUserAnswer:
			// An answer renders nothing: the model reads it as the result
			// of the question's call, which the runner renders from it
			// (spec 039). Its sender is noted as a confirmation's is, so a
			// later message of a second person is led by their name.
			var p UserAnswer
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
		case TypeAgentMessage:
			var p AgentMessage
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			blocks := make([]lux.Block, 0, len(p.Message.Blocks))
			var uses []string
			for _, b := range p.Message.Blocks {
				if b.Type == ir.BlockToolUse {
					if b.ToolUse == nil || (p.Truncated && !approved[b.ToolUse.ID]) {
						continue
					}
					uses = append(uses, b.ToolUse.ID)
				}
				blocks = append(blocks, b)
			}
			if len(blocks) > 0 {
				items = append(items, item{role: ir.RoleAssistant, blocks: blocks})
				if len(uses) > 0 {
					items = append(items, item{role: ir.RoleUser, results: uses})
				}
				lastStep = len(items) - 1
				if len(uses) == 0 {
					lastStep = -1
				}
			}
			if p.Truncated {
				user(text(prompts.Text(prompts.TranscriptTruncated)))
			}
		case TypeToolResult:
			var p ToolResult
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			results[p.ToolUseID] = resultBlock(p.ToolUseID, p.Content, p.IsError, cleared)
		case TypeUserToolResult:
			var p UserToolResult
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
			results[p.ToolUseID] = resultBlock(p.ToolUseID, p.Content, p.IsError, cleared)
		case TypeThreadStarted:
			var p ThreadStarted
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			user(text(p.Task))
		case TypeThreadMessage:
			var p ThreadMessage
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			from := prompts.Render(prompts.TranscriptThreadMessage, prompts.Data{"FromName": p.FromName, "From": p.From})
			user(append([]lux.Block{text(from)}, p.Content...)...)
		case TypeSessionRewound:
			var p SessionRewound
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			user(text(prompts.Render(prompts.TranscriptRewound, prompts.Data{"Turn": p.ToTurn})))
		case TypeApprovalRequested:
			// A refused connection reads after the result of the call that
			// made it, which its event follows, in the same user message.
			var p ApprovalRequested
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			approvalHosts[p.ApprovalID] = p.Destination.Host
			user(text(prompts.Render(prompts.TranscriptApprovalRequested, prompts.Data{
				"Host": p.Destination.Host, "Port": p.Destination.Port, "More": p.More, "Ask": p.Verdict == "ask",
			})))
		case TypeApprovalDecided:
			var p ApprovalDecided
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			data := prompts.Data{"Host": approvalHosts[p.ApprovalID], "Note": p.Note}
			switch {
			case p.Decision != DecisionAllow:
				user(text(prompts.Render(prompts.TranscriptApprovalDenied, data)))
			case widened[p.ApprovalID]:
				user(text(prompts.Render(prompts.TranscriptApprovalAllowed, data)))
			default:
				user(text(prompts.Render(prompts.TranscriptApprovalUnavailable, data)))
			}
		}
	}
	for i := range ranges {
		if !ranges[i].emitted {
			ranges[i].emitted = true
			user(summary(ranges[i].summary))
		}
	}

	for i, it := range items {
		if it.results == nil {
			continue
		}
		var blocks []lux.Block
		for _, id := range it.results {
			if b, ok := results[id]; ok {
				blocks = append(blocks, b)
			} else if i == lastStep {
				t.Open = append(t.Open, id)
			}
		}
		items[i].blocks = blocks
	}

	for _, it := range items {
		if len(it.blocks) == 0 {
			continue
		}
		if n := len(t.Messages); n > 0 && t.Messages[n-1].Role == it.role {
			t.Messages[n-1].Blocks = append(t.Messages[n-1].Blocks, it.blocks...)
			continue
		}
		t.Messages = append(t.Messages, lux.Message{Role: it.role, Blocks: slices.Clone(it.blocks)})
	}
	return t, nil
}

// inView reports whether e belongs to the fold of thread: the events
// whose thread is it, and the session-wide machine, memory and (for the
// session's thread) rewind events. An event's thread is the thread whose
// transcript it renders into: a thread.started carries its own id, a
// thread.message the receiving thread's.
func inView(e Event, thread string) bool {
	switch e.Type {
	case TypeSessionMachine, TypeMemoryAttached:
		return true
	case TypeSessionRewound:
		return thread == ""
	}
	return e.Thread == thread
}

// Uncompacted returns the redacted events of a thread's view that would
// render and that no summary covers, in sequence order: the events a
// redaction compaction must cover before the thread's next request.
func Uncompacted(events []Event, thread string) ([]Event, error) {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b Event) int { return cmp.Compare(a.Seq, b.Seq) })
	view := make([]Event, 0, len(evs))
	for _, e := range evs {
		if inView(e, thread) {
			view = append(view, e)
		}
	}
	ranges, _, err := compactions(view)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range view {
		if e.Redacted() && visible(e) && covering(ranges, e.Seq) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// visible reports whether a redacted event of the view would have
// rendered into the messages.
func visible(e Event) bool {
	switch e.Type {
	case TypeUserMessage, TypeUserInterrupt, TypeAgentMessage, TypeToolResult,
		TypeUserToolResult, TypeThreadStarted, TypeThreadMessage, TypeContextCompacted,
		TypeSessionRewound:
		return true
	}
	return false
}

// systemParts renders the latest session.machine's context, instructions
// and skills, then one memory part per attached store in first-attachment
// order. Redacted machine and memory events are skipped.
func systemParts(view []Event) ([]Part, error) {
	var machine *SessionMachine
	var order []string
	stores := map[string]MemoryAttached{}
	for _, e := range view {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeSessionMachine:
			var p SessionMachine
			if err := e.Decode(&p); err != nil {
				return nil, err
			}
			machine = &p
		case TypeMemoryAttached:
			var p MemoryAttached
			if err := e.Decode(&p); err != nil {
				return nil, err
			}
			if _, ok := stores[p.MemoryStoreID]; !ok {
				order = append(order, p.MemoryStoreID)
			}
			stores[p.MemoryStoreID] = p
		}
	}
	parts := []Part{}
	if machine != nil {
		if machine.Context != "" {
			parts = append(parts, Part{Kind: PartContext, Context: machine.Context})
		}
		for _, in := range machine.Instructions {
			parts = append(parts, Part{Kind: PartInstructions, Instructions: &in})
		}
		if len(machine.Skills) > 0 {
			parts = append(parts, Part{Kind: PartSkills, Skills: slices.Clone(machine.Skills)})
		}
	}
	for _, id := range order {
		m := stores[id]
		parts = append(parts, Part{Kind: PartMemory, Memory: &m})
	}
	return parts, nil
}

// compactions returns the effective summary ranges in order of their
// first sequence, and the tool_use ids whose results were cleared. A
// summary that overlaps earlier ones supersedes them over the union of
// their ranges.
func compactions(view []Event) ([]summaryRange, map[string]bool, error) {
	var ranges []summaryRange
	cleared := map[string]bool{}
	for _, e := range view {
		if e.Type != TypeContextCompacted || e.Redacted() {
			continue
		}
		var p ContextCompacted
		if err := e.Decode(&p); err != nil {
			return nil, nil, err
		}
		switch p.Kind {
		case CompactClearToolResults:
			for _, id := range p.ToolUseIDs {
				cleared[id] = true
			}
		case CompactSummary:
			if p.FromSeq == 0 || p.ToSeq < p.FromSeq || p.ToSeq >= e.Seq {
				return nil, nil, fmt.Errorf("%w: compaction %s covers %d..%d at seq %d", ErrCorrupt, e.ID, p.FromSeq, p.ToSeq, e.Seq)
			}
			r := summaryRange{from: p.FromSeq, to: p.ToSeq, summary: p.Summary}
			kept := ranges[:0]
			for _, o := range ranges {
				if o.to < r.from || o.from > r.to {
					kept = append(kept, o)
					continue
				}
				r.from, r.to = min(r.from, o.from), max(r.to, o.to)
			}
			ranges = append(kept, r)
		}
	}
	slices.SortFunc(ranges, func(a, b summaryRange) int { return cmp.Compare(a.from, b.from) })
	return ranges, cleared, nil
}

// covering returns the summary range seq falls in.
func covering(ranges []summaryRange, seq uint64) *summaryRange {
	for i := range ranges {
		if seq >= ranges[i].from && seq <= ranges[i].to {
			return &ranges[i]
		}
	}
	return nil
}

// emitBefore renders every summary whose range ends before seq and has
// not rendered, because no event of the view fell inside it.
func emitBefore(ranges []summaryRange, seq uint64, user func(...lux.Block)) {
	for i := range ranges {
		if !ranges[i].emitted && ranges[i].to < seq {
			ranges[i].emitted = true
			user(summary(ranges[i].summary))
		}
	}
}

func resultBlock(id string, content []lux.Block, isError bool, cleared map[string]bool) lux.Block {
	blocks := slices.Clone(content)
	if cleared[id] {
		blocks = []lux.Block{text(prompts.Text(prompts.TranscriptCleared))}
	}
	return lux.Block{Type: ir.BlockToolResult, ToolResult: &lux.ToolResult{ToolUseID: id, Blocks: blocks, IsError: isError}}
}

func text(s string) lux.Block { return lux.Block{Type: ir.BlockText, Text: s} }

// summary is the user text a summary compaction renders as.
func summary(s string) lux.Block {
	return text(prompts.Render(prompts.TranscriptSummary, prompts.Data{"Summary": s}))
}

// sender is the values a text naming a sender renders with; the text
// falls back from the name to the subject to a word of its own.
func sender(s Sender) prompts.Data {
	return prompts.Data{"Name": s.Name, "Subject": s.Subject}
}
