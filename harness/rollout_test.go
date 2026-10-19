// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// TestARolloutsRunRefusesACallThatWouldAsk: a call a session would wait
// on a person's confirmation for is answered at once as denied in a run
// of a rollout, which no person attends, so the turn goes on to its end
// rather than stopping idle on tool_confirmation; the same call in a
// session of no rollout still waits (spec 077, R1).
func TestARolloutsRunRefusesACallThatWouldAsk(t *testing.T) {
	ctx := t.Context()
	host := func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} }
	e := setupSession(t, host, func(s *session.Session) { s.Rollout = "cel_01.smoke" })
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("done")))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.write.ran()) != 0 {
		t.Fatalf("outcome %+v, ran %v", out, e.write.ran())
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeDenied || res.Content[0].Text != prompts.Text(prompts.CallUnattended) {
		t.Fatalf("result %+v, %v", res, err)
	}

	f := setup(t, host)
	f.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)))
	f.send(ctx, "Build.")
	if out := f.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("a session of no rollout: outcome %+v", out)
	}
}
