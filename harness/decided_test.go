// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// Every decision of the rules names the layer that made it and, where a
// rule applies, the rule: one case per value of session.Decided that the
// rules give. The question, learned and fallback values are the
// harness's and the decision service's, tested where they are made.
func TestEveryDecisionNamesItsLayer(t *testing.T) {
	write := tools.Properties{Effect: tools.EffectWrite}
	read := tools.Properties{Effect: tools.EffectRead}
	bash := func(cmd string) jsontext.Value { b, _ := json.Marshal(map[string]string{"command": cmd}); return b }
	fetch := jsontext.Value(`{"url":"https://elsewhere.example/page"}`)
	allowlist := &Network{Mode: session.NetworkAllowlist, Hosts: []string{"docs.example"}}
	asking := &Network{Mode: session.NetworkAllowlist, Hosts: []string{"docs.example"}, Ask: true}
	cases := []struct {
		name       string
		policy     Policy
		tool       string
		props      tools.Properties
		input      jsontext.Value
		score      float64
		kind       string
		remembered []string
		by, rule   string
		verdict    Verdict
	}{
		{"network blocks", Policy{Mode: ModeConfirm, Network: allowlist}, tools.NameWebFetch, read, fetch, 0, machine.KindHost, nil, session.DecidedNetwork, "", VerdictBlock},
		{"network asks", Policy{Mode: ModeConfirm, Network: asking}, tools.NameWebFetch, read, fetch, 0, machine.KindHost, nil, session.DecidedNetwork, "", VerdictAsk},
		{"always confirm", Policy{Mode: ModeConfirm, AlwaysConfirm: []string{"bash(git push*)"}}, "bash", write, bash("git push origin main"), 0, machine.KindHost, nil, session.DecidedAlwaysConfirm, "bash(git push*)", VerdictAsk},
		{"plan blocks a change", Policy{Mode: ModePlan}, "bash", write, bash("make"), 0, machine.KindHost, nil, session.DecidedPlan, "", VerdictBlock},
		{"read-only in plan", Policy{Mode: ModePlan}, "read", read, jsontext.Value(`{"path":"a"}`), 0, machine.KindHost, nil, session.DecidedReadOnly, "", VerdictAllow},
		{"read-only in confirm", Policy{Mode: ModeConfirm}, "read", read, jsontext.Value(`{"path":"a"}`), 0, machine.KindHost, nil, session.DecidedReadOnly, "", VerdictAllow},
		{"always allow", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"bash(go test*)"}}, "bash", write, bash("go test ./..."), 0, machine.KindHost, nil, session.DecidedAlwaysAllow, "bash(go test*)", VerdictAllow},
		{"remembered", Policy{Mode: ModeConfirm}, "bash", write, bash("make lint"), 0, machine.KindHost, []string{"bash(make*)"}, session.DecidedRemembered, "bash(make*)", VerdictAllow},
		{"sandbox", Policy{Mode: ModeConfirm}, "write", write, jsontext.Value(`{"path":"a"}`), 0, machine.KindCella, nil, session.DecidedSandbox, "", VerdictAllow},
		{"confirm asks", Policy{Mode: ModeConfirm}, "bash", write, bash("make"), 0, machine.KindHost, nil, session.DecidedConfirm, "", VerdictAsk},
		{"score below flag", Policy{Mode: ModeProgressive}, "bash", write, bash("make"), 0.1, machine.KindCella, nil, session.DecidedScore, RuleBelowFlag, VerdictAllow},
		{"score flags", Policy{Mode: ModeProgressive}, "bash", write, bash("make"), 0.4, machine.KindCella, nil, session.DecidedScore, RuleFlagAt, VerdictFlag},
		{"score asks", Policy{Mode: ModeProgressive}, "bash", write, bash("make"), 0.6, machine.KindCella, nil, session.DecidedScore, RuleAskAt, VerdictAsk},
		{"score blocks", Policy{Mode: ModeProgressive}, "bash", write, bash("make"), 0.95, machine.KindCella, nil, session.DecidedScore, RuleBlockAt, VerdictBlock},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		d := c.policy.Decide(c.tool, c.props, c.input, session.Risk{Score: c.score}, c.kind, c.remembered)
		if d.DecidedBy != c.by || d.Rule != c.rule || d.Verdict != c.verdict {
			t.Errorf("%s: decided by %q rule %q verdict %s, want %q %q %s", c.name, d.DecidedBy, d.Rule, d.Verdict, c.by, c.rule, c.verdict)
		}
		seen[d.DecidedBy] = true
	}

	// Memory: its own default, the always-confirm feature as the rule, and
	// plan.
	p := Policy{Mode: ModeConfirm}
	if d := p.decideMemory("write", write, nil, machine.KindHost); d.DecidedBy != session.DecidedMemory || d.Verdict != VerdictAllow {
		t.Errorf("memory: %+v", d)
	}
	seen[session.DecidedMemory] = true
	p.AlwaysConfirm = []string{FeatureMemory}
	if d := p.decideMemory("write", write, nil, machine.KindHost); d.DecidedBy != session.DecidedAlwaysConfirm || d.Rule != FeatureMemory {
		t.Errorf("memory on always-confirm: %+v", d)
	}

	// A tool that holds its call, through held and through Settle.
	if d := held(Decision{Verdict: VerdictAllow, DecidedBy: session.DecidedReadOnly}, "it publishes", "publish"); d.DecidedBy != session.DecidedTool || d.Rule != "publish" || d.Verdict != VerdictAsk {
		t.Errorf("held: %+v", d)
	}
	if d := held(Decision{Verdict: VerdictBlock, DecidedBy: session.DecidedPlan}, "it publishes", "publish"); d.DecidedBy != session.DecidedPlan {
		t.Errorf("a block is stricter than holding: %+v", d)
	}
	seen[session.DecidedTool] = true

	// Settle names the same layers.
	for _, c := range []struct {
		name string
		call Call
		risk float64
		by   string
		rule string
	}{
		{"settle always confirm", Call{Policy: Policy{Mode: ModeProgressive, AlwaysConfirm: []string{"bash(git push*)"}}, Name: "bash", Props: write, Input: bash("git push")}, 0, session.DecidedAlwaysConfirm, "bash(git push*)"},
		{"settle tool", Call{Policy: Policy{Mode: ModeProgressive}, Name: "publish", Props: write, Confirm: "it publishes"}, 0, session.DecidedTool, "publish"},
		{"settle score", Call{Policy: Policy{Mode: ModeProgressive}, Name: "bash", Props: write, Input: bash("make")}, 0.95, session.DecidedScore, RuleBlockAt},
		{"settle read-only", Call{Policy: Policy{Mode: ModeProgressive}, Name: "read", Props: read}, 0, session.DecidedReadOnly, ""},
		{"settle always allow", Call{Policy: Policy{Mode: ModeProgressive, AlwaysAllow: []string{"bash(go test*)"}}, Name: "bash", Props: write, Input: bash("go test")}, 0, session.DecidedAlwaysAllow, "bash(go test*)"},
		{"settle remembered", Call{Policy: Policy{Mode: ModeProgressive}, Name: "bash", Props: write, Input: bash("make lint"), Remembered: []string{"bash(make*)"}}, 0, session.DecidedRemembered, "bash(make*)"},
	} {
		d, ok := c.call.Policy.Settle(c.call, session.Risk{Score: c.risk})
		if !ok || d.DecidedBy != c.by || d.Rule != c.rule {
			t.Errorf("%s: %+v, %v", c.name, d, ok)
		}
	}

	// Rules.Decide carries the layer through for every call.
	_, d, err := Rules{}.Decide(context.Background(), Call{Policy: Policy{Mode: ModeConfirm}, Name: "bash", Props: write, Input: bash("make"), MachineKind: machine.KindHost})
	if err != nil || d.DecidedBy != session.DecidedConfirm {
		t.Errorf("Rules.Decide: %+v, %v", d, err)
	}

	for _, v := range []string{session.DecidedNetwork, session.DecidedAlwaysConfirm, session.DecidedTool, session.DecidedPlan, session.DecidedReadOnly,
		session.DecidedAlwaysAllow, session.DecidedRemembered, session.DecidedSandbox, session.DecidedScore, session.DecidedConfirm, session.DecidedMemory} {
		if !seen[v] {
			t.Errorf("no case decides by %q", v)
		}
		if !slices.Contains(session.Decided, v) {
			t.Errorf("%q is not in session.Decided", v)
		}
	}
}
