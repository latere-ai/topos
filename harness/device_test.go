// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// TestDeviceScoresAsHost: a call on a person's own computer scores as a
// call on a host, write 0.3, bash 0.5 and a network or delete command
// 0.7, never as one in a sandbox; in confirm mode a write on a device
// asks where the same write in a Cella sandbox is allowed; and progressive
// is refused, and decided as confirm, on a device whose computer reports
// isolation none (spec 080). It pins the device apart from the Cella
// branch, so a later change there does not reach a computer.
func TestDeviceScoresAsHost(t *testing.T) {
	for _, c := range []struct {
		name, input string
		effect      tools.Effect
		want        float64
	}{
		{"write", `{}`, tools.EffectWrite, 0.3},
		{"bash", `{"command":"go test ./..."}`, tools.EffectWrite, 0.5},
		{"bash", `{"command":"git push origin main"}`, tools.EffectWrite, 0.7},
		{"bash", `{"command":"rm -rf build"}`, tools.EffectWrite, 0.7},
	} {
		device := Score(c.name, props(c.effect), json.RawMessage(c.input), machine.KindDevice, nil)
		host := Score(c.name, props(c.effect), json.RawMessage(c.input), machine.KindHost, nil)
		if device.Score != c.want || device.Score != host.Score {
			t.Errorf("%s %s scores %v on a device and %v on a host, want %v for both", c.name, c.input, device.Score, host.Score, c.want)
		}
	}
	write := json.RawMessage(`{"path":"main.go","content":"x"}`)
	low := session.Risk{Score: 0.1}
	confirm := Policy{Mode: ModeConfirm}
	if d := confirm.Decide("write", props(tools.EffectWrite), write, low, machine.KindDevice, nil); d.Verdict != VerdictAsk {
		t.Fatalf("a write on a device in confirm: %+v, want ask", d)
	}
	if d := confirm.Decide("write", props(tools.EffectWrite), write, low, machine.KindCella, nil); d.Verdict != VerdictAllow {
		t.Fatalf("a write in a Cella sandbox in confirm: %+v, want allow", d)
	}

	// A turn decides by the kind its machine says, before the machine
	// opens: a device's call asks, a Cella sandbox's runs.
	for kind, want := range map[string]struct {
		verdict Verdict
		stop    session.StopReason
	}{machine.KindDevice: {VerdictAsk, session.StopToolConfirmation}, machine.KindCella: {VerdictAllow, session.StopEndTurn}} {
		e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: kind, sandbox: "process"} })
		ctx := t.Context()
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("Built.")))
		e.send(ctx, "Build.")
		out := e.turn(ctx)
		if u := e.uses(ctx)["toolu_w"]; u.Verdict != string(want.verdict) || out.StopReason != want.stop {
			t.Fatalf("bash on %s in confirm: %+v, outcome %+v", kind, u, out)
		}
	}

	none := machine.Info{Kind: machine.KindDevice, OS: "linux", Sandbox: machine.SandboxNone}
	if err := CheckSandbox(ModeProgressive, none); err == nil {
		t.Fatal("progressive ran on a device whose computer reports no isolation")
	}
	if err := CheckSandbox(ModeProgressive, machine.Info{Kind: machine.KindDevice, Sandbox: "process"}); err != nil {
		t.Fatalf("progressive on a device of process isolation: %v", err)
	}
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindDevice, sandbox: machine.SandboxNone}
		c.Policy.Thresholds = DefaultThresholds
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_b", "bash", `{"command":"make"}`)))
	e.send(ctx, "Build.")
	e.changeMode(ctx, ModeConfirm, ModeProgressive)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if u := e.uses(ctx)["toolu_b"]; u.Mode != string(ModeConfirm) || u.Verdict != string(VerdictAsk) {
		t.Fatalf("a call on a device of no isolation in progressive: %+v", u)
	}
}

// TestADeviceDecidesProgressiveAsConfirm: on a person's own computer the
// progressive mode decides as confirm, whether the agent names it or a
// person changes to it: a call that reads runs, a write and a bash that
// progressive would run ask, always_allow and a pattern the person
// remembered earlier in the session run, plan still blocks a write, and
// each agent.tool_use records the mode confirm. The same calls run under
// progressive on a host with a sandbox (spec 080).
func TestADeviceDecidesProgressiveAsConfirm(t *testing.T) {
	lax := Thresholds{FlagAt: 0.6, AskAt: 0.8, BlockAt: 0.95}
	write := json.RawMessage(`{"path":"main.go","content":"x"}`)
	build := json.RawMessage(`{"command":"make"}`)
	for _, p := range []Policy{{Mode: ModeProgressive}, {Mode: ModeProgressive, Thresholds: lax}} {
		for _, c := range []struct {
			name  string
			input json.RawMessage
		}{{"write", write}, {"bash", build}} {
			risk := Score(c.name, props(tools.EffectWrite), c.input, machine.KindDevice, nil)
			if d := p.Decide(c.name, props(tools.EffectWrite), c.input, risk, machine.KindDevice, nil); d.Verdict != VerdictAsk {
				t.Errorf("%s on a device under progressive %+v: %+v, want ask", c.name, p.Thresholds, d)
			}
		}
	}
	p := Policy{Mode: ModeProgressive, Thresholds: lax}
	risk := Score("bash", props(tools.EffectWrite), build, machine.KindHost, nil)
	if d := p.Decide("bash", props(tools.EffectWrite), build, risk, machine.KindHost, nil); d.Verdict != VerdictAllow {
		t.Fatalf("bash on a host under lax progressive: %+v, want allow", d)
	}
	for why, c := range map[string]struct {
		p          Policy
		name       string
		effect     tools.Effect
		input      json.RawMessage
		remembered []string
		want       Verdict
	}{
		"a read":                  {p, "read", tools.EffectRead, json.RawMessage(`{"path":"main.go"}`), nil, VerdictAllow},
		"always_allow":            {Policy{Mode: ModeProgressive, AlwaysAllow: []string{"write"}}, "write", tools.EffectWrite, write, nil, VerdictAllow},
		"a remembered pattern":    {p, "bash", tools.EffectWrite, build, []string{"bash(make*)"}, VerdictAllow},
		"another bash":            {p, "bash", tools.EffectWrite, json.RawMessage(`{"command":"rm -rf build"}`), []string{"bash(make*)"}, VerdictAsk},
		"a write in plan":         {Policy{Mode: ModePlan}, "write", tools.EffectWrite, write, nil, VerdictBlock},
		"a write in confirm":      {Policy{Mode: ModeConfirm}, "write", tools.EffectWrite, write, nil, VerdictAsk},
		"a remembered in confirm": {Policy{Mode: ModeConfirm}, "bash", tools.EffectWrite, build, []string{"bash(make*)"}, VerdictAllow},
	} {
		risk := Score(c.name, props(c.effect), c.input, machine.KindDevice, nil)
		if d := c.p.Decide(c.name, props(c.effect), c.input, risk, machine.KindDevice, c.remembered); d.Verdict != c.want {
			t.Errorf("%s on a device: %+v, want %s", why, d, c.want)
		}
	}
	for mode, want := range map[Mode]Mode{ModeProgressive: ModeConfirm, ModeConfirm: ModeConfirm, ModePlan: ModePlan, "": ModeConfirm} {
		if got := ModeOn(mode, machine.KindDevice); got != want {
			t.Errorf("%q on a device decides as %q, want %q", mode, got, want)
		}
	}
	if got := ModeOn(ModeProgressive, machine.KindCella); got != ModeProgressive {
		t.Errorf("progressive on Cella decides as %q", got)
	}

	// A turn on a device whose agent names progressive asks before a
	// bash and records confirm; the person's allow that remembers a
	// pattern runs the next bash it matches, and a write still asks.
	withWrite := func(c *Config) {
		if err := c.Tools.AddBuiltin(&fakeTool{name: "write", props: tools.Properties{Effect: tools.EffectWrite}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := t.Context()
	e := setup(t, func(c *Config) {
		c.Machine = fakeMachine{kind: machine.KindDevice, sandbox: "process"}
		c.Policy = Policy{Mode: ModeProgressive, Thresholds: lax}
		withWrite(c)
	})
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_b1", "bash", `{"command":"make"}`)),
		reply(ir.StopToolUse, call("toolu_b2", "bash", `{"command":"make install"}`)),
		reply(ir.StopToolUse, call("toolu_w", "write", `{"text":"x"}`)),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if u := e.uses(ctx)["toolu_b1"]; u.Verdict != string(VerdictAsk) || u.Mode != string(ModeConfirm) {
		t.Fatalf("bash on a device under progressive: %+v", u)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_b1", Decision: session.DecisionAllow, Remember: "bash(make*)"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("after the allow: %+v", out)
	}
	uses := e.uses(ctx)
	if u := uses["toolu_b2"]; u.Verdict != string(VerdictAllow) || u.Reason != "allowed earlier in this session" || u.Mode != string(ModeConfirm) {
		t.Fatalf("a bash the remembered pattern matches: %+v", u)
	}
	if u := uses["toolu_w"]; u.Verdict != string(VerdictAsk) || u.Mode != string(ModeConfirm) {
		t.Fatalf("a write on a device under progressive: %+v", u)
	}
	if got := e.write.ran(); len(got) != 2 {
		t.Fatalf("bash ran %v, want the allowed call and the remembered one", got)
	}

	// A person's change to progressive decides as confirm on a device, a
	// change to plan blocks a write, and on a host with a sandbox the same
	// bash runs under progressive.
	for _, c := range []struct {
		kind, to string
		verdict  Verdict
		mode     Mode
	}{
		{machine.KindDevice, string(ModeProgressive), VerdictAsk, ModeConfirm},
		{machine.KindDevice, string(ModePlan), VerdictBlock, ModePlan},
		{machine.KindHost, string(ModeProgressive), VerdictAllow, ModeProgressive},
	} {
		e := setup(t, func(cfg *Config) {
			cfg.Machine = fakeMachine{kind: c.kind, sandbox: "process"}
			cfg.Policy.Thresholds = lax
			withWrite(cfg)
		})
		name := "write"
		if c.kind == machine.KindHost {
			name = "bash"
		}
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_x", name, `{"text":"x"}`)), reply(ir.StopEndTurn, text("Done.")))
		e.send(ctx, "Change it.")
		e.changeMode(ctx, ModeConfirm, Mode(c.to))
		e.turn(ctx)
		if u := e.uses(ctx)["toolu_x"]; u.Verdict != string(c.verdict) || u.Mode != string(c.mode) {
			t.Fatalf("%s on %s after a change to %s: %+v", name, c.kind, c.to, u)
		}
	}
}
