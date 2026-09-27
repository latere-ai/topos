// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

const reviewerModel = "reviewer-model"

func withReviewer(mut func(*Subagent)) func(*Config) {
	return func(c *Config) {
		conn := c.Connection
		conn.Model = reviewerModel
		sub := Subagent{Name: "reviewer", Instructions: "You review diffs.", Connection: &conn, Tools: []string{"echo"}}
		if mut != nil {
			mut(&sub)
		}
		c.Name = "builder"
		c.Subagents = map[string]Subagent{"reviewer": sub}
	}
}

func spawnCall(id, args string) ir.Block { return call(id, ToolSpawn, args) }

func (e *env) thread(ctxEvents []session.Event) (string, session.ThreadStarted) {
	e.t.Helper()
	for _, ev := range ctxEvents {
		if ev.Type == session.TypeThreadStarted {
			var p session.ThreadStarted
			if err := ev.Decode(&p); err != nil {
				e.t.Fatal(err)
			}
			if ev.Thread != ev.ID {
				e.t.Fatalf("thread.started carries thread %q, want its own id", ev.Thread)
			}
			return ev.ID, p
		}
	}
	e.t.Fatal("no thread.started")
	return "", session.ThreadStarted{}
}

func (e *env) all() []session.Event {
	e.t.Helper()
	evs, err := e.store.Events(e.t.Context(), e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

func TestSpawnRunsASubagentAndMessageContinuesIt(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			for _, tool := range r.Tools {
				if tool.Name == ToolSpawn {
					return nil
				}
			}
			return errors.New("the parent was not offered spawn")
		}},
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_m", ToolMessage, `{}`)}, StopReason: ir.StopToolUse}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_m", ToolMessage, `{"thread":"`+id+`","content":"Check line 9 too.","end":true}`)}
		}},
		reply(ir.StopEndTurn, text("Reviewed.")),
	)
	e.stub.Script(reviewerModel,
		luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{call("toolu_r1", "echo", `{"text":"main.go"}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			if len(r.Messages) != 1 || r.Messages[0].Blocks[0].Text != "Review main.go." {
				return fmt.Errorf("the thread saw more than its task: %+v", r.Messages)
			}
			if !strings.Contains(r.System[1].Text, "You review diffs.") {
				return errors.New("the subagent's instructions are missing")
			}
			names := []string{}
			for _, tool := range r.Tools {
				names = append(names, tool.Name)
			}
			if strings.Join(names, ",") != "echo" {
				return fmt.Errorf("the thread holds %v, want only echo", names)
			}
			return nil
		}},
		reply(ir.StopEndTurn, text("Line 3 leaks a file.")),
		luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Line 9 is fine.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if !strings.Contains(last.Blocks[0].Text, "Message from thread builder:") {
				return fmt.Errorf("the message did not name its sender: %+v", last)
			}
			return nil
		}},
	)
	e.send(ctx, "Build and review.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	evs := e.all()
	id, started := e.thread(evs)
	if started.Parent != "" || started.Depth != 1 || started.Agent.Name != "reviewer" || started.Model != reviewerModel || strings.Join(started.Tools, ",") != "echo" {
		t.Fatalf("thread.started %+v", started)
	}
	var results []session.ToolResult
	for _, ev := range evs {
		if ev.Type == session.TypeToolResult && ev.Thread == "" {
			var p session.ToolResult
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			results = append(results, p)
		}
	}
	if len(results) != 2 || !strings.Contains(results[0].Content[0].Text, "Line 3 leaks a file.") || !strings.Contains(results[0].Content[0].Text, "Thread "+id+" is idle") || !strings.Contains(results[1].Content[0].Text, "Line 9 is fine.") {
		t.Fatalf("parent results %+v", results)
	}
	childEvents := 0
	for _, ev := range evs {
		if ev.Thread == id {
			childEvents++
		}
	}
	if childEvents < 5 {
		t.Fatalf("%d events in the thread", childEvents)
	}
	if !e.hasEnded(evs, id) {
		t.Fatal("end: true did not end the thread")
	}
	statuses := 0
	for _, ev := range evs {
		if ev.Type == session.TypeSessionStatus {
			statuses++
		}
	}
	if statuses != 2 {
		t.Fatalf("%d session.status events; a thread's turn appends none", statuses)
	}
}

func (e *env) hasEnded(evs []session.Event, id string) bool {
	for _, ev := range evs {
		if ev.Type == session.TypeThreadEnded && ev.Thread == id {
			return true
		}
	}
	return false
}

func (e *env) parentResults(t *testing.T) []session.ToolResult {
	t.Helper()
	var out []session.ToolResult
	for _, ev := range e.all() {
		if ev.Type == session.TypeToolResult && ev.Thread == "" {
			var p session.ToolResult
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			out = append(out, p)
		}
	}
	return out
}

func TestThreadToolErrors(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse,
			spawnCall("toolu_1", `{"agent":"reviewer","task":"x","isolation":"worktree"}`),
			call("toolu_2", ToolMessage, `{"thread":"evt_nope","content":"hi"}`),
		),
		reply(ir.StopToolUse, spawnCall("toolu_3", `{"agent":"ghost","task":"x"}`)),
		reply(ir.StopEndTurn, text("ok")),
	)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	byID := map[string]session.ToolResult{}
	for _, r := range e.parentResults(t) {
		byID[r.ToolUseID] = r
	}
	if len(byID) != 3 || !strings.HasPrefix(byID["toolu_1"].Content[0].Text, CodeIsolationUnavailable) || !strings.HasPrefix(byID["toolu_2"].Content[0].Text, CodeThreadNotFound) || byID["toolu_3"].Outcome != tools.OutcomeInvalidInput {
		t.Fatalf("results %+v", byID)
	}
}

func TestTheDepthLimitWithholdsSpawn(t *testing.T) {
	e := setup(t, withReviewer(func(s *Subagent) {
		inner := *s
		s.Subagents = map[string]Subagent{"reviewer": inner}
		s.Tools = nil
	}))
	e.cfg.MaxDepth = 1
	h, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Go deeper.","tools":["echo","bash"]}`)), reply(ir.StopEndTurn, text("done")))
	e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("at the limit")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		for _, tool := range r.Tools {
			if tool.Name == ToolSpawn || tool.Name == ToolMessage {
				return errors.New("a thread at the depth limit was offered " + tool.Name)
			}
		}
		if len(r.Tools) != 2 {
			return fmt.Errorf("%d tools after narrowing to echo and bash", len(r.Tools))
		}
		return nil
	}})
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if e.h.maxDepth() != 1 {
		t.Fatal("maxDepth")
	}
}

func TestAPausedThreadPausesTheSessionAndResumes(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
		c.Machine = fakeMachine{kind: machine.KindHost}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Run the tests."}`)),
		reply(ir.StopEndTurn, text("All good.")),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, call("toolu_b", "bash", `{"command":"make test"}`)),
		reply(ir.StopEndTurn, text("Tests pass.")),
	)
	e.send(ctx, "Test it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.parentResults(t)); n != 0 {
		t.Fatalf("the spawn call has %d results while its thread waits", n)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_b", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmation %+v", out)
	}
	if len(e.write.ran()) != 1 {
		t.Fatalf("the thread's confirmed call ran %d times", len(e.write.ran()))
	}
	res := e.parentResults(t)
	if len(res) != 1 || !strings.Contains(res[0].Content[0].Text, "Tests pass.") {
		t.Fatalf("the spawn result %+v", res)
	}
}

func TestAThreadThatFinishedBeforeACrashIsNotRunAgain(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.send(ctx, "Go.")
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_s", Name: ToolSpawn, Args: json.RawMessage(`{"agent":"reviewer","task":"t"}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_s", Name: ToolSpawn, Input: json.RawMessage(`{"agent":"reviewer","task":"t"}`), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	started, err := session.NewEvent(session.TypeThreadStarted, session.ThreadStarted{Agent: session.AgentRef{Name: "reviewer"}, ToolUseID: "toolu_s", Task: "t", Depth: 1, Tools: []string{"echo"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	started.Thread = started.ID
	answer, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "Already reviewed."}}}, StopReason: ir.StopEndTurn}, t0)
	if err != nil {
		t.Fatal(err)
	}
	answer.Thread = started.ID
	e.appendEvents(ctx, msg, use, started, answer)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("done")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	for _, r := range e.stub.Requests() {
		if r.Model == reviewerModel {
			t.Fatal("the finished thread was asked again")
		}
	}
	if res := e.parentResults(t); len(res) != 1 || !strings.Contains(res[0].Content[0].Text, "Already reviewed.") {
		t.Fatalf("result %+v", res)
	}

	lost := setup(t, withReviewer(nil))
	lost.send(ctx, "Go.")
	use2, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_x", Name: ToolSpawn, Input: json.RawMessage(`{}`), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	lost.appendEvents(ctx, use2)
	lost.running(ctx)
	lost.stub.Script(model, reply(ir.StopEndTurn, text("ok")))
	if out := lost.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := lost.parentResults(t); len(res) != 1 || res[0].Outcome != tools.OutcomeUnknownEffect {
		t.Fatalf("a spawn that started no thread %+v", res)
	}
}

func TestParallelSpawnsAndTheConcurrencyCap(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_a", `{"agent":"reviewer","task":"A"}`), spawnCall("toolu_b", `{"agent":"reviewer","task":"B"}`)),
		reply(ir.StopEndTurn, text("both done")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("one")), reply(ir.StopEndTurn, text("two")))
	e.send(ctx, "Two reviews.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := e.parentResults(t); len(res) != 2 || res[0].Outcome != tools.OutcomeOK || res[1].Outcome != tools.OutcomeOK {
		t.Fatalf("results %+v", res)
	}

	c := setup(t, withReviewer(nil))
	c.cfg.MaxConcurrent = 1
	h, err := New(c.cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.h = h
	c.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_a", `{"agent":"reviewer","task":"A"}`), spawnCall("toolu_b", `{"agent":"reviewer","task":"B"}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	c.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("one")}, StopReason: ir.StopEndTurn}, Expect: func(*ir.Request) error {
		return nil
	}}, reply(ir.StopEndTurn, text("two")))
	c.send(ctx, "Two reviews.")
	if out := c.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	ok, capped := 0, 0
	for _, r := range c.parentResults(t) {
		switch {
		case r.Outcome == tools.OutcomeOK:
			ok++
		case strings.HasPrefix(r.Content[0].Text, CodeTooManyThreads):
			capped++
		}
	}
	if ok+capped != 2 || ok == 0 {
		t.Fatalf("ok %d, capped %d", ok, capped)
	}
}

func TestStricterMode(t *testing.T) {
	for _, c := range []struct{ a, b, want Mode }{
		{ModeProgressive, ModeConfirm, ModeConfirm},
		{ModeConfirm, ModePlan, ModePlan},
		{ModePlan, ModeProgressive, ModePlan},
		{"", "", ModeConfirm},
		{ModeProgressive, "", ModeProgressive},
	} {
		if got := stricterMode(c.a, c.b); got != c.want {
			t.Fatalf("stricterMode(%q, %q) = %q", c.a, c.b, got)
		}
	}
	var entry models.Entry
	if (Subagent{Entry: &entry}).Entry == nil {
		t.Fatal("entry")
	}
}

func TestAThreadThatStopsOtherwiseIsAnErrorResult(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)), reply(ir.StopEndTurn, text("noted")))
	for range 3 {
		e.stub.Script(reviewerModel, reply(ir.StopMaxTokens, text("more")))
	}
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	res := e.parentResults(t)
	if len(res) != 1 || res[0].Outcome != tools.OutcomeError || !strings.Contains(res[0].Content[0].Text, "stopped: output_limit") {
		t.Fatalf("result %+v", res)
	}
}

func TestMessagingAnEndedThread(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)),
		luxstub.Reply{Response: ir.Response{Model: model}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_e", ToolMessage, `{"thread":"`+id+`","content":"bye","end":true}`)}
			r.StopReason = ir.StopToolUse
		}},
		luxstub.Reply{Response: ir.Response{Model: model}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_f", ToolMessage, `{"thread":"`+id+`","content":"again"}`)}
			r.StopReason = ir.StopToolUse
		}},
		reply(ir.StopEndTurn, text("done")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("first")), reply(ir.StopEndTurn, text("last")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	res := e.parentResults(t)
	if len(res) != 3 || !strings.HasPrefix(res[2].Content[0].Text, CodeThreadNotFound) || !strings.Contains(res[2].Content[0].Text, "has ended") {
		t.Fatalf("results %+v", res)
	}
}

func TestTheHarnessTakesAndChainsCheckpoints(t *testing.T) {
	var calls []string
	e := setup(t, func(c *Config) {
		c.Checkpoint = func(_ context.Context, turn int, previous string) (*session.CheckpointRef, error) {
			calls = append(calls, fmt.Sprintf("%d:%s", turn, previous))
			if turn == 3 {
				return nil, errors.New("disk full")
			}
			return &session.CheckpointRef{Ref: fmt.Sprintf("refs/topos/checkpoints/s/%d", turn), Commit: fmt.Sprintf("c%d", turn)}, nil
		}
	})
	ctx := t.Context()
	for i := range 3 {
		e.stub.Script(model, reply(ir.StopEndTurn, text(fmt.Sprint(i))))
		e.send(ctx, "Go.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("turn %d: %+v", i, out)
		}
	}
	if strings.Join(calls, ",") != "1:,2:c1,3:c2" {
		t.Fatalf("checkpoint calls %v", calls)
	}
	if LastCheckpoint(e.all(), "") != "c2" {
		t.Fatal("the failed third checkpoint replaced the last one")
	}
	var se session.SessionError
	if err := e.events(ctx, session.TypeSessionError)[0].Decode(&se); err != nil || se.Code != CodeCheckpointFailed {
		t.Fatalf("session.error %+v", se)
	}
	if !strings.Contains((&appendError{errors.New("x")}).Error(), "append") || !strings.Contains((&errPause{reason: session.StopToolResult}).Error(), "tool_result") {
		t.Fatal("error texts")
	}
}

func TestAPausedMessageResumes(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
		c.Machine = fakeMachine{kind: machine.KindHost}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Get ready."}`)),
		luxstub.Reply{Response: ir.Response{Model: model}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_m", ToolMessage, `{"thread":"`+id+`","content":"Run the tests."}`)}
			r.StopReason = ir.StopToolUse
		}},
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopEndTurn, text("Ready.")),
		reply(ir.StopToolUse, call("toolu_b", "bash", `{"command":"make test"}`)),
		reply(ir.StopEndTurn, text("Tests pass.")),
	)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_b", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmation %+v", out)
	}
	res := e.parentResults(t)
	if len(res) != 2 || !strings.Contains(res[1].Content[0].Text, "Tests pass.") {
		t.Fatalf("results %+v", res)
	}
}

func TestResumingAThreadWhoseSubagentIsGone(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
		c.Machine = fakeMachine{kind: machine.KindHost}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)), reply(ir.StopEndTurn, text("ok")))
	e.stub.Script(reviewerModel, reply(ir.StopToolUse, call("toolu_b", "bash", `{"command":"make"}`)))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	cfg := e.cfg
	cfg.Subagents = map[string]Subagent{"other": {Name: "other"}}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := e.parentResults(t); len(res) != 1 || !strings.HasPrefix(res[0].Content[0].Text, CodeUnknownSubagent) {
		t.Fatalf("results %+v", res)
	}
}

func TestAThreadCanPauseAgainAfterResuming(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = []string{"bash"} })(c)
		c.Machine = fakeMachine{kind: machine.KindHost}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)), reply(ir.StopEndTurn, text("ok")))
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, call("toolu_b1", "bash", `{"command":"make"}`)),
		reply(ir.StopToolUse, call("toolu_b2", "bash", `{"command":"make install"}`)),
		reply(ir.StopEndTurn, text("installed")),
	)
	e.send(ctx, "Go.")
	for i, id := range []string{"toolu_b1", "toolu_b2"} {
		if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
			t.Fatalf("pause %d: %+v", i, out)
		}
		conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: id, Decision: session.DecisionAllow}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, conf)
		e.running(ctx)
	}
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := e.parentResults(t); len(res) != 1 || !strings.Contains(res[0].Content[0].Text, "installed") {
		t.Fatalf("results %+v", res)
	}
}

func TestSettled(t *testing.T) {
	const th = "evt_thread"
	mk := func(seq uint64, typ session.Type, p any) session.Event {
		e, err := session.NewEvent(typ, p, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq, e.Thread = seq, th
		return e
	}
	answer := mk(5, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "done"}}}})
	cut := mk(5, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: "cut"}}}, Truncated: true})
	asks := mk(5, session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "x", Name: "echo"}}}}})
	open := mk(6, session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "x", Name: "echo", Verdict: "ask"})
	for name, c := range map[string]struct {
		evs    []session.Event
		driven uint64
		want   bool
	}{
		"answered":         {[]session.Event{answer}, 3, true},
		"before the drive": {[]session.Event{answer}, 7, false},
		"truncated":        {[]session.Event{cut}, 3, false},
		"calls a tool":     {[]session.Event{asks}, 3, false},
		"an open call":     {[]session.Event{asks, open}, 3, false},
		"nothing yet":      {nil, 3, false},
	} {
		if got, _ := settled(c.evs, th, c.driven); got != c.want {
			t.Fatalf("%s: settled %v", name, got)
		}
	}
	if pauseReason(errors.New("x")) != "" {
		t.Fatal("a reason from an error that is not a pause")
	}
}

func TestAPauseFromASequentialCallAndALostAppend(t *testing.T) {
	e := setup(t, nil)
	pauser := &fakeTool{name: "wait_here", props: tools.Properties{Effect: tools.EffectNone}, run: func(tools.Call) (tools.Result, error) {
		return tools.Result{}, &errPause{reason: session.StopToolResult}
	}}
	if err := e.cfg.Tools.AddBuiltin(pauser); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "wait_here", `{}`), call("toolu_e", "echo", `{"text":"x"}`)))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopToolResult {
		t.Fatalf("outcome %+v", out)
	}
	for _, r := range e.parentResults(t) {
		if r.ToolUseID == "toolu_w" {
			t.Fatal("a paused call got a result")
		}
	}

	l := setup(t, nil)
	for _, p := range []bool{false, true} {
		name := fmt.Sprintf("lose_%v", p)
		lose := &fakeTool{name: name, props: tools.Properties{Parallel: p, Effect: tools.EffectNone}, run: func(tools.Call) (tools.Result, error) {
			return tools.Result{}, &appendError{session.ErrLocked}
		}}
		if err := l.cfg.Tools.AddBuiltin(lose); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"lose_false", "lose_true"} {
		l.stub.Script(model, reply(ir.StopToolUse, call("toolu_"+name, name, `{}`)))
		l.send(ctx, "Go.")
		s, err := l.store.Get(ctx, l.s.ID)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := l.store.Events(ctx, l.s.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.h.RunTurn(ctx, s, evs, l.log); !errors.Is(err, session.ErrLocked) {
			t.Fatalf("%s: a lost append inside a call: %v", name, err)
		}
		st, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopError}, t0)
		if err != nil {
			t.Fatal(err)
		}
		l.appendEvents(ctx, st)
	}
}

const advisorModel = "advisor-model"

func withAdvisor(c *Config) {
	conn := c.Connection
	conn.Model = advisorModel
	c.Name = "builder"
	c.Advisor = &Subagent{Connection: &conn}
}

func TestTheAdvisorSeesTheConversationAndActsOnNothing(t *testing.T) {
	e := setup(t, withAdvisor)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"probe"}`)),
		reply(ir.StopToolUse, call("toolu_a1", ToolAdvisor, `{"question":"Is the probe enough?"}`)),
		reply(ir.StopToolUse, call("toolu_a2", ToolAdvisor, `{}`)),
		reply(ir.StopEndTurn, text("Following the advice.")),
	)
	e.stub.Script(advisorModel,
		luxstub.Reply{Response: ir.Response{Model: advisorModel, Blocks: []ir.Block{text("Add a second probe.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			if len(r.Tools) != 0 {
				return fmt.Errorf("the advisor holds %d tools", len(r.Tools))
			}
			if !strings.Contains(r.System[1].Text, "You advise another agent") {
				return errors.New("the advisor's instructions are missing")
			}
			first := r.Messages[0].Blocks[0].Text
			if !strings.Contains(first, "Agent called echo") || !strings.Contains(first, "echo probe") || !strings.Contains(first, "The question: Is the probe enough?") {
				return fmt.Errorf("the advisor saw %q", first)
			}
			return nil
		}},
		luxstub.Reply{Response: ir.Response{Model: advisorModel, Blocks: []ir.Block{text("Looks right now.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1].Blocks
			if !strings.Contains(last[len(last)-1].Text, "Review the work so far.") || len(r.Messages) < 3 {
				return fmt.Errorf("the second call did not continue the advisor thread: %+v", r.Messages)
			}
			return nil
		}},
	)
	e.send(ctx, "Probe it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	byID := map[string]session.ToolResult{}
	for _, r := range e.parentResults(t) {
		byID[r.ToolUseID] = r
	}
	if byID["toolu_a1"].Content[0].Text != "Add a second probe." || byID["toolu_a2"].Content[0].Text != "Looks right now." {
		t.Fatalf("advice %+v", byID)
	}
	starts := 0
	for _, ev := range e.all() {
		if ev.Type == session.TypeThreadStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("%d advisor threads; the second call reuses the first", starts)
	}
}

func TestAnAdvisorCallResumes(t *testing.T) {
	e := setup(t, withAdvisor)
	ctx := t.Context()
	e.send(ctx, "Go.")
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_a", Name: ToolAdvisor, Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_a", Name: ToolAdvisor, Input: json.RawMessage(`{}`), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	started, err := session.NewEvent(session.TypeThreadStarted, session.ThreadStarted{Agent: session.AgentRef{Name: advisorAgent}, ToolUseID: "toolu_a", Task: "Review.", Depth: 1, Tools: []string{}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	started.Thread = started.ID
	e.appendEvents(ctx, msg, use, started)
	e.running(ctx)
	e.stub.Script(advisorModel, reply(ir.StopEndTurn, text("resumed advice")))
	e.stub.Script(model, reply(ir.StopEndTurn, text("done")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := e.parentResults(t); len(res) != 1 || res[0].Content[0].Text != "resumed advice" {
		t.Fatalf("results %+v", res)
	}
}

func TestSubagentsDoNotInheritTheAdvisor(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(nil)(c)
		withAdvisor(c)
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t"}`)), reply(ir.StopEndTurn, text("ok")))
	e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("done")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		for _, tool := range r.Tools {
			if tool.Name == ToolAdvisor {
				return errors.New("a subagent inherited the advisor")
			}
		}
		return nil
	}})
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if !strings.Contains(renderTranscript(session.Transcript{Messages: []lux.Message{{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockToolResult, ToolResult: &lux.ToolResult{Blocks: []lux.Block{{Type: ir.BlockText, Text: strings.Repeat("x", 5000)}}}}}}}}), "[cut]") {
		t.Fatal("a long result was not cut")
	}
}

func TestASubagentsEffortReachesItsRequests(t *testing.T) {
	for want, sub := range map[string]string{"low": "low", "high": ""} {
		e := setup(t, func(c *Config) {
			withReviewer(func(s *Subagent) { s.Effort = sub })(c)
			c.Effort = "high"
		})
		ctx := t.Context()
		e.stub.Script(model,
			luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Review."}`)}, StopReason: ir.StopToolUse}},
			reply(ir.StopEndTurn, text("done")),
		)
		e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("fine")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			if r.Reasoning == nil || string(r.Reasoning.Effort) != want {
				return fmt.Errorf("the reviewer's effort is %+v, want %s", r.Reasoning, want)
			}
			return nil
		}})
		e.send(ctx, "Build it.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("want %s: %+v", want, out)
		}
	}
}

// TestASubagentActsWithTheSessionsCredentials: a subagent whose agent
// declares its own identity and permissions acts as a thread of the
// session, and its thread.started names what it ignores (spec 013).
func TestASubagentActsWithTheSessionsCredentials(t *testing.T) {
	e := setup(t, withReviewer(func(s *Subagent) { s.Ignored = []string{"identity", "permissions"} }))
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review."}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("Fine.")))
	e.send(ctx, "Review it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if _, started := e.thread(e.all()); !slices.Equal(started.Ignored, []string{"identity", "permissions"}) {
		t.Fatalf("thread.started ignored %v", started.Ignored)
	}
}
