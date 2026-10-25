// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// TestASubagentMayLeaveItsModelOut: an inline subagent with no model
// resolves, its stored spec holds no model, and its harness subagent
// names no model, connection, figures or effort, so its thread runs its
// caller's; nothing is connected for it. A subagent that names a model is
// connected by that name as before; a model given without a name, and a
// referenced agent with no model, are refused.
func TestASubagentMayLeaveItsModelOut(t *testing.T) {
	r := one(t, agent("lead",
		"model: {name: tier/quick, reasoning: low}",
		"subagents: [{name: helper, spec: {instructions: Help., tools: [read]}}, {name: scout, spec: {model: {name: s}, instructions: Look.}}]",
	), fixed(nil))
	if strings.Contains(string(r.Spec), `"name":""`) {
		t.Fatalf("the stored spec holds an empty model: %s", r.Spec)
	}
	if m := r.Agent.Spec.Subagents[0].Spec.Model; m != (v1.AgentModel{}) {
		t.Fatalf("the helper's resolved model is %+v", m)
	}
	var connected []string
	c, err := r.AgentConfig(func(m v1.AgentModel, overlay models.Entry) (models.Model, *models.Connection, *models.Entry, error) {
		connected = append(connected, m.Name)
		return nil, &models.Connection{BaseURL: "https://models.example.com", Model: m.Name}, &overlay, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	helper, scout := c.Subagents["helper"], c.Subagents["scout"]
	if helper.Model != nil || helper.Connection != nil || helper.Entry != nil || helper.Effort != "" || helper.Instructions != "Help." || len(helper.Tools) != 1 {
		t.Fatalf("helper %+v", helper)
	}
	if scout.Connection == nil || scout.Connection.Model != "s" || scout.Entry == nil || scout.Entry.Name != "s" {
		t.Fatalf("scout %+v", scout)
	}
	if len(connected) != 1 || connected[0] != "s" {
		t.Fatalf("connected %v, want the scout's model alone", connected)
	}

	e := refused(t, CodeInvalidManifest, agent("lead", "model: {name: m}", "subagents: [{name: helper, spec: {model: {reasoning: low}}}]"), fixed(nil))
	if !hasProblem(e, "spec.subagents[0].spec.model.name", "leave model out") {
		t.Fatalf("a model with no name: %+v", e.Problems)
	}
	e = refused(t, CodeInvalidManifest, agent("lead", "subagents: [{name: helper, spec: {instructions: Help.}}]"), fixed(nil))
	if !hasProblem(e, "spec.model.name", "required") {
		t.Fatalf("an agent with no model: %+v", e.Problems)
	}
	e = refused(t, CodeInvalidManifest, agent("lead", "model: {name: m}", "subagents: [{name: w, agent: worker}]")+"---\n"+agent("worker", "instructions: Work."), fixed(nil))
	if !hasProblem(e, "spec.model.name", "required") {
		t.Fatalf("a referenced agent with no model: %+v", e.Problems)
	}
}

// TestAPatternNamingSpawnMatchesAgent: an approval list stored with the
// names the subagent tools had before, spawn and message, decides the
// calls of agent and send_message, and a hook's matcher of either name
// is a pattern as any other.
func TestAPatternNamingSpawnMatchesAgent(t *testing.T) {
	r := one(t, agent("lead",
		"model: {name: m}",
		"approvals: {mode: confirm, alwaysConfirm: [spawn, message]}",
		"hooks: [{event: pre_tool_use, matcher: spawn, command: 'true'}]",
		"subagents: [{name: helper, spec: {model: {name: m}, instructions: Help.}}]",
	), fixed(nil))
	c, err := r.AgentConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	props := tools.Properties{Parallel: true, Effect: tools.EffectNone}
	for _, name := range []string{harness.ToolAgent, harness.ToolSendMessage} {
		if d := c.Policy.Decide(name, props, json.RawMessage(`{}`), session.Risk{}, "", nil); d.Verdict != harness.VerdictAsk {
			t.Errorf("a call of %s is decided %s, want ask", name, d.Verdict)
		}
	}
}

// TestAClientToolMayNotTakeASubagentToolsName: an agent with subagents
// holds agent and send_message, so neither is a client tool's name there;
// an agent without subagents may name a client tool either.
func TestAClientToolMayNotTakeASubagentToolsName(t *testing.T) {
	client := func(name string) string {
		return "{name: " + name + ", client: true, description: Send., inputSchema: {type: object}}"
	}
	e := refused(t, CodeInvalidManifest, agent("lead", "model: {name: m}", "tools: [read, "+client("agent")+", "+client("send_message")+"]", "subagents: [{name: helper, spec: {model: {name: m}, instructions: Help.}}]"), fixed(nil))
	if !hasProblem(e, "spec.tools[1].name", "rename the client tool") || !hasProblem(e, "spec.tools[2].name", "rename the client tool") {
		t.Fatalf("problems %+v", e.Problems)
	}
	one(t, agent("lead", "model: {name: m}", "tools: [read, "+client("send_message")+"]"), fixed(nil))
}
