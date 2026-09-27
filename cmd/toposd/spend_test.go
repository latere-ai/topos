// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestACoresSpendRefusalStopsTheTurn: Cella refusing a hosted session's
// sandbox create, or a command in it, because the session's allowance
// is spent stops the turn with budget and the refusal in detail, as a
// model gateway's refusal does, and a resume once the allowance is
// raised continues the session to its answer.
func TestACoresSpendRefusalStopsTheTurn(t *testing.T) {
	const model = "anthropic/claude-haiku-4.5"
	reviewed := luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Reviewed."}}, StopReason: ir.StopEndTurn}}
	for _, c := range []struct {
		name, code string
		// command refuses the turn's first command instead of the
		// sandbox create.
		command bool
		// requests is how many model requests the refused turn made.
		requests int
	}{
		{"a sandbox create", "budget_exhausted", false, 0},
		{"a command", "spend_exceeded", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			refusal := cellastub.Failure{Status: http.StatusPaymentRequired, Code: c.code, Detail: "the session's sandbox allowance is spent"}
			var cella *cellastub.Server
			replies := []luxstub.Reply{reviewed}
			if c.command {
				bash := luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_1", Name: "bash", Args: json.RawMessage(`{"command":"ls"}`)}}}, StopReason: ir.StopToolUse},
					// The sandbox is up by the time the model is asked,
					// so the refusal meets the command, not the setup.
					Respond: func(*ir.Request, *ir.Response) { cella.Fail(cellastub.OpSession, refusal) }}
				replies = append([]luxstub.Reply{bash}, replies...)
			}
			vars, lux, stub := hostedStubs(t, replies...)
			cella = stub
			if !c.command {
				cella.Fail(cellastub.OpCreate, refusal)
			}
			maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
			publicURL, _, stop := startServe(t, vars)
			send, id := createHostedSession(t, publicURL, vars, "cella")
			await := func(want string) string {
				t.Helper()
				for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
					_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
					if strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"`+want+`"`) {
						_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
						return events
					}
					if time.Now().After(deadline) {
						_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
						t.Fatalf("the session never stopped with %s: %s\nevents %s", want, body, events)
					}
				}
			}
			events := await("budget")
			if !strings.Contains(events, `"code":"`+c.code+`"`) || !strings.Contains(events, `"detail":"`+c.code+`"`) {
				t.Fatalf("the log does not name the refusal: %s", events)
			}
			if n := len(lux.Requests()); n != c.requests {
				t.Fatalf("%d model requests before the refusal, want %d", n, c.requests)
			}
			if code, body := send(http.MethodPost, "/v1/sessions/"+id+"/resume", `{"reason":"allowance_raised"}`); code != http.StatusOK {
				t.Fatalf("resume: %d %s", code, body)
			}
			if events := await("end_turn"); !strings.Contains(events, `"text":"Reviewed."`) {
				t.Fatalf("the resumed session did not answer: %s", events)
			}
			if code := stop(); code != 0 {
				t.Fatalf("exit %d", code)
			}
		})
	}
}
