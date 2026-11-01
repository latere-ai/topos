// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/config"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// decisionService is a decision service on loopback (spec 099): it
// records each request's route, bearer and key, suggests ask for every
// prediction, and answers 503 to everything while failing is set.
type decisionService struct {
	mu      sync.Mutex
	failing bool
	reqs    []decisionRequest
	srv     *httptest.Server
}

type decisionRequest struct {
	Path, Bearer         string
	Org, Subject, Action string
}

func newDecisionService(t *testing.T) *decisionService {
	t.Helper()
	d := &decisionService{}
	d.srv = httptest.NewServer(d)
	t.Cleanup(d.srv.Close)
	return d
}

func (d *decisionService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var k struct {
		Org      string `json:"org"`
		Subject  string `json:"subject"`
		ActionID string `json:"action_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&k)
	d.mu.Lock()
	d.reqs = append(d.reqs, decisionRequest{Path: r.URL.Path, Bearer: r.Header.Get("Authorization"), Org: k.Org, Subject: k.Subject, Action: k.ActionID})
	failing := d.failing
	d.mu.Unlock()
	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path != "/v1/predictions" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"verdict": "ask", "approve": 0.5, "uncertainty": 1, "audit_rate": 0.05, "source": "learned/1",
		"thresholds": map[string]float64{"allow_above": 0.9, "block_below": 0.25},
		"reason":     "No answers from this person yet.",
	})
}

func (d *decisionService) requests() []decisionRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]decisionRequest(nil), d.reqs...)
}

// await is the requests once n have arrived: a confirm-mode decision is
// sent in the background, so it may arrive after the turn ends.
func (d *decisionService) await(t *testing.T, n int) []decisionRequest {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if reqs := d.requests(); len(reqs) >= n || time.Now().After(deadline) {
			return reqs
		}
	}
}

// TestServeAsksTheDecisionServiceWithItsOwnToken: a server with an
// identity provider decides a hosted session's calls with the decision
// service, presenting its own token for TOPOS_DECISIONS_AUDIENCE, and
// keys each call by the session and the person who started it.
func TestServeAsksTheDecisionServiceWithItsOwnToken(t *testing.T) {
	vars, stubs := credentialStubs(t)
	svc := newDecisionService(t)
	maps.Copy(vars, map[string]string{"TOPOS_RUNNER_CAPACITY": "1", "TOPOS_DECISIONS_URL": svc.srv.URL, "TOPOS_DECISIONS_AUDIENCE": "decisions"})
	publicURL, _, stop := startServe(t, vars)
	runSession(t, publicURL, vars, "cella")
	reqs := svc.await(t, 2)
	paths := map[string]int{}
	for _, r := range reqs {
		paths[r.Path]++
		aud, ok := stubs.idp.ServiceToken(strings.TrimPrefix(r.Bearer, "Bearer "))
		if !ok || aud != "decisions" {
			t.Errorf("%s with %q: a token for %q (%v), want the server's own for decisions", r.Path, r.Bearer, aud, ok)
		}
		if !strings.HasSuffix(r.Action, "/toolu_glob") || r.Subject == "" || r.Org != r.Subject {
			t.Errorf("%s keyed %+v", r.Path, r)
		}
	}
	if paths["/v1/predictions"] != 1 || paths["/v1/decisions"] != 1 {
		t.Errorf("the session's one call was sent as %v", paths)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestServeAsksTheDecisionServiceWithItsBearer: a self-hoster's server,
// with no identity provider, sends TOPOS_DECISIONS_TOKEN.
func TestServeAsksTheDecisionServiceWithItsBearer(t *testing.T) {
	vars, _, _ := hostedStubs(t)
	svc := newDecisionService(t)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_RUNNER_CAPACITY": "1", "TOPOS_DECISIONS_URL": svc.srv.URL, "TOPOS_DECISIONS_TOKEN": "decisions-bearer"})
	publicURL, _, stop := startServe(t, vars)
	hostedSession(t, publicURL, vars)
	reqs := svc.await(t, 2)
	if len(reqs) != 2 {
		t.Errorf("%d requests, want a prediction and a decision: %+v", len(reqs), reqs)
	}
	for _, r := range reqs {
		if r.Bearer != "Bearer decisions-bearer" {
			t.Errorf("%s with %q", r.Path, r.Bearer)
		}
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// TestHostedCallWaitsWhenTheDecisionServiceFails: in progressive mode a
// call the rules leave open waits for the person when the decision
// service does not answer, and the failure is logged.
func TestHostedCallWaitsWhenTheDecisionServiceFails(t *testing.T) {
	bash := luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_bash", Name: "bash", Args: json.RawMessage(`{"command":"make deploy"}`)}}}, StopReason: ir.StopToolUse}}
	vars, _, _ := hostedStubs(t, bash)
	svc := newDecisionService(t)
	svc.failing = true
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(),
		"TOPOS_RUNNER_CAPACITY": "1", "TOPOS_DECISIONS_URL": svc.srv.URL, "TOPOS_DECISIONS_TOKEN": "decisions-bearer"})
	var log syncBuffer
	ctx, cancel := context.WithCancel(t.Context())
	publicURL, _, wait := serveLogging(t, ctx, vars, 10*time.Millisecond, &log)
	defer func() {
		cancel()
		if code := wait(); code != 0 {
			t.Errorf("exit %d", code)
		}
	}()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: deployer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n  tools: [bash]\n  approvals: {mode: progressive}\n"
	if code, body := send(http.MethodPut, "/v1/agents/deployer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"deployer","message":"Deploy it."}`)
	var s struct {
		ID string `json:"id"`
	}
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+s.ID, "")
		if strings.Contains(body, `"stop_reason":"tool_confirmation"`) {
			break
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
			t.Fatalf("the call never waited: %s\nevents %s", body, events)
		}
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
	if !strings.Contains(events, `"verdict":"ask"`) || !strings.Contains(events, "The decision service did not answer.") {
		t.Errorf("the call was not held for the person: %s", events)
	}
	if !strings.Contains(log.String(), "decision service") {
		t.Errorf("the failure was not logged: %s", log.String())
	}
}

// TestDeciderFromTheConfiguration: no URL decides by the rules, and a
// URL the decision client refuses stops the start.
func TestDeciderFromTheConfiguration(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	if d, err := decider(config.Config{}, nil, log); d != nil || err != nil {
		t.Errorf("no decision service: %v %v", d, err)
	}
	if _, err := decider(config.Config{DecisionsURL: "https://decisions.example"}, nil, log); err == nil || !strings.Contains(err.Error(), "TOPOS_DECISIONS_URL") {
		t.Errorf("a decision service with no token: %v", err)
	}
	if _, err := decider(config.Config{DecisionsURL: "https://decisions.example", DecisionsToken: "t", DecisionsMode: "sometimes"}, nil, log); err == nil || !strings.Contains(err.Error(), "TOPOS_DECISIONS_MODE") {
		t.Errorf("an unknown mode: %v", err)
	}
	if d, err := decider(config.Config{DecisionsURL: "https://decisions.example", DecisionsToken: "t", DecisionsMode: "learned"}, nil, log); d == nil || err != nil {
		t.Errorf("learned: %v %v", d, err)
	}
	if _, err := decider(config.Config{DecisionsURL: "https://decisions.example", DecisionsToken: "t", DecisionsOrg: "owner"}, nil, log); err == nil || !strings.Contains(err.Error(), "TOPOS_DECISIONS_ORG") {
		t.Errorf("an unknown organization source: %v", err)
	}
}

// TestTheDeciderKeysByTheConfiguredOrganization (spec 100): the decider
// toposd builds keys a call of a session whose agent an organization owns
// by the organization with TOPOS_DECISIONS_ORG=agent, and by the
// initiator with it unset; the subject is the initiator either way.
func TestTheDeciderKeysByTheConfiguredOrganization(t *testing.T) {
	for _, c := range []struct{ org, want string }{{"", "https://issuer.example|usr_1"}, {"initiator", "https://issuer.example|usr_1"}, {"agent", "acme"}} {
		svc := newDecisionService(t)
		d, err := decider(config.Config{DecisionsURL: svc.srv.URL, DecisionsToken: "t", DecisionsOrg: c.org}, nil, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		s := session.Session{ID: "ses_1", Org: "acme", Agent: session.AgentRef{ID: "agent_1"}, Initiator: session.Sender{Subject: "https://issuer.example|usr_1", Kind: session.SenderPerson}}
		_, out, err := d.Decide(t.Context(), harness.Call{Policy: harness.Policy{Mode: harness.ModeProgressive}, Session: s, ToolUseID: "toolu_1", Name: "bash",
			Props: tools.Properties{Effect: tools.EffectWrite}, Input: json.RawMessage(`{"command":"make"}`), MachineKind: machine.KindCella})
		if err != nil {
			t.Fatal(err)
		}
		reqs := svc.await(t, 2)
		if len(reqs) != 2 || out.Org != c.want {
			t.Fatalf("TOPOS_DECISIONS_ORG=%q: %d requests, the decision names %q", c.org, len(reqs), out.Org)
		}
		for _, r := range reqs {
			if r.Org != c.want || r.Subject != "https://issuer.example|usr_1" || r.Action != "ses_1/toolu_1" {
				t.Errorf("TOPOS_DECISIONS_ORG=%q: %s keyed %+v, want the organization %q", c.org, r.Path, r, c.want)
			}
		}
	}
}
