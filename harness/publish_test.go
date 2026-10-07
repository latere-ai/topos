// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// systemText is a request's system prompt as one text.
func systemText(r *ir.Request) string {
	var b strings.Builder
	for _, block := range r.System {
		b.WriteString(block.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestTheRequestNamesTheRoute: every request of a session that runs by a
// routed name carries that name as a system part, and a session that
// runs by none carries no such part (spec 043).
func TestTheRequestNamesTheRoute(t *testing.T) {
	for name, c := range map[string]struct {
		model *session.ModelRef
		want  bool
	}{
		"routed":   {&session.ModelRef{Name: model, Via: "tier/quick"}, true},
		"unrouted": {&session.ModelRef{Name: model}, false},
		"no model": {nil, false},
	} {
		e := setupSession(t, nil, func(s *session.Session) { s.Model = c.model })
		ctx := t.Context()
		e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("ok")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			got := strings.Contains(systemText(r), "<context>\nModel route: tier/quick\n</context>")
			if got != c.want {
				return fmt.Errorf("the system prompt names the route: %v, want %v:\n%s", got, c.want, systemText(r))
			}
			return nil
		}})
		e.send(ctx, "Hi.")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("%s: outcome %+v", name, out)
		}
	}
}

// TestASpawnedThreadHoldsNoPublish: the session's own thread holds the
// publish tool its registry carries, and a thread a spawn starts holds
// none, whatever its subagent names, so a session has one app (spec 043).
func TestASpawnedThreadHoldsNoPublish(t *testing.T) {
	pub := &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", session.ToolPublish} })(c)
	})
	ctx := t.Context()
	e.stub.Script(model,
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{spawnCall("toolu_s", `{"agent":"reviewer","task":"Publish it.","tools":["echo","publish"]}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			if names := offered(r); !slices.Contains(names, session.ToolPublish) {
				return fmt.Errorf("the session's own thread is offered %v", names)
			}
			return nil
		}},
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel, luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{text("Fine.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		if names := offered(r); !slices.Equal(names, []string{"echo"}) {
			return fmt.Errorf("a spawned thread is offered %v, want echo alone", names)
		}
		return nil
	}})
	e.send(ctx, "Publish.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
}

// TestAPublishCallAsksInConfirm: a publish call on a Cella machine in
// confirm mode waits for a person, a release included, since its effect
// reaches outside the sandbox: the session goes idle tool_confirmation
// with the call recorded ask, and the tool does not run (spec 043).
func TestAPublishCallAsksInConfirm(t *testing.T) {
	pub := &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal},
		schema: `{"type":"object","properties":{"path":{"type":"string"},"release":{"type":"boolean"}},"additionalProperties":false}`}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_r", session.ToolPublish, `{"release":true}`)))
	e.send(ctx, "Release it.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want a wait for the person", out)
	}
	var use session.AgentToolUse
	for _, ev := range e.all() {
		if ev.Type == session.TypeAgentToolUse {
			if err := ev.Decode(&use); err != nil {
				t.Fatal(err)
			}
		}
	}
	if use.Name != session.ToolPublish || use.Verdict != string(VerdictAsk) {
		t.Fatalf("the call is recorded %+v, want publish asked", use)
	}
	for _, ev := range e.all() {
		if ev.Type == session.TypeToolResult {
			t.Fatalf("the call ran before the person answered: %s", ev.Payload)
		}
	}
}

// checkingTool is a fake tool of an external effect whose Check refuses
// an input of text "nope", recording each state it was handed.
type checkingTool struct {
	*fakeTool
	mu     sync.Mutex
	checks []string
}

func (c *checkingTool) Check(call tools.Call) *tools.Result {
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(call.Input, &in); err != nil {
		return nil
	}
	c.mu.Lock()
	c.checks = append(c.checks, call.ID)
	c.mu.Unlock()
	if in.Text != "nope" {
		return nil
	}
	res := tools.Text(tools.OutcomeError, "refused: nope")
	return &res
}

func (c *checkingTool) checked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.checks)
}

// TestACallRefusedOnItsInputIsNotAsked: in confirm mode, a call its tool
// refuses on its input is answered with the refusal before it is
// decided: it gets no agent.tool_use, the session does not wait for a
// person, the tool does not run, and the model reads the refusal in the
// next step. A call the check passes is asked as before, and a call after
// another of the same tool in its step is not checked, since the earlier
// call's result may change what it would be refused for (spec 012).
func TestACallRefusedOnItsInputIsNotAsked(t *testing.T) {
	pub := &checkingTool{fakeTool: &fakeTool{name: session.ToolPublish, props: tools.Properties{Effect: tools.EffectExternal}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(pub); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", session.ToolPublish, `{"text":"nope"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("It was refused.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 1 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.ToolUseID != "toolu_1" || !last.Blocks[0].ToolResult.IsError {
				return fmt.Errorf("the model was not sent the refusal: %+v", last)
			}
			return nil
		}},
	)
	e.send(ctx, "Publish it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v, want the turn to end without a person", out)
	}
	var results []session.ToolResult
	for _, ev := range e.all() {
		switch ev.Type {
		case session.TypeAgentToolUse:
			t.Fatalf("a refused call was recorded for a decision: %s", ev.Payload)
		case session.TypeToolResult:
			var p session.ToolResult
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			results = append(results, p)
		}
	}
	if len(results) != 1 || results[0].ToolUseID != "toolu_1" || !results[0].IsError || results[0].Outcome != tools.OutcomeError || results[0].Content[0].Text != "refused: nope" {
		t.Fatalf("the results %+v", results)
	}
	if ran := pub.ran(); len(ran) != 0 {
		t.Fatalf("a refused call ran: %v", ran)
	}

	// A refused call leaves the next call of its tool to be checked; that
	// one passes and is asked, and the one after it is asked unchecked.
	e.stub.Script(model, reply(ir.StopToolUse,
		call("toolu_2", session.ToolPublish, `{"text":"nope"}`),
		call("toolu_3", session.ToolPublish, `{"text":"ok"}`),
		call("toolu_4", session.ToolPublish, `{"text":"nope"}`)))
	e.send(ctx, "Publish both.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want a wait for the person", out)
	}
	if got := pub.checked(); !slices.Equal(got, []string{"toolu_1", "toolu_2", "toolu_3"}) {
		t.Fatalf("checked %v, want toolu_1, toolu_2 and toolu_3", got)
	}
	asked := map[string]string{}
	for _, ev := range e.all() {
		if ev.Type == session.TypeAgentToolUse {
			var use session.AgentToolUse
			if err := ev.Decode(&use); err != nil {
				t.Fatal(err)
			}
			asked[use.ToolUseID] = use.Verdict
		}
	}
	if len(asked) != 2 || asked["toolu_3"] != string(VerdictAsk) || asked["toolu_4"] != string(VerdictAsk) {
		t.Fatalf("the decided calls %v, want toolu_3 and toolu_4 asked", asked)
	}
}
