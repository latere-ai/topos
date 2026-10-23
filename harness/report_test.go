// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// reportParts are the names of a report's parts in their order.
func reportParts(r *session.ContextReport) []string {
	var out []string
	for _, p := range r.Parts {
		out = append(out, p.Part)
	}
	return out
}

// tokensOf is the tokens of the part named in r.
func tokensOf(r *session.ContextReport, part string) int64 {
	for _, p := range r.Parts {
		if p.Part == part {
			return p.Tokens
		}
	}
	return -1
}

// checkReport holds r to the rules every report keeps: the parts in
// order, the parts but free summing to used, and free the window less
// used.
func checkReport(t *testing.T, r *session.ContextReport, window int64) {
	t.Helper()
	want := []string{session.ReportInstructions, session.ReportTools, session.ReportMessages, session.ReportFiles, session.ReportSummary, session.ReportFree}
	if got := reportParts(r); len(got) != len(want) {
		t.Fatalf("parts %v, want %v", got, want)
	}
	var sum int64
	for i, p := range r.Parts {
		if p.Part != want[i] {
			t.Fatalf("part %d is %s, want %s", i, p.Part, want[i])
		}
		if p.Tokens < 0 {
			t.Fatalf("part %s has %d tokens", p.Part, p.Tokens)
		}
		if p.Part != session.ReportFree {
			sum += p.Tokens
		}
	}
	if sum != r.Used {
		t.Fatalf("the parts sum to %d, used is %d: %+v", sum, r.Used, r)
	}
	if r.Window != window || tokensOf(r, session.ReportFree) != max(window-r.Used, 0) {
		t.Fatalf("window %d, free %d, used %d", r.Window, tokensOf(r, session.ReportFree), r.Used)
	}
}

// TestEveryRequestReportsItsContext (spec 087): a step's model.request
// records the model's window, the provider's prompt tokens as used, and
// the request's parts in order summing to them, with the files a person
// brought and the summary of compacted history counted apart; a request
// that failed records the harness's estimate and says so.
func TestEveryRequestReportsItsContext(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	body := []byte("the meeting notes, which the request names")
	d, err := e.store.PutBlob(ctx, e.s.ID, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "List what we decided."}}}, t0)
	answer, _ := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Three things."}}}, StopReason: ir.StopEndTurn}, t0)
	idle, _ := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, t0)
	e.appendEvents(ctx, first, answer, idle)
	summary, _ := session.NewEvent(session.TypeContextCompacted, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 3,
		Summary: "Ada asked what was decided, and the answer named three decisions about the launch, the budget and the hiring plan."}, t0)
	attached, _ := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And these notes?"}},
		Attachments: []session.Attachment{{Name: "notes.txt", MediaType: "text/plain", Size: int64(len(body)), Blob: d, Path: "/workspace/attachments/notes.txt"}}}, t0)
	e.appendEvents(ctx, summary, attached)
	e.running(ctx)
	cache := int64(300)
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("They agree.")}, StopReason: ir.StopEndTurn,
		Usage: ir.Usage{InputTokens: 1200, CacheReadInputTokens: &cache, OutputTokens: 10}}})
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	mrs := e.modelRequests(ctx)
	if len(mrs) != 1 || mrs[0].Context == nil {
		t.Fatalf("model requests %+v", mrs)
	}
	r := mrs[0].Context
	checkReport(t, r, 200_000)
	if r.Source != session.ContextFromProvider || r.Used != 1500 {
		t.Fatalf("used %d from %s, want 1500 from the provider", r.Used, r.Source)
	}
	for _, part := range []string{session.ReportInstructions, session.ReportTools, session.ReportMessages, session.ReportFiles, session.ReportSummary} {
		if tokensOf(r, part) <= 0 {
			t.Fatalf("part %s counted nothing: %+v", part, r.Parts)
		}
	}

	// A request the provider refused counts nothing of its own; the
	// report is the harness's estimate.
	e.send(ctx, "Once more.")
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: 400, Times: 9}})
	e.turn(ctx)
	mrs = e.modelRequests(ctx)
	failed := mrs[len(mrs)-1]
	if failed.Outcome != "error" || failed.Context == nil {
		t.Fatalf("the failed request %+v", failed)
	}
	checkReport(t, failed.Context, 200_000)
	if failed.Context.Source != session.ContextFromEstimate || failed.Context.Used <= 0 {
		t.Fatalf("a failed request's report %+v", failed.Context)
	}
}

// TestAReportScalesItsEstimates: the parts are the estimates scaled to
// what was used, the rounding's remainder on the largest part, and a
// request the provider counted beyond the window leaves nothing free.
func TestAReportScalesItsEstimates(t *testing.T) {
	e := setup(t, func(c *Config) { c.Entry.InputWindow = 1000 })
	tr := &turn{h: e.h}
	r := tr.contextReport(requestShares{instructions: 1, tools: 1, messages: 1, estimate: 3}, 1200)
	if tokensOf(r, session.ReportInstructions) != 400 || tokensOf(r, session.ReportTools) != 400 || tokensOf(r, session.ReportMessages) != 400 || tokensOf(r, session.ReportFree) != 0 {
		t.Fatalf("report %+v", r)
	}
	r = tr.contextReport(requestShares{instructions: 2, messages: 1, estimate: 3}, 100)
	if tokensOf(r, session.ReportInstructions) != 67 || tokensOf(r, session.ReportMessages) != 33 || tokensOf(r, session.ReportFree) != 900 {
		t.Fatalf("report %+v", r)
	}
	r = tr.contextReport(requestShares{}, 0)
	if r.Used != 0 || r.Source != session.ContextFromEstimate || tokensOf(r, session.ReportFree) != 1000 {
		t.Fatalf("an empty report %+v", r)
	}
}
