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
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// toolNames are the names of the tools a request offers.
func toolNames(r *ir.Request) []string {
	names := make([]string, 0, len(r.Tools))
	for _, tool := range r.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// usedNames are the names the thread's agent.tool_use events record, in
// order.
func (e *env) usedNames(thread string) []string {
	e.t.Helper()
	var out []string
	for _, ev := range e.all() {
		if ev.Type != session.TypeAgentToolUse || ev.Thread != thread {
			continue
		}
		var p session.AgentToolUse
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, p.Name)
	}
	return out
}

// TestTheAgentAndSendMessageTools: a thread with subagents is offered
// agent, with subagent, task, isolation, tools and budget, and
// send_message, with to, message and end, each described by its text.
func TestTheAgentAndSendMessageTools(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	inputs := func(d ir.Tool) ([]string, []string, error) {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(schema.Properties))
		for n := range schema.Properties {
			names = append(names, n)
		}
		slices.Sort(names)
		return names, schema.Required, nil
	}
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("ok")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		want := map[string]struct {
			inputs, required []string
			description      string
		}{
			ToolAgent:       {[]string{"budget", "isolation", "subagent", "task", "tools"}, []string{"subagent", "task"}, prompts.Text(prompts.ToolAgent)},
			ToolSendMessage: {[]string{"end", "message", "to"}, []string{"to", "message"}, prompts.Text(prompts.ToolSendMessage)},
		}
		for _, d := range r.Tools {
			w, ok := want[d.Name]
			if !ok {
				continue
			}
			delete(want, d.Name)
			got, required, err := inputs(d)
			if err != nil {
				return err
			}
			if !slices.Equal(got, w.inputs) || !slices.Equal(required, w.required) || d.Description != w.description {
				return fmt.Errorf("%s takes %v, requires %v, described %q", d.Name, got, required, d.Description)
			}
		}
		if len(want) > 0 {
			return fmt.Errorf("the thread is not offered %v: %v", want, toolNames(r))
		}
		return nil
	}})
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
}

// TestTheOldToolNamesRunForOneRelease: a thread with subagents is offered
// neither name the tools had before; a call of spawn with agent runs
// agent with subagent, and a call of message with thread and content runs
// send_message with to and message, each recorded as the model wrote it;
// and a call of either old name by a thread that holds neither tool is an
// unknown tool.
func TestTheOldToolNamesRunForOneRelease(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_s", ToolSpawn, `{"agent":"reviewer","task":"Review main.go."}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			names := toolNames(r)
			if !slices.Contains(names, ToolAgent) || !slices.Contains(names, ToolSendMessage) || slices.Contains(names, ToolSpawn) || slices.Contains(names, ToolMessage) {
				return fmt.Errorf("the parent is offered %v, want agent and send_message", names)
			}
			return nil
		}},
		luxstub.Reply{Response: ir.Response{Model: model, StopReason: ir.StopToolUse}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_m", ToolMessage, `{"thread":"`+id+`","content":"Check line 9 too."}`)}
		}},
		reply(ir.StopEndTurn, text("Reviewed.")),
	)
	e.stub.Script(reviewerModel,
		luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Line 3 leaks a file.")}, StopReason: ir.StopEndTurn}},
		luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Line 9 is fine.")}, StopReason: ir.StopEndTurn}},
	)
	e.send(ctx, "Build and review.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.usedNames(""); !slices.Equal(got, []string{ToolSpawn, ToolMessage}) {
		t.Fatalf("the calls are recorded as %v, want spawn then message as the model wrote them", got)
	}
	id, started := e.thread(e.all())
	if started.Agent.Name != "reviewer" || started.Task != "Review main.go." {
		t.Fatalf("thread.started %+v", started)
	}
	for _, ev := range e.all() {
		var p session.ThreadMessage
		if ev.Type == session.TypeThreadMessage && (ev.Decode(&p) != nil || p.To != id || p.Content[0].Text != "Check line 9 too.") {
			t.Fatalf("thread.message %+v", p)
		}
	}
	res := e.parentResults(t)
	if len(res) != 2 || res[0].Outcome != tools.OutcomeOK || !strings.Contains(res[0].Content[0].Text, "Subagent "+id+" is idle; send it more work with send_message.") ||
		res[1].Outcome != tools.OutcomeOK || !strings.Contains(res[1].Content[0].Text, "Line 9 is fine.") {
		t.Fatalf("results %+v", res)
	}

	// Without subagents the thread holds neither tool, so neither old name
	// stands for anything.
	plain := setup(t, nil)
	plain.stub.Script(model, reply(ir.StopToolUse, call("toolu_x", ToolSpawn, `{"agent":"reviewer","task":"t"}`)), reply(ir.StopEndTurn, text("ok")))
	plain.send(ctx, "Go.")
	if out := plain.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := plain.parentResults(t); len(res) != 1 || res[0].Outcome != tools.OutcomeUnknownTool || !strings.Contains(res[0].Content[0].Text, "No tool named spawn.") {
		t.Fatalf("a spawn call with no subagents %+v", res)
	}
}

// TestAToolOfAnOldNameIsItsOwn: a client tool named message, which an
// agent may hold beside its subagents, is called as itself and waits for
// its client; the agent's send_message is offered beside it.
func TestAToolOfAnOldNameIsItsOwn(t *testing.T) {
	e := setup(t, withReviewer(nil))
	client := &fakeTool{name: ToolMessage, props: tools.Properties{Client: true, Effect: tools.EffectNone}}
	if err := e.cfg.Tools.Add(client); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{call("toolu_c", ToolMessage, `{"text":"hi"}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
		if names := toolNames(r); !slices.Contains(names, ToolMessage) || !slices.Contains(names, ToolSendMessage) {
			return fmt.Errorf("the thread is offered %v, want the client's message beside send_message", names)
		}
		return nil
	}})
	e.send(ctx, "Say hi.")
	if out := e.turn(ctx); out.StopReason != session.StopToolResult {
		t.Fatalf("outcome %+v, want the turn waiting for the client", out)
	}
	if got := e.usedNames(""); !slices.Equal(got, []string{ToolMessage}) {
		t.Fatalf("the call is recorded as %v, want the client's message", got)
	}
}

// TestACallRecordedUnderAnOldNameResumes: a spawn whose thread finished
// before the runner stopped, recorded by a release that named the tool
// spawn, yields the thread's final text on resume without running it
// again.
func TestACallRecordedUnderAnOldNameResumes(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.send(ctx, "Go.")
	input := json.RawMessage(`{"agent":"reviewer","task":"t"}`)
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_s", Name: ToolSpawn, Args: input}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_s", Name: ToolSpawn, Input: input, Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	started, err := session.NewEvent(session.TypeThreadStarted, session.ThreadStarted{Agent: session.AgentRef{Name: "reviewer"}, ToolUseID: "toolu_s", Task: "t", Depth: 1, Model: reviewerModel, Tools: []string{"echo"}}, t0)
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
	if res := e.parentResults(t); len(res) != 1 || res[0].Outcome != tools.OutcomeOK || !strings.Contains(res[0].Content[0].Text, "Already reviewed.") {
		t.Fatalf("result %+v", res)
	}
}

// TestAPatternOfAnOldNameMatchesTheRenamedTool: a list an agent stored
// with spawn or message holds the tools of those names now.
func TestAPatternOfAnOldNameMatchesTheRenamedTool(t *testing.T) {
	props := tools.Properties{Parallel: true, Effect: tools.EffectNone}
	for _, c := range []struct{ pattern, call string }{{ToolSpawn, ToolAgent}, {ToolMessage, ToolSendMessage}, {ToolAgent, ToolAgent}} {
		p := Policy{Mode: ModeConfirm, AlwaysConfirm: []string{c.pattern}}
		if d := p.Decide(c.call, props, json.RawMessage(`{}`), session.Risk{}, "", nil); d.Verdict != VerdictAsk {
			t.Errorf("always_confirm %s decided a call of %s %s, want ask", c.pattern, c.call, d.Verdict)
		}
	}
	p := Policy{Mode: ModeConfirm, AlwaysConfirm: []string{ToolSpawn}}
	if d := p.Decide(ToolSendMessage, props, json.RawMessage(`{}`), session.Risk{}, "", nil); d.Verdict != VerdictAllow {
		t.Errorf("always_confirm spawn decided a call of send_message %s", d.Verdict)
	}
}

// TestAConfirmedCallOfAnOldNameRuns: an always_confirm list that names
// spawn holds a call of spawn for the person, recorded as the model wrote
// it, and once the person allows it, the call runs as agent, its agent
// input read as subagent.
func TestAConfirmedCallOfAnOldNameRuns(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(nil)(c)
		c.Policy.AlwaysConfirm = []string{ToolSpawn}
	})
	ctx := t.Context()
	written := `{"agent":"reviewer","task":"Review main.go."}`
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_s", ToolSpawn, written)), reply(ir.StopEndTurn, text("Reviewed.")))
	e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Line 3 leaks a file.")}, StopReason: ir.StopEndTurn}})
	e.send(ctx, "Review.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want the spawn call held for the person", out)
	}
	uses := e.events(ctx, session.TypeAgentToolUse)
	var use session.AgentToolUse
	if len(uses) != 1 || uses[0].Decode(&use) != nil || use.Name != ToolSpawn || string(use.Input) != written || use.Verdict != string(VerdictAsk) {
		t.Fatalf("agent.tool_use %+v", use)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_s", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmation %+v", out)
	}
	_, started := e.thread(e.all())
	if started.Agent.Name != "reviewer" || started.Task != "Review main.go." {
		t.Fatalf("thread.started %+v", started)
	}
	if res := e.parentResults(t); len(res) != 1 || res[0].Outcome != tools.OutcomeOK || !strings.Contains(res[0].Content[0].Text, "Line 3 leaks a file.") {
		t.Fatalf("results %+v", res)
	}
}

// helperSub is a subagent that names no model, connection, figures or
// effort: its thread runs its caller's.
func helperSub(c *Config) {
	c.Name = "lead"
	c.Subagents = map[string]Subagent{"helper": {Name: "helper", Instructions: "You help.", Tools: []string{"echo"}}}
}

// onModel is r answered by the model name, with usage to price.
func onModel(name string, r luxstub.Reply) luxstub.Reply {
	r.Response.Model = name
	r.Response.Usage = ir.Usage{InputTokens: 100, OutputTokens: 10}
	return r
}

// helperAnswer is the helper's answer, to a request that must hold the
// helper's tools and instructions and run at effort.
func helperAnswer(answer, effort string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{text(answer)}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		if names := toolNames(r); !slices.Equal(names, []string{"echo"}) {
			return fmt.Errorf("the helper holds %v, want only echo", names)
		}
		if !strings.Contains(r.System[1].Text, "You help.") {
			return errors.New("the helper's instructions are missing")
		}
		if r.Reasoning == nil || string(r.Reasoning.Effort) != effort {
			return fmt.Errorf("the helper's effort is %+v, want its caller's %s", r.Reasoning, effort)
		}
		return nil
	}}
}

// helperModels are the models the helper's thread's model.requests
// record, in order.
func (e *env) helperModels(ctx context.Context, thread string) []string {
	e.t.Helper()
	var out []string
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := ev.Decode(&p); err != nil {
			e.t.Fatal(err)
		}
		if ev.Thread == thread {
			out = append(out, p.Model)
		}
	}
	return out
}

// TestASubagentWithoutAModelRunsItsCallers: a subagent that names no
// model runs the model its caller stands on when it starts, here the
// model the authorizer routed the session's routed name to, at the
// caller's effort and priced by that model's figures, which the session's
// meter counts; the routed name itself is never connected.
func TestASubagentWithoutAModelRunsItsCallers(t *testing.T) {
	const asked, routed = "tier/quick", "routed-model"
	var connected []string
	e := setup(t, func(c *Config) {
		helperSub(c)
		c.Effort = "low"
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			connected = append(connected, name)
			return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: name, Family: models.FamilyAnthropic},
				models.Entry{Name: name, InputWindow: 30_000, MaxOutputTokens: 2_000, Pricing: &models.Pricing{Input: price("2"), Output: price("10")}}, nil
		}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("One.")))
	e.send(ctx, "One.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	by := session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService}
	changed, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: by,
		Old: session.ModelRef{Name: model, Via: asked, Effort: "low"}, New: session.ModelRef{Name: routed, Via: asked, Effort: "low"}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, changed)

	e.stub.Script(routed,
		onModel(routed, luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{call("toolu_a", ToolAgent, `{"subagent":"helper","task":"Count the files."}`)}, StopReason: ir.StopToolUse}}),
		onModel(routed, helperAnswer("Three files.", "low")),
		onModel(routed, luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{text("Two.")}, StopReason: ir.StopEndTurn}}),
	)
	e.send(ctx, "Two.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	id, started := e.thread(e.all())
	if started.Model != routed || started.Agent.Name != "helper" {
		t.Fatalf("thread.started %+v, want the helper on %s", started, routed)
	}
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := ev.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if ev.Thread == id && (p.Model != routed || p.CostUSDMicro == nil || *p.CostUSDMicro != 300) {
			t.Fatalf("the helper's model.request %+v, want %s priced at 300 micro-USD", p, routed)
		}
	}
	if ran := e.helperModels(ctx, id); !slices.Equal(ran, []string{routed}) {
		t.Fatalf("the helper ran %v", ran)
	}
	// The session's meter counts the helper's request beside its caller's
	// three: one on the agent's own model, priced at 1 and 5 per million,
	// and two on the routed model.
	if spent := session.Spent(e.all()); spent != 150+300+300+300 {
		t.Fatalf("the session spent %d", spent)
	}
	if slices.Contains(connected, asked) {
		t.Fatalf("the routed name was connected: %v", connected)
	}
}

// TestASubagentWithoutAModelFollowsItsCaller: a subagent that names no
// model runs each later turn on the model, connection and effort its
// caller stands on then (spec 091). After a failover within the caller's
// turn, a message to it runs on the model the turn moved to, at the level
// the move named; after a switch of the session between turns, on the
// model switched to, which the session's key now lists alone. Each
// model.request records the model it ran, and no model the subagent ran
// before is connected again.
func TestASubagentWithoutAModelFollowsItsCaller(t *testing.T) {
	const moved, switched = "moved-model", "switched-model"
	r := &router{next: []session.ModelRef{{Name: moved, Via: via, Effort: "high"}}}
	var connected []string
	listed := map[string]bool{moved: true, switched: true}
	e := setupSession(t, func(c *Config) {
		helperSub(c)
		c.Effort = "low"
		c.Failover = r.failover
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			connected = append(connected, name)
			if !listed[name] {
				return nil, models.Connection{}, models.Entry{}, &models.Coded{Code: models.CodeUnavailable, Message: "the session's key does not list " + name}
			}
			return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: name, Family: models.FamilyAnthropic},
				models.Entry{Name: name, InputWindow: 30_000, MaxOutputTokens: 2_000}, nil
		}
	}, func(s *session.Session) { s.Model = &session.ModelRef{Name: model, Via: via, Effort: "low"} })
	ctx := t.Context()
	message := func(id, text string) luxstub.Reply {
		return luxstub.Reply{Response: ir.Response{StopReason: ir.StopToolUse}, Respond: func(_ *ir.Request, r *ir.Response) {
			thread, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call(id, ToolSendMessage, `{"to":"`+thread+`","message":"`+text+`"}`)}
		}}
	}

	// The caller starts the helper on its model; its next request fails,
	// and the turn moves to another model, from which it messages the
	// helper.
	e.stub.Script(model,
		onModel(model, luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{call("toolu_a", ToolAgent, `{"subagent":"helper","task":"Count the files."}`)}, StopReason: ir.StopToolUse}}),
		onModel(model, helperAnswer("Three files.", "low")),
		limitedReply(model),
	)
	e.stub.Script(moved,
		onModel(moved, message("toolu_m", "Count again.")),
		onModel(moved, helperAnswer("Still three.", "high")),
		onModel(moved, luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{text("Moved.")}, StopReason: ir.StopEndTurn}}),
	)
	e.send(ctx, "One.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	id, started := e.thread(e.all())
	if started.Model != model {
		t.Fatalf("thread.started %+v, want the helper started on %s", started, model)
	}
	if ran := e.helperModels(ctx, id); !slices.Equal(ran, []string{model, moved}) {
		t.Fatalf("the helper ran %v, want %s and then %s", ran, model, moved)
	}

	// The person switches the session to a model its key now lists alone;
	// the helper follows, and neither model it ran before is connected.
	listed[moved] = false
	e.switchTo(ctx, switched)
	e.stub.Script(switched,
		onModel(switched, message("toolu_n", "Once more.")),
		onModel(switched, helperAnswer("Three again.", "low")),
		onModel(switched, luxstub.Reply{Response: ir.Response{Blocks: []ir.Block{text("Switched.")}, StopReason: ir.StopEndTurn}}),
	)
	e.send(ctx, "Two.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if ran := e.helperModels(ctx, id); !slices.Equal(ran, []string{model, moved, switched}) {
		t.Fatalf("the helper ran %v", ran)
	}
	res := e.parentResults(t)
	if len(res) != 3 || res[1].Outcome != tools.OutcomeOK || !strings.Contains(res[1].Content[0].Text, "Still three.") || res[2].Outcome != tools.OutcomeOK || !strings.Contains(res[2].Content[0].Text, "Three again.") {
		t.Fatalf("results %+v", res)
	}
	if !slices.Equal(connected, []string{moved, switched}) {
		t.Fatalf("connected %v, want only the models the caller moved to", connected)
	}
}
