// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package idpstub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type answer struct {
	status int
	body   map[string]any
}

func (a answer) code() string { s, _ := a.body["error"].(string); return s }

func send(t *testing.T, method, u, bearer, body string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, u, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s %s answered %q", method, u, b)
	}
	return answer{resp.StatusCode, out}
}

func hostToken(t *testing.T, s *Server, secret string) answer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/token", strings.NewReader(url.Values{"grant_type": {"client_credentials"}}.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("host", secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return answer{resp.StatusCode, out}
}

// TestTheStubAnswersTheHostContract: the host's token needs the client's
// secret; every host route needs a live host token; create is idempotent
// per owner; archive and disable need the confirmation; the mint refuses
// a malformed session, an unknown subject, a disabled one and an audience
// outside the host's list with the contract's codes, and records what
// each token carries; failures are injected per operation.
func TestTheStubAnswersTheHostContract(t *testing.T) {
	s := New(t, "host", "secret")
	if a := hostToken(t, s, "wrong"); a.status != http.StatusUnauthorized || a.code() != "invalid_client" {
		t.Fatalf("a wrong secret: %+v", a)
	}
	a := hostToken(t, s, "secret")
	host, _ := a.body["access_token"].(string)
	if a.status != http.StatusOK || host == "" || a.body["expires_in"] != float64(3600) || len(s.HostTokens()) != 1 {
		t.Fatalf("the host's token: %+v", a)
	}
	u := s.URL()
	for _, c := range []struct {
		bearer, code string
	}{{"", "unauthorized"}, {"stranger", "unauthorized"}} {
		if a := send(t, http.MethodGet, u+"/hosted-agents", c.bearer, ""); a.status != http.StatusUnauthorized || a.code() != c.code {
			t.Fatalf("bearer %q: %+v", c.bearer, a)
		}
	}
	put := `{"name":"reviewer","owner":{"type":"user","id":"alice"},"applied_by":"alice"}`
	a = send(t, http.MethodPut, u+"/hosted-agents/agent_01", host, put)
	subject, _ := a.body["subject"].(string)
	if a.status != http.StatusCreated || subject == "" {
		t.Fatalf("create: %+v", a)
	}
	if a := send(t, http.MethodPut, u+"/hosted-agents/agent_01", host, put); a.status != http.StatusOK || a.body["subject"] != subject {
		t.Fatalf("a repeated create: %+v", a)
	}
	for body, code := range map[string]string{
		`{"name":"r","owner":{"type":"organization","id":"o"}}`: "owner_mismatch",
		`{"name":"r"}`:                           "invalid_owner",
		`{"owner":{"type":"user","id":"alice"}}`: "bad_request",
		`[`:                                      "bad_request",
	} {
		if a := send(t, http.MethodPut, u+"/hosted-agents/agent_01", host, body); a.code() != code {
			t.Errorf("%s: %+v, want %s", body, a, code)
		}
	}
	if a := send(t, http.MethodPut, u+"/hosted-agents/bad%20ref", host, put); a.code() != "bad_request" {
		t.Fatalf("a malformed ref: %+v", a)
	}
	mint := func(body string) answer { return send(t, http.MethodPost, u+"/actor-tokens", host, body) }
	for body, code := range map[string]string{
		`{"audience":"cella","subject":"` + subject + `"}`:                                                 "invalid_session",
		`{"audience":"cella","subject":"` + subject + `","session":{"id":"s","workload":"x"}}`:             "invalid_session",
		`{"audience":"cella","subject":"` + subject + `","session":{"id":"s","workload":"session","x":1}}`: "invalid_session",
		`{"audience":"cella","subject":"nobody","session":{"id":"s","workload":"session"}}`:                "unknown_agent",
		`{"audience":"lux","subject":"` + subject + `","session":{"id":"s","workload":"session"}}`:         "invalid_target",
		`{`: "bad_request",
	} {
		if a := mint(body); a.code() != code {
			t.Errorf("mint %s: %+v, want %s", body, a, code)
		}
	}
	a = mint(`{"audience":"origo","subject":"` + subject + `","session":{"id":"ses_1","workload":"sandbox"},"ttl_seconds":60}`)
	tok, _ := a.body["actor_token"].(string)
	if m, ok := s.Token(tok); a.status != http.StatusOK || !ok || m.Audience != "origo" || m.SessionID != "ses_1" || m.Workload != "sandbox" || m.Ref != "agent_01" || a.body["expires_in"] != float64(60) {
		t.Fatalf("mint: %+v %+v", a, m)
	}
	if len(s.Mints()) != 1 {
		t.Fatalf("mints %+v", s.Mints())
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/archive", host, `{}`); a.code() != "confirmation_required" {
		t.Fatalf("an unconfirmed archive: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/archive", host, `{"permanent":true}`); a.status != http.StatusOK || a.body["status"] != StatusArchived {
		t.Fatalf("archive: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/archive", host, `{"permanent":true}`); a.status != http.StatusOK {
		t.Fatalf("a repeated archive: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_02/archive", host, `{"permanent":true}`); a.code() != "unknown_agent" {
		t.Fatalf("an unknown archive: %+v", a)
	}
	if a := send(t, http.MethodGet, u+"/hosted-agents?status=archived", host, ""); len(a.body["items"].([]any)) != 1 {
		t.Fatalf("list: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/disable", host, `{"subject":"`+subject+`"}`); a.code() != "confirmation_required" {
		t.Fatalf("an unconfirmed disable: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/disable", host, `{"subject":"nobody","permanent":true}`); a.code() != "unknown_agent" {
		t.Fatalf("a disable of another subject: %+v", a)
	}
	if a := send(t, http.MethodPost, u+"/hosted-agents/agent_01/disable", host, `{"subject":"`+subject+`","permanent":true}`); a.status != http.StatusOK || a.body["status"] != StatusDisabled {
		t.Fatalf("disable: %+v", a)
	}
	if a := mint(`{"audience":"cella","subject":"` + subject + `","session":{"id":"s","workload":"session"}}`); a.code() != "agent_disabled" {
		t.Fatalf("a disabled agent: %+v", a)
	}
	if ag := s.Agents(); len(ag) != 1 || ag[0].Status != StatusDisabled || ag[0].AppliedBy != "alice" {
		t.Fatalf("agents %+v", ag)
	}
	s.Fail(OpList, Failure{Status: http.StatusServiceUnavailable, Code: "unavailable", Times: 2})
	for range 2 {
		if a := send(t, http.MethodGet, u+"/hosted-agents", host, ""); a.status != http.StatusServiceUnavailable {
			t.Fatalf("an injected failure: %+v", a)
		}
	}
	s.SetAudiences("cella")
	if a := send(t, http.MethodGet, u+"/nothing", host, ""); a.code() != "not_found" {
		t.Fatalf("an unknown route: %+v", a)
	}
	if n := s.Count(OpList); n < 4 || len(s.Requests()) == 0 {
		t.Fatalf("%d lists recorded", n)
	}
}
