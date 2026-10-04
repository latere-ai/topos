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

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestARoutedSessionRunsTheModelsItsAuthorizerNames: through toposd, on
// an installation whose authorizer routes, an agent names a choice no
// door lists. Its session starts on the model the create's allow names,
// keeps the agent's name as via, and its first model.request names the
// model that ran. A send whose allow names another model appends
// session.model_changed made by the service before the turn's first
// model.request, which the provider is asked for; the send's question
// carries the model the session stood on, the name it was asked by and
// idle_seconds. With an authorizer that names no model, the same create
// is model_unknown.
func TestARoutedSessionRunsTheModelsItsAuthorizerNames(t *testing.T) {
	const (
		quick  = "tier/quick"
		first  = "anthropic/claude-haiku-4.5"
		second = "anthropic/claude-sonnet-4.5"
	)
	vars, lux, _ := hostedStubs(t, saidReply("Answered by the first."))
	lux.Script(second, luxstub.Reply{Response: ir.Response{Model: second, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Answered by the second."}}, StopReason: ir.StopEndTurn}})
	az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	routed := func(action, model string) stub.Rule {
		return stub.Rule{Action: action, Allow: true, Limits: map[string]any{"model": model}}
	}
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_AUTHORIZER_URL": az.URL(), "TOPOS_AUTHORIZER_TOKEN": az.Token()})
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: quick\nspec:\n  model: {name: " + quick + "}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/quick", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	// No allow names a model yet, so the name is sent as written and no
	// door lists it.
	if code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"quick","message":"Answer."}`); code != http.StatusUnprocessableEntity || !strings.Contains(body, `"code":"model_unknown"`) {
		t.Fatalf("a create of a name nothing resolves: %d %s", code, body)
	}

	az.SetRules(routed(authorizer.ActionSessionCreate, first))
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"quick","message":"Answer."}`)
	if code != http.StatusCreated || !strings.Contains(body, `"model":{"name":"`+first+`","via":"`+quick+`"}`) {
		t.Fatalf("create: %d %s", code, body)
	}
	var s session.Session
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatal(err)
	}
	waitAnswered(t, publicURL, vars, s.ID)

	az.SetRules(routed(authorizer.ActionSessionSend, second))
	if code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"And again."}]}}`); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+s.ID, "")
		if strings.Contains(body, `"turn":2`) && strings.Contains(body, `"status":"idle"`) && strings.Contains(body, `"stop_reason":"end_turn"`) {
			if !strings.Contains(body, `"model":{"name":"`+second+`","via":"`+quick+`"}`) {
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
	var log struct {
		Items []session.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatal(err)
	}
	var ran []string
	var changed, sent, asked uint64
	for _, e := range log.Items {
		switch e.Type {
		case session.TypeModelRequest:
			var p session.ModelRequest
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if ran = append(ran, p.Model); e.Turn == 2 && asked == 0 {
				asked = e.Seq
			}
		case session.TypeModelChanged:
			var p session.ModelChanged
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			want := session.ModelChanged{By: session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService},
				Old: session.ModelRef{Name: first, Via: quick}, New: session.ModelRef{Name: second, Via: quick}}
			if changed != 0 || p != want {
				t.Fatalf("session.model_changed %+v at %d after one at %d", p, e.Seq, changed)
			}
			changed = e.Seq
		case session.TypeUserMessage:
			sent = e.Seq
		}
	}
	if len(ran) != 2 || ran[0] != first || ran[1] != second {
		t.Fatalf("the model requests ran %v", ran)
	}
	if changed == 0 || sent != changed+1 || asked < sent {
		t.Fatalf("the model changed at %d, the message is at %d and the turn's first request at %d", changed, sent, asked)
	}
	if reqs := lux.Requests(); len(reqs) != 2 || reqs[0].Request.Model != first || reqs[1].Request.Model != second {
		t.Fatalf("the provider was asked %d times", len(reqs))
	}

	// What the authorizer was told, as its endpoint read it off the wire.
	var create, message *authz.Request
	for _, r := range az.Requests() {
		switch r.Action {
		case authorizer.ActionSessionCreate:
			create = &r
		case authorizer.ActionSessionSend:
			message = &r
		}
	}
	if create == nil || create.Resource.String("model") != quick {
		t.Fatalf("session.create asked about %+v", create)
	}
	if message == nil || message.Resource.String("model") != first || message.Resource.String("model_via") != quick {
		t.Fatalf("session.send asked about %+v", message)
	}
	if idle, ok := message.Resource.Fields["idle_seconds"].(float64); !ok || idle < 0 || idle != float64(int(idle)) {
		t.Fatalf("session.send carried idle_seconds %v", message.Resource.Fields["idle_seconds"])
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
