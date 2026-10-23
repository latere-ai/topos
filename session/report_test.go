// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// report is a context report of used tokens of a 1000-token window.
func report(used int64) *ContextReport {
	return &ContextReport{Window: 1000, Used: used, Source: ContextFromProvider, Parts: []ReportPart{
		{Part: ReportInstructions, Tokens: used}, {Part: ReportFree, Tokens: 1000 - used},
	}}
}

// TestTheSessionFoldsItsContextReport (spec 087): the header holds the
// context of the latest request of the session's own thread that records
// one; a subagent's request, a title's, a request that records none and
// a redacted one leave it.
func TestTheSessionFoldsItsContextReport(t *testing.T) {
	l := &qlog{t: t}
	var s Session
	ApplyBatch(&s, l.evs)
	if s.ContextReport != nil {
		t.Fatalf("a session with no request reports %+v", s.ContextReport)
	}
	l.add(TypeModelRequest, ModelRequest{Model: "m", Outcome: "ok", Context: report(100)})
	latest := l.add(TypeModelRequest, ModelRequest{Model: "m", Outcome: "ok", Context: report(300)})
	l.add(TypeModelRequest, ModelRequest{Model: "m", Outcome: "ok", Context: report(900)})
	l.evs[len(l.evs)-1].Thread = "thr_1"
	l.add(TypeModelRequest, ModelRequest{Model: "small", Outcome: "ok", Purpose: PurposeTitle, Context: report(50)})
	l.add(TypeModelRequest, ModelRequest{Model: "m", Outcome: "ok"})
	gone := l.add(TypeModelRequest, ModelRequest{Model: "m", Outcome: "ok", Context: report(700)})
	l.redact(gone.ID)
	ApplyBatch(&s, l.evs)
	if s.ContextReport == nil || s.ContextReport.Used != 300 {
		t.Fatalf("the header reports %+v, want the 300 tokens of %s", s.ContextReport, latest.ID)
	}

	// A session read back from its JSON keeps the report under its own
	// name, beside the context an allow attached.
	s.Context = []ContextPart{{Title: "Notes", Text: "Ship on Friday."}}
	raw, err := Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var back Session
	if err := (Event{Payload: raw}).Decode(&back); err != nil {
		t.Fatal(err)
	}
	if back.ContextReport == nil || back.ContextReport.Used != 300 || len(back.Context) != 1 {
		t.Fatalf("the session read back %+v", back)
	}
}

// TestTheFoldNamesTheFilesAndSummariesItRenders (spec 087): a transcript
// says which of its blocks name a person's files and carry their text,
// and which stand for compacted history, so a request's report counts
// them apart.
func TestTheFoldNamesTheFilesAndSummariesItRenders(t *testing.T) {
	l := &qlog{t: t}
	l.add(TypeUserMessage, said("What did we decide?"))
	l.add(TypeAgentMessage, AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Three things."}}}, StopReason: ir.StopEndTurn})
	l.add(TypeContextCompacted, ContextCompacted{Kind: CompactSummary, FromSeq: 1, ToSeq: 2, Summary: "Three decisions."})
	notes := Attachment{Name: "notes.txt", MediaType: "text/plain", Size: 5, Blob: DigestOf([]byte("notes")), Path: "/workspace/notes.txt", Inline: true}
	msg := said("And these?")
	msg.Attachments = []Attachment{notes}
	l.add(TypeUserMessage, msg)
	tr, err := FoldWith(l.evs, "", FoldOptions{Texts: map[Digest]string{notes.Blob: "notes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Attached) != 2 || len(tr.Summaries) != 1 {
		t.Fatalf("attached %+v, summaries %+v", tr.Attached, tr.Summaries)
	}
	if got := tr.Summaries[0].Text; got != tr.Messages[0].Blocks[0].Text {
		t.Fatalf("the summary block %q is not the transcript's %q", got, tr.Messages[0].Blocks[0].Text)
	}
}
