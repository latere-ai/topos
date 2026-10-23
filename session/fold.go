// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

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
	// Attached are the blocks of Messages that name the files persons
	// brought and carry the texts of the ones read inline (spec 079), and
	// Summaries the blocks that stand for compacted history (spec 010):
	// the parts of the messages the context report counts apart (spec
	// 087).
	Attached  []lux.Block `json:"-"`
	Summaries []lux.Block `json:"-"`
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
	return FoldWith(events, thread, FoldOptions{})
}

// FoldOmittingRedacted renders the transcript with every redacted event
// left out instead of refused. It exists for the one request that
// summarizes a range holding a redaction (spec 010), so the summary is
// written from what remains and the removed value never reaches a model.
func FoldOmittingRedacted(events []Event, thread string) (Transcript, error) {
	return FoldWith(events, thread, FoldOptions{OmitRedacted: true})
}

// FoldOptions are what a fold renders beside the events it is given.
type FoldOptions struct {
	// OmitRedacted leaves every redacted event out instead of refusing
	// it, as FoldOmittingRedacted does.
	OmitRedacted bool
	// Texts are the texts of the files persons' messages mark Inline, by
	// their digest, which the caller read where each file is kept (spec
	// 079): a marked file whose text is here is rendered after the line
	// that names it, and one whose text is not stays named by its path
	// alone. The fold stays pure: the same events and texts give
	// byte-identical output.
	Texts map[Digest]string
	// Device is set for a session on a person's own computer, where a
	// message's files are kept outside the working directory (spec 080):
	// the line that names them says where a command reaches them.
	Device bool
	// History is set for a thread that holds the history tool (spec
	// 084): a cleared result names its call's id and a summary the events
	// it covers, which the tool reads back.
	History bool
	// Notify is set for a thread that holds the notify tool (spec 086): a
	// message that began a turn nobody waits for, a wake's or a judged
	// trigger's, is led by what such a turn is and when to notify, and
	// Attended says whether a question waits for the person.
	Notify, Attended bool
}

// FoldWith renders the transcript of one thread as Fold does, with o.
func FoldWith(events []Event, thread string, o FoldOptions) (Transcript, error) {
	return fold(events, thread, o)
}

// InlineFiles are the files marked Inline of the persons' messages the
// fold of thread renders (spec 079), in order: of a message in the
// thread's view that is not redacted and that no summary covers. They
// are the files whose texts FoldOptions.Texts holds.
func InlineFiles(events []Event, thread string) ([]Attachment, error) {
	var view []Event
	for _, e := range events {
		if inView(e, thread) {
			view = append(view, e)
		}
	}
	ranges, _, err := compactions(view)
	if err != nil {
		return nil, err
	}
	var out []Attachment
	for _, e := range view {
		if e.Type != TypeUserMessage || e.Redacted() || covering(ranges, e.Seq) != nil {
			continue
		}
		var p UserMessage
		if err := e.Decode(&p); err != nil {
			return nil, err
		}
		for _, a := range p.Attachments {
			if a.Inline {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

func fold(events []Event, thread string, o FoldOptions) (Transcript, error) {
	omitRedacted := o.OmitRedacted
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
		emitBefore(ranges, e.Seq, user, o.History)
		if r := covering(ranges, e.Seq); r != nil {
			if !r.emitted {
				r.emitted = true
				user(summary(*r, o.History))
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
			// A wake's message is the agent's own note, led by when it set
			// the wake and when it was due, and a message that began a turn
			// nobody waits for by what such a turn is (spec 086).
			if w := p.Wake; w != nil {
				blocks = append([]lux.Block{text(wakeLine(*w, e.Time))}, blocks...)
			}
			if p.FiringID != "" && p.Judged() && o.Notify {
				woken := prompts.Render(prompts.ContextWoken, prompts.Data{"Wake": p.Wake != nil, "Attended": o.Attended})
				blocks = append([]lux.Block{text(woken)}, blocks...)
			}
			if multi {
				blocks = append([]lux.Block{text(prompts.Render(prompts.TranscriptSender, sender(p.Sender)))}, blocks...)
			}
			// The files a message carries are named to the model by the
			// paths the runner writes them at (spec 015), and a short text
			// file's text follows the line that names it (spec 079).
			if len(p.Attachments) > 0 {
				named := prompts.Render(prompts.TranscriptAttachments, prompts.Data{"Attachments": p.Attachments})
				if o.Device {
					named = prompts.Render(prompts.TranscriptAttachmentsDevice, prompts.Data{"Attachments": p.Attachments})
				}
				blocks = append(blocks, text(named))
				t.Attached = append(t.Attached, text(named))
				for _, a := range p.Attachments {
					if body, ok := o.Texts[a.Blob]; ok && a.Inline {
						body = strings.TrimRight(body, "\r\n")
						inline := text(prompts.Render(prompts.TranscriptAttachmentText, prompts.Data{"Path": a.Path, "Fence": fence(body), "Text": body}))
						blocks = append(blocks, inline)
						t.Attached = append(t.Attached, inline)
					}
				}
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
			results[p.ToolUseID] = resultBlock(p.ToolUseID, p.Content, p.IsError, cleared, o.History)
		case TypeUserToolResult:
			var p UserToolResult
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			noteSender(p.Sender)
			results[p.ToolUseID] = resultBlock(p.ToolUseID, p.Content, p.IsError, cleared, o.History)
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
		case TypeSessionMachine:
			// A machine that stopped and came back, or that was replaced
			// by an empty one, reads where it happened, in the session's
			// own thread: its agent is the one that says so to the person,
			// and a thread's command that stopped with the machine says it
			// in its own result. Any other record renders nothing here;
			// its context is a system part.
			if thread != "" {
				break
			}
			var p SessionMachine
			if err := e.Decode(&p); err != nil {
				return Transcript{}, err
			}
			switch p.Reason {
			case MachineRestarted:
				user(text(prompts.Render(prompts.TranscriptMachineRestarted, prompts.Data{"OutOfMemory": p.Cause == MachineOutOfMemory})))
			case MachineReplaced:
				user(text(prompts.Text(prompts.TranscriptMachineReplaced)))
			case MachineRestored:
				// A working directory rebuilt from the log is said with
				// what is back and what is not (spec 065); one a
				// checkpoint restored holds all its files and reads as
				// nothing.
				// One restored from the copy the session keeps in its folder
				// is said with what the copy gave, what the log added and
				// what the copy does not keep (spec 073).
				switch r := p.Restore; {
				case r != nil && r.Source == RestoreFromLog:
					user(text(prompts.Render(prompts.TranscriptMachineRebuilt, rebuiltData(*r))))
				case r != nil && r.Source == RestoreFromFolder && r.Copy != nil:
					user(text(prompts.Render(prompts.TranscriptMachineWorkRestored, workRestoredData(*r))))
				}
			}
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
			user(summary(ranges[i], o.History))
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
	for _, r := range ranges {
		if r.emitted {
			t.Summaries = append(t.Summaries, summary(r, o.History))
		}
	}
	return t, nil
}

// WakeLateAfter is how far past its due time a wake's message must come
// for the model to read that it came late: twice the minute the trigger
// loop runs every, so a wake the loop took on its next pass is on time.
const WakeLateAfter = 2 * time.Minute

// wakeLine is the line that leads a wake's message, sent at fired.
func wakeLine(w MessageWake, fired time.Time) string {
	return prompts.Render(prompts.WakeMessage, prompts.Data{
		"Set": w.SetAt.UTC().Format(time.RFC3339), "Due": w.DueAt.UTC().Format(time.RFC3339), "Count": w.Count,
		"Late": fired.Sub(w.DueAt) > WakeLateAfter, "Fired": fired.UTC().Format(time.RFC3339),
	})
}

// rebuiltData are the values the note of a rebuilt working directory is
// rendered with: the paths a Restore names, and how many more of each
// kind it counts than it names.
func rebuiltData(r Restore) prompts.Data {
	omitted := make([]string, len(r.OmittedFiles))
	for i, f := range r.OmittedFiles {
		omitted[i] = f.Path
	}
	return prompts.Data{
		"Files": r.Files, "Paths": r.Paths, "More": max(r.Files-len(r.Paths), 0),
		"Unverified": r.UnverifiedPaths, "UnverifiedMore": max(r.Unverified-len(r.UnverifiedPaths), 0),
		"Omitted": omitted, "OmittedMore": max(r.Omitted-len(omitted), 0),
		"Commands": r.Commands,
	}
}

// workRestoredData are the values the note of a working directory
// restored from the session's copy of it is rendered with: rebuiltData's,
// what the copy gave and left out, the directories it leaves out but a
// repository's history, which the note names apart, and its bound on one
// file.
func workRestoredData(r Restore) prompts.Data {
	d := rebuiltData(r)
	skipped := make([]string, len(r.Copy.SkippedFiles))
	for i, f := range r.Copy.SkippedFiles {
		skipped[i] = f.Path
	}
	d["Copied"], d["Turn"], d["Inherited"] = r.Copy.Files, r.Copy.Turn, r.Copy.Inherited
	d["Skipped"], d["SkippedMore"] = skipped, max(r.Copy.Skipped-len(skipped), 0)
	d["LeftOut"] = strings.Join(slices.DeleteFunc(slices.Clone(WorkLeftOut), func(n string) bool { return n == ".git" }), ", ")
	d["MaxFileMiB"] = MaxWorkFileBytes >> 20
	return d
}

// fence is the fence a text is set between: three backticks, or one
// more than the longest run of backticks the text holds, so nothing in
// the text closes it.
func fence(text string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	return strings.Repeat("`", max(3, longest+1))
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
func emitBefore(ranges []summaryRange, seq uint64, user func(...lux.Block), history bool) {
	for i := range ranges {
		if !ranges[i].emitted && ranges[i].to < seq {
			ranges[i].emitted = true
			user(summary(ranges[i], history))
		}
	}
}

// resultBlock is the result of call id, or the line that stands in for
// it once it was cleared, which for a thread that holds the history tool
// names the call's id the tool reads it back by.
func resultBlock(id string, content []lux.Block, isError bool, cleared map[string]bool, history bool) lux.Block {
	blocks := slices.Clone(content)
	switch {
	case cleared[id] && history:
		blocks = []lux.Block{text(prompts.Render(prompts.TranscriptClearedHistory, prompts.Data{"ID": id}))}
	case cleared[id]:
		blocks = []lux.Block{text(prompts.Text(prompts.TranscriptCleared))}
	}
	return lux.Block{Type: ir.BlockToolResult, ToolResult: &lux.ToolResult{ToolUseID: id, Blocks: blocks, IsError: isError}}
}

func text(s string) lux.Block { return lux.Block{Type: ir.BlockText, Text: s} }

// summary is the user text a summary compaction renders as: for a thread
// that holds the history tool, with the events it covers, which the tool
// reads back.
func summary(r summaryRange, history bool) lux.Block {
	if history {
		return text(prompts.Render(prompts.TranscriptSummaryHistory, prompts.Data{"Summary": r.summary, "From": r.from, "To": r.to}))
	}
	return text(prompts.Render(prompts.TranscriptSummary, prompts.Data{"Summary": r.summary}))
}

// sender is the values a text naming a sender renders with; the text
// falls back from the name to the subject to a word of its own.
func sender(s Sender) prompts.Data {
	return prompts.Data{"Name": s.Name, "Subject": s.Subject}
}
