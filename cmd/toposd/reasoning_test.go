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

	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestASessionRunsAtTheLevelItsAuthorizerNames: through toposd, an
// authorizer's allow names the reasoning level a session runs at (spec
// 048). The create's level is the header's from the start, and the first
// turn's request carries it; a send whose allow names another level
// appends one session.model_changed by the service, keeping the model,
// and the next turn's request carries the new level. The API answers the
// level as reasoning, a change in the log too.
func TestASessionRunsAtTheLevelItsAuthorizerNames(t *testing.T) {
	const model = "anthropic/claude-haiku-4.5"
	vars, lux, _ := hostedStubs(t, saidReply("Thought it through."))
	lux.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Answered quickly."}}, StopReason: ir.StopEndTurn}})
	az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	leveled := func(action, level string) stub.Rule {
		return stub.Rule{Action: action, Allow: true, Limits: map[string]any{"reasoning": level}}
	}
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_AUTHORIZER_URL": az.URL(), "TOPOS_AUTHORIZER_TOKEN": az.Token()})
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: thinker\nspec:\n  model: {name: " + model + "}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/thinker", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}

	az.SetRules(leveled(authorizer.ActionSessionCreate, "high"))
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"thinker","message":"Think."}`)
	if code != http.StatusCreated || !strings.Contains(body, `"model":{"name":"`+model+`","reasoning":"high"}`) {
		t.Fatalf("create: %d %s", code, body)
	}
	var s session.Session
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	waitAnswered(t, publicURL, vars, s.ID)

	az.SetRules(leveled(authorizer.ActionSessionSend, "low"))
	if code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Quicker."}]}}`); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+s.ID, "")
		if strings.Contains(body, `"turn":2`) && strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"end_turn"`) {
			if !strings.Contains(body, `"model":{"name":"`+model+`","reasoning":"low"}`) {
				t.Fatalf("the session after the send: %s", body)
			}
			break
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
			t.Fatalf("the second turn never answered: %s\nevents %s", body, events)
		}
	}

	_, body = send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
	if strings.Contains(body, `"effort"`) {
		t.Fatalf("the log answers effort: %s", body)
	}
	var log struct {
		Items []session.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatal(err)
	}
	var changes []session.ModelChanged
	for _, e := range log.Items {
		if e.Type == session.TypeModelChanged {
			var p session.ModelChanged
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			changes = append(changes, p)
		}
	}
	want := session.ModelChanged{By: session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService},
		Old: session.ModelRef{Name: model, Reasoning: "high"}, New: session.ModelRef{Name: model, Reasoning: "low"}}
	if len(changes) != 1 || changes[0] != want {
		t.Fatalf("session.model_changed %+v", changes)
	}
	reqs := lux.Requests()
	if len(reqs) != 2 {
		t.Fatalf("the provider was asked %d times", len(reqs))
	}
	for i, level := range []string{"high", "low"} {
		if r := reqs[i].Request; r.Reasoning == nil || r.Reasoning.Effort != ir.Effort(level) {
			t.Fatalf("turn %d's request ran at %+v, want %s", i+1, r.Reasoning, level)
		}
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
