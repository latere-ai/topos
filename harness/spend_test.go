// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestACallACoreRefusesForSpendStopsTheTurn: a call whose machine's core
// refused it for spend is answered with the refusal, the step's other
// calls still run, and the turn stops with budget and a session.error
// naming the refusal and the core, with no request after it and no
// retry; a message after the allowance is raised continues the session
// with the refused call's result in view.
func TestACallACoreRefusesForSpendStopsTheTurn(t *testing.T) {
	for _, code := range []string{"budget_exhausted", "spend_exceeded"} {
		t.Run(code, func(t *testing.T) {
			e := setup(t, nil)
			ctx := t.Context()
			refused := &models.SpendError{Core: machine.KindCella, Code: code, Err: errors.New("Cella refused to act in the sandbox")}
			// A sequential call refused, then a parallel one, so both
			// paths that run calls meet a refusal.
			e.write.run = func(tools.Call) (tools.Result, error) { return tools.Result{}, fmt.Errorf("machine: %w", refused) }
			e.echo.run = func(c tools.Call) (tools.Result, error) {
				if c.ID == "toolu_d" {
					return tools.Result{}, refused
				}
				return tools.Text(tools.OutcomeOK, "echo "+c.ID), nil
			}
			e.stub.Script(model,
				reply(ir.StopToolUse, call("toolu_a", "echo", `{"text":"a"}`), call("toolu_b", "bash", `{"command":"ls"}`), call("toolu_c", "echo", `{"text":"c"}`), call("toolu_d", "echo", `{"text":"d"}`)),
				luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Carrying on.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
					for _, m := range r.Messages {
						for _, b := range m.Blocks {
							if b.ToolResult != nil && b.ToolResult.ToolUseID == "toolu_b" && b.ToolResult.IsError && strings.Contains(b.ToolResult.Blocks[0].Text, code) {
								return nil
							}
						}
					}
					return errors.New("the refused call's result is not in view")
				}},
			)
			e.send(ctx, "Echo and list.")
			out := e.turn(ctx)
			if out.StopReason != session.StopBudget || out.Detail != code {
				t.Fatalf("outcome %+v", out)
			}
			if got := e.echo.ran(); len(got) != 3 || len(e.write.ran()) != 1 {
				t.Fatalf("ran %v; the step's other calls run", got)
			}
			if n := len(e.events(ctx, session.TypeToolResult)); n != 4 {
				t.Fatalf("%d results; every call is answered", n)
			}
			var se session.SessionError
			if err := e.events(ctx, session.TypeSessionError)[0].Decode(&se); err != nil || se.Code != code || se.Retryable || se.Detail != machine.KindCella {
				t.Fatalf("session.error %+v, %v", se, err)
			}
			if n := len(e.stub.Requests()); n != 1 {
				t.Fatalf("%d requests; the refusal stops the turn before the next", n)
			}
			e.send(ctx, "The allowance is raised.")
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("the next turn %+v", out)
			}
		})
	}
}
