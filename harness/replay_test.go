// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// foreign appends events as another writer does while a turn runs: a
// client's message arriving mid-step, which the step's request did not
// carry.
func (e *env) foreign(ctx context.Context, text string) {
	e.t.Helper()
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	other := &storeLog{st: e.store, id: e.s.ID, last: evs[len(evs)-1].Seq}
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := other.Append(ctx, []session.Event{msg}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) replay(ctx context.Context, h *Harness, log []session.Event) []models.ReplayStep {
	e.t.Helper()
	steps, err := models.Replay(ctx, log, h.Rebuild(e.s, e.log), &dialect.Model{})
	if err != nil {
		e.t.Fatal(err)
	}
	return steps
}

func outcomes(steps []models.ReplayStep) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Outcome
	}
	return out
}

// TestReplayReproducesRequestHash: every request of a recorded session,
// the main thread's, a subagent's, the advisor's and a compaction's
// summary request among them, is built again from the log and encoded
// by this build to the bytes whose hash the session recorded, though a
// message arrived while a step was being answered.
func TestReplayReproducesRequestHash(t *testing.T) {
	e := setup(t, func(c *Config) { withReviewer(nil)(c); withAdvisor(c) })
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, ir.Block{Type: ir.BlockThinking, Text: "plan", Signature: "sig-1"}, call("toolu_1", "echo", `{"text":"a"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)}, StopReason: ir.StopToolUse}, Respond: func(*ir.Request, *ir.Response) {
			e.foreign(ctx, "Also check go.mod.")
		}},
		reply(ir.StopToolUse, call("toolu_a", ToolAdvisor, `{"question":"Enough?"}`)),
		reply(ir.StopEndTurn, text("Done.")),
		reply(ir.StopEndTurn, text("Checked go.mod.")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopToolUse, call("toolu_r", "echo", `{"text":"main.go"}`)), reply(ir.StopEndTurn, text("Fine.")))
	e.stub.Script(advisorModel, reply(ir.StopEndTurn, text("Enough.")))
	e.send(ctx, "Work.")
	for range 2 {
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
		e.running(ctx)
	}
	log := e.all()
	steps := e.replay(ctx, e.h, log)
	if len(steps) != 8 || slices.ContainsFunc(steps, func(s models.ReplayStep) bool { return s.Outcome != models.ReplayMatch }) {
		t.Fatalf("replay %+v", steps)
	}
	threads := map[string]bool{}
	for _, s := range steps {
		threads[s.Thread] = true
		if s.Replayed != s.Recorded || s.Codec != s.Build {
			t.Fatalf("step %+v", s)
		}
	}
	if len(threads) != 3 {
		t.Fatalf("the replay covered threads %v, want the main thread, the reviewer's and the advisor's", threads)
	}

	// Folding the log up to the request instead of through its fold
	// point would take in the message that arrived during step 2, which
	// its request never carried.
	var second session.ModelRequest
	for _, ev := range log {
		if ev.Type == session.TypeModelRequest && ev.Thread == "" && ev.Step == 2 && ev.Turn == 1 {
			if err := ev.Decode(&second); err != nil {
				t.Fatal(err)
			}
		}
	}
	arrived := slices.IndexFunc(log, func(ev session.Event) bool {
		var p session.UserMessage
		return ev.Type == session.TypeUserMessage && ev.Decode(&p) == nil && p.Content[0].Text == "Also check go.mod."
	})
	if arrived < 0 || second.FoldSeq == 0 || log[arrived].Seq <= second.FoldSeq {
		t.Fatalf("the message at %d is not after step 2's fold point %d", arrived, second.FoldSeq)
	}
}

func TestReplayReportsWhatItDoesNotCompare(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review."}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("Fine.")))
	e.send(ctx, "Work.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	log := e.all()
	edit := func(i int, f func(*session.ModelRequest)) []session.Event {
		out := slices.Clone(log)
		var mr session.ModelRequest
		if err := out[i].Decode(&mr); err != nil {
			t.Fatal(err)
		}
		f(&mr)
		b, err := session.Marshal(mr)
		if err != nil {
			t.Fatal(err)
		}
		out[i].Payload = b
		return out
	}
	var first int
	for i, ev := range log {
		if ev.Type == session.TypeModelRequest {
			first = i
			break
		}
	}
	for name, c := range map[string]struct {
		log  []session.Event
		want string
	}{
		"another codec":    {edit(first, func(mr *session.ModelRequest) { mr.Codec = "anthropic-messages@v0.0.1" }), models.CodeMismatch},
		"another hash":     {edit(first, func(mr *session.ModelRequest) { mr.RequestSHA256 = "00" }), models.ReplayMismatch},
		"no hash":          {edit(first, func(mr *session.ModelRequest) { mr.RequestSHA256 = "" }), models.ReplaySkipped},
		"no fold point":    {edit(first, func(mr *session.ModelRequest) { mr.FoldSeq = 0 }), models.ReplaySkipped},
		"a prompt version": {edit(first, func(mr *session.ModelRequest) { mr.PromptVersion = "v1" }), models.ReplaySkipped},
	} {
		if steps := e.replay(ctx, e.h, c.log); steps[0].Outcome != c.want || steps[0].Outcome == models.CodeMismatch && steps[0].Replayed != "" {
			t.Errorf("%s: %+v", name, steps[0])
		}
	}
	redacted := slices.Clone(log)
	for i, ev := range redacted {
		if ev.Type == session.TypeUserMessage {
			redacted[i].Payload = json.RawMessage(`{"tombstone":true}`)
		}
	}
	if steps := e.replay(ctx, e.h, redacted); steps[0].Outcome != models.ReplaySkipped || steps[0].Detail == "" {
		t.Fatalf("a log redacted after the request: %+v", steps[0])
	}
	cfg := e.cfg
	cfg.Subagents = nil
	lone, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Without the subagent the main thread holds no spawn tool, so its
	// requests come out other than they were sent, and the reviewer's
	// thread cannot be built at all.
	if got := outcomes(e.replay(ctx, lone, log)); !slices.Equal(got, []string{models.ReplayMismatch, models.ReplaySkipped, models.ReplayMismatch}) {
		t.Fatalf("a configuration without the subagent: %v", got)
	}
	failed := edit(first, func(*session.ModelRequest) {})
	failed[first].Payload = json.RawMessage(`{"model":`)
	if _, err := models.Replay(ctx, failed, e.h.Rebuild(e.s, e.log), &dialect.Model{}); err == nil {
		t.Fatal("an unreadable model.request replayed")
	}
	if _, err := (replayLog{}).Append(ctx, nil); !errors.Is(err, errReplayWrite) {
		t.Fatalf("a replay appended: %v", err)
	}
	if _, err := (replayLog{}).PutBlob(ctx, nil); !errors.Is(err, errReplayWrite) {
		t.Fatalf("a replay stored a blob: %v", err)
	}
}

// TestASummaryRequestReplays: a compaction's summary request is built
// again from the range it summarized.
func TestASummaryRequestReplays(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.history(ctx, 8000, 8000, 8000, 8000, 40)
	e.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Requests: the list. Next step: finish it.")}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 9000, OutputTokens: 30}}},
		reply(ir.StopEndTurn, text("Finished.")),
	)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if len(e.compactions(ctx)) == 0 {
		t.Fatal("no compaction")
	}
	if got := outcomes(e.replay(ctx, e.h, e.all())); !slices.Equal(got, []string{models.ReplayMatch, models.ReplayMatch}) {
		t.Fatalf("replay %v", got)
	}
}
