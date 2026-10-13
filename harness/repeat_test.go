// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// threadResults are the tool.result payloads of one thread, in order.
func (e *env) threadResults(thread string) []session.ToolResult {
	e.t.Helper()
	var out []session.ToolResult
	for _, ev := range e.all() {
		if ev.Type == session.TypeToolResult && ev.Thread == thread {
			var p session.ToolResult
			if err := ev.Decode(&p); err != nil {
				e.t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	return out
}

// noted reports whether a result carries the note of a repeated call
// after its own content.
func noted(r session.ToolResult) bool {
	n := len(r.Content)
	return n > 1 && r.Content[n-1].Text == prompts.Text(prompts.CallRepeated)
}

// never is a call none of whose results carries the note.
func never(int) bool { return false }

// expectNoted checks that the request's last message holds the result of
// a repeated call: the model reads the note with the refusal.
func expectNoted(r *ir.Request) error {
	last := r.Messages[len(r.Messages)-1]
	if len(last.Blocks) != 1 || last.Blocks[0].ToolResult == nil {
		return fmt.Errorf("the last message is no result: %+v", last)
	}
	tr := last.Blocks[0].ToolResult
	if n := len(tr.Blocks); n != 2 || tr.Blocks[1].Text != prompts.Text(prompts.CallRepeated) {
		return fmt.Errorf("the repeated call's result did not carry the note: %+v", tr.Blocks)
	}
	return nil
}

// TestARefusedCallMadeAgainEndsTheTurnAtTheBound: a model that sends a
// call its tool refuses on its input again, unchanged, reads the refusal
// and the note on each repeat, and the turn ends with repeated_call once
// the call repeated MaxRepeatedCalls times, after every call has its
// result and before another request. The input is compared as JSON, so
// other spacing is the same call. The refusal is answered before the call
// is decided, so the guard holds for a call that has no agent.tool_use.
func TestARefusedCallMadeAgainEndsTheTurnAtTheBound(t *testing.T) {
	pub := &checkingTool{fakeTool: &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	for i := range MaxRepeatedCalls + 1 {
		args := `{"text":"nope"}`
		if i == 1 {
			args = `{ "text" : "nope" }`
		}
		r := reply(ir.StopToolUse, call(fmt.Sprintf("toolu_%d", i), session.ToolPublish, args))
		if i == 2 {
			r.Expect = expectNoted
		}
		e.stub.Script(model, r)
	}
	e.stub.Script(model, reply(ir.StopEndTurn, text("A request past the bound.")))
	e.send(ctx, "Publish it.")
	out := e.turn(ctx)
	if out.Status != session.StatusIdle || out.StopReason != session.StopError || out.Detail != CodeRepeatedCall {
		t.Fatalf("outcome %+v, want idle error %s", out, CodeRepeatedCall)
	}
	if n := len(e.stub.Requests()); n != MaxRepeatedCalls+1 {
		t.Fatalf("%d requests, want %d: the turn ends before another", n, MaxRepeatedCalls+1)
	}
	results := e.threadResults("")
	if len(results) != MaxRepeatedCalls+1 {
		t.Fatalf("%d results for %d calls", len(results), MaxRepeatedCalls+1)
	}
	for i, r := range results {
		if r.ToolUseID != fmt.Sprintf("toolu_%d", i) || !r.IsError || r.Content[0].Text != "refused: nope" || noted(r) != (i > 0) {
			t.Fatalf("result %d %+v, want the refusal, and the note on each repeat", i, r)
		}
	}
	if ran := pub.ran(); len(ran) != 0 {
		t.Fatalf("a refused call ran: %v", ran)
	}
	errs := e.events(ctx, session.TypeSessionError)
	var se session.SessionError
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeRepeatedCall || se.Message != MessageRepeatedCall || se.Retryable ||
		!strings.Contains(se.Detail, fmt.Sprintf("%s was called %d times", session.ToolPublish, MaxRepeatedCalls+1)) || !strings.HasSuffix(se.Detail, fmt.Sprintf("toolu_%d", MaxRepeatedCalls)) {
		t.Fatalf("session.error %+v", se)
	}
	evs := e.all()
	var st session.SessionStatus
	if last := evs[len(evs)-1]; last.Type != session.TypeSessionStatus || last.Decode(&st) != nil || st.Status != session.StatusIdle ||
		st.StopReason != session.StopError || st.Detail != CodeRepeatedCall || evs[len(evs)-2].Type != session.TypeSessionError {
		t.Fatalf("the turn closed with %s %s, after %s", evs[len(evs)-1].Type, evs[len(evs)-1].Payload, evs[len(evs)-2].Type)
	}
	if open := openCalls(evs, ""); len(open) != 0 {
		t.Fatalf("%d calls left without a result", len(open))
	}
}

// TestTheFirstRepeatOfAFailedCallCarriesTheNote: a call that ran and
// failed, made again unchanged and failing the same way, is answered with
// the failure and the note, which the model reads on its next request; a
// model that then changes the call goes on, and the turn ends as it
// chooses.
func TestTheFirstRepeatOfAFailedCallCarriesTheNote(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) { return tools.Text(tools.OutcomeError, "disk full"), nil }
	noteRead := reply(ir.StopToolUse, call("toolu_3", "echo", `{"text":"y"}`))
	noteRead.Expect = expectNoted
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)),
		reply(ir.StopToolUse, call("toolu_2", "echo", `{"text":"x"}`)),
		noteRead,
		reply(ir.StopEndTurn, text("The disk is full.")),
	)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	results := e.threadResults("")
	if len(results) != 3 || noted(results[0]) || !noted(results[1]) || noted(results[2]) || results[1].Content[0].Text != "disk full" {
		t.Fatalf("results %+v, want the note on the repeat alone", results)
	}
	if n := len(e.echo.ran()); n != 3 {
		t.Fatalf("echo ran %d times; a repeated call still runs", n)
	}
	if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
		t.Fatalf("session.error %+v", errs)
	}
}

// TestCallsThatChangeOrSucceedAreNotRepeats: a call whose input differs,
// a call whose error differs, as a command run again after a file
// changed, and a call that succeeds, as a read that polls a log, are no
// repeats: past the bound, none carries the note and the turn ends as
// the model chooses. A success clears the call's count, so the same error
// after it counts from the start again.
func TestCallsThatChangeOrSucceedAreNotRepeats(t *testing.T) {
	for _, c := range []struct {
		name string
		args func(i int) string
		run  func(i int) tools.Result
		// noted reports whether result i carries the note.
		noted func(i int) bool
	}{
		{"other input", func(i int) string { return fmt.Sprintf(`{"text":"%d"}`, i) }, func(int) tools.Result { return tools.Text(tools.OutcomeError, "disk full") }, never},
		{"other error", func(int) string { return `{"text":"x"}` }, func(i int) tools.Result { return tools.Text(tools.OutcomeError, fmt.Sprintf("%d tests failed", i)) }, never},
		{"success", func(int) string { return `{"text":"x"}` }, func(int) tools.Result { return tools.Text(tools.OutcomeOK, "still running") }, never},
		// The repeat before the success and the repeats from the second
		// error after it are noted; without the clear the first error
		// after it would be noted too and the last would end the turn.
		{"success between", func(int) string { return `{"text":"x"}` }, func(i int) tools.Result {
			if i == 2 {
				return tools.Text(tools.OutcomeOK, "built")
			}
			return tools.Text(tools.OutcomeError, "disk full")
		}, func(i int) bool { return i == 1 || i > 3 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t, nil)
			ctx := t.Context()
			ran := 0
			e.echo.run = func(tools.Call) (tools.Result, error) {
				ran++
				return c.run(ran - 1), nil
			}
			calls := MaxRepeatedCalls + 2
			for i := range calls {
				e.stub.Script(model, reply(ir.StopToolUse, call(fmt.Sprintf("toolu_%d", i), "echo", c.args(i))))
			}
			e.stub.Script(model, reply(ir.StopEndTurn, text("Done.")))
			e.send(ctx, "Go.")
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			results := e.threadResults("")
			if len(results) != calls {
				t.Fatalf("%d results", len(results))
			}
			for i, r := range results {
				if want := c.noted(i); noted(r) != want {
					t.Fatalf("result %d %+v, noted %v", i, r, !want)
				}
			}
			if errs := e.events(ctx, session.TypeSessionError); len(errs) != 0 {
				t.Fatalf("session.error %+v", errs)
			}
		})
	}
}

// TestAThreadCountsItsOwnRepeats: a subagent that repeats a refused call
// ends its own turn with repeated_call, its session.error on its thread,
// and the parent reads that the thread stopped and goes on. The parent's
// calls and the thread's are counted apart: the thread's first call is no
// repeat of the parent's identical one, and the parent's second call is
// its first repeat, though the thread made the call MaxRepeatedCalls+1
// times in between.
func TestAThreadCountsItsOwnRepeats(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) { return tools.Text(tools.OutcomeError, "refused: nope"), nil }
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_p1", "echo", `{"text":"nope"}`)),
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)),
		reply(ir.StopToolUse, call("toolu_p2", "echo", `{"text":"nope"}`)),
		reply(ir.StopEndTurn, text("The reviewer got stuck.")),
	)
	for i := range MaxRepeatedCalls + 1 {
		e.stub.Script(reviewerModel, reply(ir.StopToolUse, call(fmt.Sprintf("toolu_r%d", i), "echo", `{"text":"nope"}`)))
	}
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("A request past the bound.")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v, want the parent to go on", out)
	}
	id, _ := e.thread(e.all())
	child := e.threadResults(id)
	if len(child) != MaxRepeatedCalls+1 {
		t.Fatalf("%d thread results", len(child))
	}
	for i, r := range child {
		if noted(r) != (i > 0) {
			t.Fatalf("thread result %d %+v, want the note on each of the thread's own repeats", i, r)
		}
	}
	parent := e.threadResults("")
	if len(parent) != 3 || noted(parent[0]) || parent[1].Outcome != tools.OutcomeError ||
		!strings.Contains(parent[1].Content[0].Text, "stopped: error "+CodeRepeatedCall) || !noted(parent[2]) {
		t.Fatalf("parent results %+v", parent)
	}
	errs := e.events(ctx, session.TypeSessionError)
	var se session.SessionError
	if len(errs) != 1 || errs[0].Thread != id || errs[0].Decode(&se) != nil || se.Code != CodeRepeatedCall {
		t.Fatalf("session.error %+v, want one on the thread", errs)
	}
	if n := len(e.events(ctx, session.TypeSessionStatus)); n != 2 {
		t.Fatalf("%d session.status events; a thread's turn appends none", n)
	}
}
