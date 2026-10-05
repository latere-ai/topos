// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/llmdialect/bridge"
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

// TestARoutedTurnMovesOffAModelThatCannotServe: through toposd, on an
// installation whose sessions act with their own keys, a routed session
// whose model the gateway answers upstream_error over an upstream's 429
// asks the authorizer session.update with failed_model and the gateway's
// detail as failed_detail in the middle of its first turn, as its
// initiator, and the turn is answered by the model the allow names. The
// door answers the session's key for the models the key selects, and the
// model named is one only the door's list gives figures for. The
// authorizer widens the key to it as it answers, and the door applies the
// widening some time after the allow, as a gateway replica that has not
// read the change yet answers a key with its earlier models: the runner
// reads the list again until it names the model. The log holds the
// failed request, the service's session.model_changed with the reason
// model_busy, and the request that answered, each made with the session's
// key, and the session stands on the new model with its via.
func TestARoutedTurnMovesOffAModelThatCannotServe(t *testing.T) {
	const (
		quick  = "tier/quick"
		first  = "anthropic/claude-haiku-4.5"
		second = "vendor/door-only-model"
		// lag is how long after the failover's question the door goes on
		// answering the key's earlier models: well inside hosted.DoorSettle.
		lag = 600 * time.Millisecond
	)
	const detail = `upstream status 429: {"error":{"message":"Rate limit exceeded: free-models-per-min.","code":429}}`
	vars, s := credentialStubs(t, luxstub.Reply{Response: ir.Response{Model: first}, Fail: &luxstub.Failure{Status: 502, Times: 99, Detail: detail,
		Body: `{"type":"error","error":{"type":"upstream_error","message":"The provider returned an error."}}`}})
	vars["TOPOS_MODELS_URL"] = s.lux.URL()
	s.lux.Models(bridge.Model{Name: first, ContextWindow: 200_000, MaxOutputTokens: 8_192}, bridge.Model{Name: second, ContextWindow: 100_000, MaxOutputTokens: 4_096})
	s.lux.Script(second, luxstub.Reply{Response: ir.Response{Model: second, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Answered by the second."}}, StopReason: ir.StopEndTurn}})
	var (
		mu      sync.Mutex
		widened time.Time
	)
	s.lux.Select(func(key string) []string {
		k, ok := s.keys.ByHash(hash(key))
		if !ok || k.Workload != "session" {
			return nil
		}
		asked := slices.ContainsFunc(s.az.Requests(), func(r authz.Request) bool {
			return r.Action == authorizer.ActionSessionUpdate && r.Resource.String("failed_model") != ""
		})
		if !asked {
			return []string{first}
		}
		mu.Lock()
		defer mu.Unlock()
		if widened.IsZero() {
			widened = time.Now().Add(lag)
		}
		if time.Now().Before(widened) {
			return []string{first}
		}
		return []string{first, second}
	})
	s.az.SetRules(
		stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true, Limits: map[string]any{"model": first}},
		stub.Rule{Action: authorizer.ActionSessionUpdate, Allow: true, Limits: map[string]any{"model": second}},
	)
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: quick\nspec:\n  model: {name: " + quick + "}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/quick", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"quick","message":"Answer."}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var sess session.Session
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		t.Fatal(err)
	}
	// The turn ends either way: answered, or with the session.error the
	// trail below reports.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+sess.ID, "")
		if strings.Contains(body, `"turn":1`) && strings.Contains(body, `"status":"idle"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the turn never ended: %s", body)
		}
	}
	_, body = send(http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "")
	var log struct {
		Items []session.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &log); err != nil {
		t.Fatal(err)
	}
	for _, e := range log.Items {
		if e.Type == session.TypeSessionError {
			t.Fatalf("the turn ended with %s", e.Payload)
		}
	}
	if _, body := send(http.MethodGet, "/v1/sessions/"+sess.ID, ""); !strings.Contains(body, `"model":{"name":"`+second+`","via":"`+quick+`"}`) || !strings.Contains(body, `"turn":1`) {
		t.Fatalf("the session after its turn: %s", body)
	}
	var trail []string
	for _, e := range log.Items {
		switch e.Type {
		case session.TypeModelRequest:
			var p session.ModelRequest
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			trail = append(trail, p.Model+" "+p.Outcome)
		case session.TypeModelChanged:
			var p session.ModelChanged
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.By.Kind != session.SenderService || p.Reason != session.ReasonModelBusy || p.Old.Name != first || p.New != (session.ModelRef{Name: second, Via: quick}) {
				t.Fatalf("session.model_changed %+v", p)
			}
			trail = append(trail, "changed")
		case session.TypeSessionError:
			t.Fatalf("a turn that moved recorded %s", e.Payload)
		}
	}
	if got := strings.Join(trail, ", "); got != first+" error, changed, "+second+" ok" {
		t.Fatalf("the turn's trail is %s", got)
	}
	var failover *authz.Request
	for _, r := range s.az.Requests() {
		if r.Action == authorizer.ActionSessionUpdate {
			failover = &r
		}
	}
	if failover == nil || failover.Resource.String("failed_model") != first || failover.Resource.String("current_model") != first ||
		failover.Resource.String("current_model_via") != quick || failover.Resource.String("model") != quick || failover.Resource.ID != sess.ID ||
		failover.Resource.String("failed_detail") != detail {
		t.Fatalf("session.update asked about %+v", failover)
	}
	runnerKey, ok := s.keys.Key(sess.ID, "session")
	if !ok {
		t.Fatal("the session has no runner key")
	}
	for _, r := range s.lux.Requests() {
		if hash(luxstub.Presented(r.Header)) != runnerKey.Hash {
			t.Fatalf("a model request on %s carried a key that is not the session's", r.Model)
		}
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestARoutedTurnPassesOverAModelItCannotConnect: through toposd, a model
// the failover's allow names that the door never lists for the session's
// key, and that the embedded catalog does not know, cannot be connected
// once hosted.DoorSettle has passed. The turn asks session.update again,
// standing on the model it runs, with that model as failed_model and why
// it could not be connected as failed_detail, and is answered by the model
// the second allow names; the change records the model passed over.
func TestARoutedTurnPassesOverAModelItCannotConnect(t *testing.T) {
	const (
		quick  = "tier/quick"
		first  = "anthropic/claude-haiku-4.5"
		never  = "vendor/never-selected"
		second = "vendor/door-only-model"
	)
	vars, s := credentialStubs(t, luxstub.Reply{Response: ir.Response{Model: first}, Fail: &luxstub.Failure{Status: 503, Times: 99,
		Body: `{"type":"error","error":{"type":"provider_unavailable","message":"No provider for this model is available right now."}}`}})
	vars["TOPOS_MODELS_URL"] = s.lux.URL()
	s.lux.Models(bridge.Model{Name: first, ContextWindow: 200_000, MaxOutputTokens: 8_192}, bridge.Model{Name: second, ContextWindow: 100_000, MaxOutputTokens: 4_096},
		bridge.Model{Name: never, ContextWindow: 100_000, MaxOutputTokens: 4_096})
	s.lux.Script(second, luxstub.Reply{Response: ir.Response{Model: second, Blocks: []ir.Block{{Type: ir.BlockText, Text: "Answered by the second."}}, StopReason: ir.StopEndTurn}})
	s.lux.Select(func(key string) []string {
		if k, ok := s.keys.ByHash(hash(key)); ok && k.Workload == "session" {
			return []string{first, second}
		}
		return nil
	})
	// The authorizer routes on the model that failed: the first failure
	// moves the turn to a model the key never selects, and that one's to
	// the second.
	az := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()), stub.WithResourceName(func(r authz.Resource) string { return r.String("failed_model") }))
	az.SetRules(
		stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true, Limits: map[string]any{"model": first}},
		stub.Rule{Action: authorizer.ActionSessionUpdate, Resource: first, Allow: true, Limits: map[string]any{"model": never}},
		stub.Rule{Action: authorizer.ActionSessionUpdate, Resource: never, Allow: true, Limits: map[string]any{"model": second}},
	)
	vars["TOPOS_AUTHORIZER_URL"], vars["TOPOS_AUTHORIZER_TOKEN"] = az.URL(), az.Token()
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: quick\nspec:\n  model: {name: " + quick + "}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/quick", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"quick","message":"Answer."}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var sess session.Session
	if err := json.Unmarshal([]byte(body), &sess); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+sess.ID, "")
		if strings.Contains(body, `"turn":1`) && strings.Contains(body, `"status":"idle"`) {
			if !strings.Contains(body, `"model":{"name":"`+second+`","via":"`+quick+`"}`) {
				_, events := send(http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "")
				t.Fatalf("the session after its turn: %s\nevents %s", body, events)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the turn never ended: %s", body)
		}
	}
	var questions []authz.Request
	for _, r := range az.Requests() {
		if r.Action == authorizer.ActionSessionUpdate {
			questions = append(questions, r)
		}
	}
	if len(questions) != 2 || questions[0].Resource.String("failed_model") != first || questions[1].Resource.String("failed_model") != never ||
		questions[1].Resource.String("current_model") != first || questions[1].Resource.String("current_model_via") != quick ||
		!strings.Contains(questions[1].Resource.String("failed_detail"), never+" was named and could not be connected") {
		t.Fatalf("session.update was asked %+v", questions)
	}
	_, body = send(http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "")
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
	if len(changes) != 1 || changes[0].Old.Name != first || changes[0].New.Name != second || !strings.Contains(changes[0].Detail, never+" was named and could not be connected") {
		t.Fatalf("session.model_changed %+v", changes)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}
