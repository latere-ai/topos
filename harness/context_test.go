// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// history appends a user message and one tool step per size: a tool_use
// of echo and a result of that many bytes.
func (e *env) history(ctx context.Context, sizes ...int) {
	e.t.Helper()
	e.send(ctx, "Work through the list.")
	for _, n := range sizes {
		id := fmt.Sprintf("toolu_h%02d", e.hist)
		e.hist++
		msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: id, Name: "echo", Args: json.RawMessage(`{"text":"x"}`)}}}}, StopReason: ir.StopToolUse}, t0)
		if err != nil {
			e.t.Fatal(err)
		}
		use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: "echo", Input: json.RawMessage(`{"text":"x"}`), Verdict: "allow"}, t0)
		if err != nil {
			e.t.Fatal(err)
		}
		res, err := session.NewEvent(session.TypeToolResult, session.ToolResult{ToolUseID: id, Content: []lux.Block{{Type: ir.BlockText, Text: strings.Repeat("r", n)}}, Outcome: tools.OutcomeOK}, t0)
		if err != nil {
			e.t.Fatal(err)
		}
		msg.Turn, use.Turn, res.Turn = 1, 1, 1
		e.appendEvents(ctx, msg, use, res)
	}
}

// manage runs the context management of a turn over the session's log.
func (e *env) manage(ctx context.Context) (*turn, error) {
	e.t.Helper()
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	t := &turn{h: e.h, s: s, sh: &shared{events: evs}, l: e.log, root: e.h.c.Tools, reg: e.h.c.Tools, num: s.Turn + 1, start: t0, deadline: t0.Add(DefaultRetry.Max * 100)}
	tr, err := session.Fold(t.events(), "")
	if err != nil {
		e.t.Fatal(err)
	}
	req, sum, err := t.request(ctx, tr)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _, err = t.manageContext(ctx, req, sum)
	return t, err
}

func (e *env) compactions(ctx context.Context) []session.ContextCompacted {
	e.t.Helper()
	var out []session.ContextCompacted
	for _, ev := range e.events(ctx, session.TypeContextCompacted) {
		var p session.ContextCompacted
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func window(n int64) func(*Config) {
	return func(c *Config) { c.Entry.InputWindow = n }
}

func TestClearingOldResultsIsEnough(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.history(ctx, 12000, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	if _, err := e.manage(ctx); err != nil {
		t.Fatal(err)
	}
	cs := e.compactions(ctx)
	if len(cs) != 1 || cs[0].Kind != session.CompactClearToolResults || len(cs[0].ToolUseIDs) != 2 || cs[0].ToolUseIDs[0] != "toolu_h00" {
		t.Fatalf("compactions %+v", cs)
	}
	if cs[0].TokensAfter >= cs[0].TokensBefore || cs[0].TokensAfter >= int64(0.8*8000) {
		t.Fatalf("estimates before %d after %d", cs[0].TokensBefore, cs[0].TokensAfter)
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("%d requests; clearing needs no model", n)
	}
}

func TestSummaryWhenClearingIsNotEnough(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.history(ctx, 8000, 8000, 8000, 8000, 40)
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Requests: Ada asked for the list. Next step: finish it.")}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 9000, OutputTokens: 30}}, Expect: func(r *ir.Request) error {
		last := r.Messages[len(r.Messages)-1]
		if r.ToolChoice == nil || r.ToolChoice.Mode != ir.ToolChoiceNone || !strings.Contains(last.Blocks[len(last.Blocks)-1].Text, "Write that summary now") {
			return errors.New("not a compaction request")
		}
		return nil
	}})
	if _, err := e.manage(ctx); err != nil {
		t.Fatal(err)
	}
	cs := e.compactions(ctx)
	var sum *session.ContextCompacted
	for i := range cs {
		if cs[i].Kind == session.CompactSummary {
			sum = &cs[i]
		}
	}
	if sum == nil || sum.Request == "" || !strings.Contains(sum.Summary, "Next step") || sum.FromSeq == 0 || sum.ToSeq < sum.FromSeq {
		t.Fatalf("compactions %+v", cs)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Messages[0].Blocks[0].Text, "Summary of the conversation so far:") {
		t.Fatalf("the transcript does not open with the summary: %+v", tr.Messages[0])
	}
	if n := len(e.events(ctx, session.TypeModelRequest)); n != 1 {
		t.Fatalf("%d model.request events; the compaction's request is recorded", n)
	}
}

func TestAContextThatCannotFitIsExhausted(t *testing.T) {
	e := setup(t, window(1500))
	ctx := t.Context()
	e.history(ctx, 20000)
	_, err := e.manage(ctx)
	var stop *errStop
	if !errors.As(err, &stop) || stop.out.Detail != CodeContextExhausted {
		t.Fatalf("an oversize step: %v", err)
	}
}

func TestAFailedCompactionEndsTheTurn(t *testing.T) {
	for name, r := range map[string]luxstub.Reply{
		"http":  {Response: ir.Response{Model: model}, Fail: &luxstub.Failure{Status: 400, Times: 9}},
		"empty": {Response: ir.Response{Model: model, Blocks: []ir.Block{text("   ")}, StopReason: ir.StopEndTurn}},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, window(8000))
			ctx := t.Context()
			e.history(ctx, 8000, 8000, 8000, 8000, 40)
			e.stub.Script(model, r)
			_, err := e.manage(ctx)
			var stop *errStop
			if !errors.As(err, &stop) || stop.out.Detail != CodeCompactionFailed {
				t.Fatalf("a failed compaction: %v", err)
			}
			if n := len(e.compactions(ctx)); n != 0 {
				t.Fatalf("%d compactions after a failure; nothing is replaced", n)
			}
		})
	}
}

func TestTodoResultsAreNeverCleared(t *testing.T) {
	e := setup(t, window(8000))
	todo := &fakeTool{name: "todo", props: tools.Properties{Effect: tools.EffectNone}}
	if err := e.cfg.Tools.AddBuiltin(todo); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.send(ctx, "Plan.")
	id := "toolu_todo"
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: id, Name: "todo", Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: "todo", Input: json.RawMessage(`{}`), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	res, err := session.NewEvent(session.TypeToolResult, session.ToolResult{ToolUseID: id, Content: []lux.Block{{Type: ir.BlockText, Text: strings.Repeat("t", 12000)}}, Outcome: tools.OutcomeOK}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, msg, use, res)
	e.history(ctx, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	if _, err := e.manage(ctx); err != nil {
		var stop *errStop
		if !errors.As(err, &stop) {
			t.Fatal(err)
		}
	}
	for _, c := range e.compactions(ctx) {
		for _, cleared := range c.ToolUseIDs {
			if cleared == id {
				t.Fatal("a todo result was cleared")
			}
		}
	}
}

func TestThresholdIsHeldInRange(t *testing.T) {
	for at, want := range map[float64]int64{0: 800, 0.1: 500, 0.9: 900, 2: 950} {
		e := setup(t, func(c *Config) { c.Entry.InputWindow = 1000; c.CompactAt = at })
		tr := &turn{h: e.h}
		if got := tr.threshold(); got != want {
			t.Fatalf("CompactAt %v: threshold %d, want %d", at, got, want)
		}
	}
	if promptTokens(lux.Usage{InputTokens: 1, CacheReadInputTokens: ptr(2), CacheWriteInputTokens: ptr(3)}) != 6 {
		t.Fatal("promptTokens")
	}
}

func ptr(v int64) *int64 { return &v }

func TestASecondClearingSkipsWhatIsCleared(t *testing.T) {
	e := setup(t, window(8000))
	ctx := t.Context()
	e.history(ctx, 12000, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	if _, err := e.manage(ctx); err != nil {
		t.Fatal(err)
	}
	e.history(ctx, 12000, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	if _, err := e.manage(ctx); err != nil {
		t.Fatal(err)
	}
	cs := e.compactions(ctx)
	if len(cs) != 2 {
		t.Fatalf("compactions %+v", cs)
	}
	for _, id := range cs[1].ToolUseIDs {
		if id == cs[0].ToolUseIDs[0] || id == cs[0].ToolUseIDs[1] {
			t.Fatalf("the second clearing repeats %s", id)
		}
	}
}
