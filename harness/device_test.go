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
