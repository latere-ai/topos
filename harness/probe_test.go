// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// probingTool is a fake read-only tool that holds every call for a person
// and refuses, by asking its service, each call whose text is "refused".
type probingTool struct {
	*fakeTool
	probed []string
}

// probedRefusal is the text probingTool answers a refused call with.
const probedRefusal = "the service would refuse it"

func (p *probingTool) Confirm(tools.Call) string { return heldReason }

func (p *probingTool) Probe(_ context.Context, c tools.Call) *tools.Result {
	p.probed = append(p.probed, c.ID)
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil || in.Text != "refused" {
		return nil
	}
	r := tools.Text(tools.OutcomeInvalidInput, probedRefusal)
	return &r
}

// TestAProbedCallAsksNobody: a call its tool's service would refuse is
// answered with the refusal before it is decided, so it records no
// agent.tool_use, asks nobody and never runs; a call the service would
// take is held for the person as its tool says (spec 086).
func TestAProbedCallAsksNobody(t *testing.T) {
	tool := &probingTool{fakeTool: &fakeTool{name: session.ToolTrigger, props: tools.Properties{Effect: tools.EffectRead}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(tool); err != nil {
			t.Fatal(err)
		}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_no", session.ToolTrigger, `{"text":"refused"}`)), reply(ir.StopToolUse, call("toolu_yes", session.ToolTrigger, `{"text":"fine"}`)))
	e.send(ctx, "Make it a routine.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v, want a wait for the person on the second call", out)
	}
	var uses []string
	for _, ev := range e.all() {
		if ev.Type == session.TypeAgentToolUse {
			var u session.AgentToolUse
			if err := ev.Decode(&u); err != nil {
				t.Fatal(err)
			}
			uses = append(uses, u.ToolUseID+" "+u.Verdict)
		}
	}
	if !slices.Equal(uses, []string{"toolu_yes " + string(VerdictAsk)}) {
		t.Fatalf("the decided calls are %v, want toolu_yes asked alone", uses)
	}
	results := e.events(ctx, session.TypeToolResult)
	if len(results) != 1 {
		t.Fatalf("%d results, want the refused call's", len(results))
	}
	var res session.ToolResult
	if err := results[0].Decode(&res); err != nil || res.ToolUseID != "toolu_no" || res.Outcome != tools.OutcomeInvalidInput || res.Content[0].Text != probedRefusal {
		t.Fatalf("the refused call's result is %+v (%v)", res, err)
	}
	if !slices.Equal(tool.probed, []string{"toolu_no", "toolu_yes"}) || len(tool.ran()) != 0 {
		t.Fatalf("probed %v and ran %v", tool.probed, tool.ran())
	}
}

// TestPlanModeBlocksAHeldCall: a call its tool holds for a person is
// blocked in plan mode though the tool reads, so standing work is never
// even asked for while the agent plans.
func TestPlanModeBlocksAHeldCall(t *testing.T) {
	tool := &probingTool{fakeTool: &fakeTool{name: session.ToolTrigger, props: tools.Properties{Effect: tools.EffectRead}}}
	e := setup(t, func(c *Config) {
		if err := c.Tools.Add(tool); err != nil {
			t.Fatal(err)
		}
		c.Policy = Policy{Mode: ModePlan}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_p", session.ToolTrigger, `{"text":"fine"}`)), reply(ir.StopEndTurn, text("Planned.")))
	e.send(ctx, "Plan a routine.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v, want the turn to end", out)
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeBlocked {
		t.Fatalf("the held call in plan mode answered %+v (%v)", res, err)
	}
	if len(tool.ran()) != 0 {
		t.Fatalf("ran %v", tool.ran())
	}
}
