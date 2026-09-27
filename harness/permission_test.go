// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"slices"
	"testing"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

func props(e tools.Effect) tools.Properties { return tools.Properties{Effect: e} }

func TestScoreFollowsTheRuleFeatures(t *testing.T) {
	egress := []string{"api.example.com"}
	for _, c := range []struct {
		name, input, kind string
		effect            tools.Effect
		want              float64
	}{
		{"read", `{}`, machine.KindHost, tools.EffectRead, 0},
		{"todo", `{}`, machine.KindHost, tools.EffectNone, 0},
		{"memory_sync", `{}`, machine.KindHost, tools.EffectWrite, 0.1},
		{"write", `{}`, machine.KindCella, tools.EffectWrite, 0.1},
		{"bash", `{"command":"go test ./..."}`, machine.KindCella, tools.EffectWrite, 0.1},
		{"write", `{}`, machine.KindHost, tools.EffectWrite, 0.3},
		{"bash", `{"command":"go test ./..."}`, machine.KindHost, tools.EffectWrite, 0.5},
		{"bash", `{"command":"git push origin main"}`, machine.KindHost, tools.EffectWrite, 0.7},
		{"bash", `{"command":"make && rm -rf build"}`, machine.KindHost, tools.EffectWrite, 0.7},
		{"bash", `{"command":"curl https://x"}`, machine.KindHost, tools.EffectWrite, 0.7},
		{"web_fetch", `{"url":"https://api.example.com/v1"}`, machine.KindHost, tools.EffectExternal, 0.4},
		{"web_fetch", `{"url":"http://API.example.com:8080/"}`, machine.KindHost, tools.EffectExternal, 0.4},
		{"web_fetch", `{"url":"https://elsewhere.example.org"}`, machine.KindHost, tools.EffectExternal, 0.6},
		{"web_fetch", `nope`, machine.KindHost, tools.EffectExternal, 0.6},
		{"custom", `{}`, machine.KindHost, "", 0.6},
	} {
		r := Score(c.name, props(c.effect), json.RawMessage(c.input), c.kind, egress)
		if r.Score != c.want || r.Source != RiskSource || len(r.Features) == 0 {
			t.Fatalf("Score(%s %s on %s) = %+v, want %v", c.name, c.input, c.kind, r, c.want)
		}
	}
}

func TestDecideAppliesTheModeAndTheLists(t *testing.T) {
	low := session.Risk{Score: 0.1}
	mid := session.Risk{Score: 0.4}
	high := session.Risk{Score: 0.6}
	top := session.Risk{Score: 0.95}
	write := props(tools.EffectWrite)
	read := props(tools.EffectRead)
	cmd := json.RawMessage(`{"command":"make deploy"}`)
	path := json.RawMessage(`{"path":"docs/a/b.md"}`)
	for _, c := range []struct {
		name   string
		policy Policy
		tool   string
		props  tools.Properties
		input  json.RawMessage
		risk   session.Risk
		kind   string
		rem    []string
		want   Verdict
	}{
		{"plan read", Policy{Mode: ModePlan}, "read", read, path, low, machine.KindHost, nil, VerdictAllow},
		{"plan write", Policy{Mode: ModePlan}, "write", write, path, low, machine.KindHost, nil, VerdictBlock},
		{"plan confirm list", Policy{Mode: ModePlan, AlwaysConfirm: []string{"read"}}, "read", read, path, low, machine.KindHost, nil, VerdictBlock},
		{"confirm read", Policy{}, "read", read, path, low, machine.KindHost, nil, VerdictAllow},
		{"confirm write", Policy{Mode: ModeConfirm}, "write", write, path, low, machine.KindHost, nil, VerdictAsk},
		{"confirm allow list", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"write(docs/**)"}}, "write", write, path, low, machine.KindHost, nil, VerdictAllow},
		{"confirm allow list miss", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"write(src/*)"}}, "write", write, path, low, machine.KindHost, nil, VerdictAsk},
		{"single star stays in a segment", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"write(docs/*)"}}, "write", write, path, low, machine.KindHost, nil, VerdictAsk},
		{"confirm remembered", Policy{Mode: ModeConfirm}, "bash", write, cmd, low, machine.KindHost, []string{"bash(make *)"}, VerdictAllow},
		{"confirm cella write", Policy{Mode: ModeConfirm}, "bash", write, cmd, low, machine.KindCella, nil, VerdictAllow},
		{"always confirm wins", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"bash"}, AlwaysConfirm: []string{"bash(make deploy*)"}}, "bash", write, cmd, low, machine.KindCella, nil, VerdictAsk},
		{"progressive allow", Policy{Mode: ModeProgressive}, "bash", write, cmd, low, machine.KindHost, nil, VerdictAllow},
		{"progressive flag", Policy{Mode: ModeProgressive}, "bash", write, cmd, mid, machine.KindHost, nil, VerdictFlag},
		{"progressive ask", Policy{Mode: ModeProgressive}, "bash", write, cmd, high, machine.KindHost, nil, VerdictAsk},
		{"progressive block", Policy{Mode: ModeProgressive}, "bash", write, cmd, top, machine.KindHost, nil, VerdictBlock},
		{"progressive thresholds", Policy{Mode: ModeProgressive, Thresholds: Thresholds{FlagAt: 0.05, AskAt: 0.08, BlockAt: 0.09}}, "bash", write, cmd, low, machine.KindHost, nil, VerdictBlock},
		{"progressive confirm list", Policy{Mode: ModeProgressive, AlwaysConfirm: []string{"bash"}}, "bash", write, cmd, low, machine.KindHost, nil, VerdictAsk},
		{"web domain", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"web_fetch(domain:*.example.com)"}}, "web_fetch", props(tools.EffectExternal), json.RawMessage(`{"url":"https://docs.example.com/x"}`), high, machine.KindHost, nil, VerdictAllow},
		{"bad input", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"bash(make*)"}}, "bash", write, json.RawMessage(`[`), low, machine.KindHost, nil, VerdictAsk},
		{"other tool", Policy{Mode: ModeConfirm, AlwaysAllow: []string{"edit"}}, "write", write, path, low, machine.KindHost, nil, VerdictAsk},
	} {
		d := c.policy.Decide(c.tool, c.props, c.input, c.risk, c.kind, c.rem)
		if d.Verdict != c.want || d.Reason == "" {
			t.Fatalf("%s: %+v, want %s", c.name, d, c.want)
		}
	}
}

func TestStricterOrdersVerdicts(t *testing.T) {
	order := []Verdict{VerdictAllow, VerdictFlag, VerdictAsk, VerdictBlock}
	for i, a := range order {
		for j, b := range order {
			want := order[max(i, j)]
			if got := Stricter(a, b); got != want {
				t.Fatalf("Stricter(%s, %s) = %s", a, b, got)
			}
		}
	}
	if !slices.Equal(verdictOrder, order) {
		t.Fatal("the verdict order changed")
	}
}

func TestGlobRegexpQuotesItsText(t *testing.T) {
	if !globRegexp("git status (short)?", false).MatchString("git status (short)x") {
		t.Fatal("a literal pattern with regexp characters")
	}
	if globRegexp("a.b", false).MatchString("axb") {
		t.Fatal("a dot matched any character")
	}
}

// TestProgressiveNeedsASandbox: progressive is refused on a host that
// records no operating-system sandbox, and plan and confirm run there;
// a machine with a sandbox driver, or one that names none, runs every
// mode.
func TestProgressiveNeedsASandbox(t *testing.T) {
	none := machine.Info{Kind: machine.KindHost, OS: "windows", Sandbox: machine.SandboxNone}
	if err := CheckSandbox(ModeProgressive, none); err == nil {
		t.Fatal("progressive ran on a host without a sandbox")
	}
	for _, mode := range []Mode{ModePlan, ModeConfirm, ""} {
		if err := CheckSandbox(mode, none); err != nil {
			t.Errorf("%q on a host without a sandbox: %v", mode, err)
		}
	}
	for _, sandbox := range []string{"host", ""} {
		if err := CheckSandbox(ModeProgressive, machine.Info{Kind: machine.KindHost, Sandbox: sandbox}); err != nil {
			t.Errorf("progressive with sandbox %q: %v", sandbox, err)
		}
	}
}
