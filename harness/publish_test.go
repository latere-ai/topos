// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"fmt"
	"slices"
	"strings"
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
