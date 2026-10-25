// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

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
