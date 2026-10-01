// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
)

const (
	issuer = "https://login.example.com"
	alice  = issuer + "|alice"
	bob    = issuer + "|bob"
	root   = issuer + "|root"
)

func ask(t *testing.T, p *auth.OwnerPolicy, subject, action, id, owner string, claims map[string]any) authz.Decision {
	t.Helper()
	fields := map[string]any{}
	if owner != "" {
		fields["owner"] = owner
	}
	d, err := p.Authorize(t.Context(), authz.Request{Subject: subject, Action: action, Claims: claims,
		Resource: authz.NewResource(authorizer.Kind(action), id, fields)})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestOwnerPolicyRows: the creator and the admin subjects act, everyone
// else is denied, a create is allowed on an object that does not exist,
// a list is narrowed to the caller's own, and a key's grants narrow the
// answer.
func TestOwnerPolicyRows(t *testing.T) {
	p := &auth.OwnerPolicy{Admins: []string{root}}
	allow := func(d authz.Decision) bool { return d.Allow }
	for _, row := range []struct {
		name       string
		subject    string
		action, id string
		owner      string
		allow      bool
		reason     string
	}{
		{"owner reads", alice, authorizer.ActionSessionRead, "ses_1", alice, true, ""},
		{"owner deletes", alice, authorizer.ActionSessionDelete, "ses_1", alice, true, ""},
		{"admin acts on anyone's", root, authorizer.ActionAgentArchive, "agt_1", alice, true, ""},
		{"another subject", bob, authorizer.ActionSessionSend, "ses_1", alice, false, authz.ReasonNotOwner},
		{"create of a new object", bob, authorizer.ActionMemoryStoreCreate, "", "", true, ""},
		{"a send by the session's owner", alice, authorizer.ActionSessionSend, "ses_1", alice, true, ""},
		{"read of a missing object", bob, authorizer.ActionAgentRead, "agt_none", "", false, authz.ReasonNotOwner},
		{"anonymous", "", authorizer.ActionAgentCreate, "", "", false, authz.ReasonAnonymous},
		{"probe", alice, authorizer.ActionSessionRead, authz.ProbeID, alice, false, authz.ReasonProbe},
		{"unknown action", alice, "session.explode", "ses_1", alice, false, auth.ReasonUnknownAction},
	} {
		if d := ask(t, p, row.subject, row.action, row.id, row.owner, nil); allow(d) != row.allow || d.Reason != row.reason {
			t.Errorf("%s: %+v", row.name, d)
		}
	}
	d, err := p.Authorize(t.Context(), authz.Request{Subject: alice, Action: authorizer.ActionSessionRead,
		Resource: authz.NewResource(authorizer.KindAgent, "agt_1", map[string]any{"owner": alice})})
	if err != nil || d.Allow || d.Reason != auth.ReasonUnknownAction {
		t.Fatalf("an action asked about another kind: %+v, %v", d, err)
	}

	create := func(subject, agentOwner string) authz.Decision {
		d, err := p.Authorize(t.Context(), authz.Request{Subject: subject, Action: authorizer.ActionSessionCreate,
			Resource: authz.NewResource(authorizer.KindSession, "", map[string]any{"agent_owner": agentOwner})})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if !create(alice, alice).Allow || !create(root, alice).Allow || create(bob, alice).Allow {
		t.Fatal("a session is created on the caller's own agent, or by an admin")
	}
	// A fork starts a session of the agent from a session: the agent's
	// owner forks a session of their own.
	fork := func(subject, agentOwner, owner string) authz.Decision {
		d, err := p.Authorize(t.Context(), authz.Request{Subject: subject, Action: authorizer.ActionSessionFork,
			Resource: authz.NewResource(authorizer.KindSession, "ses_1", map[string]any{"agent_owner": agentOwner, "owner": owner})})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if !fork(alice, alice, alice).Allow || !fork(root, alice, alice).Allow || fork(bob, alice, alice).Allow || fork(bob, alice, bob).Allow || fork(alice, alice, bob).Allow {
		t.Fatal("a session is forked by the agent's owner from a session of their own, or by an admin")
	}
	// An organization's agent is its admins' alone: the policy knows no
	// organization's members (spec 035).
	org := map[string]any{"type": "organization", "id": "org_1"}
	for _, action := range []string{authorizer.ActionAgentCreate, authorizer.ActionAgentRead, authorizer.ActionAgentUpdate, authorizer.ActionAgentArchive} {
		id := "agt_1"
		if action == authorizer.ActionAgentCreate {
			id = ""
		}
		res := authz.NewResource(authorizer.KindAgent, id, map[string]any{"owner": org})
		for _, subject := range []string{alice, root} {
			d, err := p.Authorize(t.Context(), authz.Request{Subject: subject, Action: action, Resource: res})
			if err != nil || d.Allow != (subject == root) || (subject != root && d.Reason != authz.ReasonNotOwner) {
				t.Errorf("%s of an organization's agent by %s: %+v, %v", action, subject, d, err)
			}
		}
	}
	orgCreate, err := p.Authorize(t.Context(), authz.Request{Subject: alice, Action: authorizer.ActionSessionCreate,
		Resource: authz.NewResource(authorizer.KindSession, "", map[string]any{"agent_owner": org})})
	if err != nil || orgCreate.Allow {
		t.Fatalf("a session of an organization's agent: %+v, %v", orgCreate, err)
	}
	if d := ask(t, p, alice, authorizer.ActionSessionList, "", "", nil); !d.Allow || d.Filter == nil || d.Filter.Owners[0] != alice {
		t.Fatalf("a list: %+v", d)
	}
	if d := ask(t, p, root, authorizer.ActionSessionList, "", "", nil); !d.Allow || d.Filter != nil {
		t.Fatalf("an admin's list: %+v", d)
	}

	narrowed := map[string]any{
		"token_use":             authz.TokenUsePAT,
		"authorization_details": []any{map[string]any{"type": authz.GrantType, "actions": []any{"topos:" + authorizer.ActionSessionRead}}},
	}
	if d := ask(t, p, alice, authorizer.ActionSessionRead, "ses_1", alice, narrowed); !d.Allow {
		t.Fatalf("a granted read: %+v", d)
	}
	if d := ask(t, p, alice, authorizer.ActionSessionDelete, "ses_1", alice, narrowed); d.Allow || d.Reason != authz.ReasonGrant {
		t.Fatalf("an ungranted delete: %+v", d)
	}
	unreadable := map[string]any{"token_use": authz.TokenUsePAT, "authorization_details": "everything"}
	if d := ask(t, p, alice, authorizer.ActionSessionRead, "ses_1", alice, unreadable); d.Allow || d.Reason != authz.ReasonGrant {
		t.Fatalf("grants that do not parse: %+v", d)
	}
}

// TestAuthorizerConformanceOwnerPolicy: the owner policy served through
// the scaffold of latere.ai/x/pkg/authz/server answers the shared
// contract for every row of the vocabulary.
func TestAuthorizerConformanceOwnerPolicy(t *testing.T) {
	const bearer = "conformance-bearer"
	s := httptest.NewServer(server.New(server.Options{Bearer: bearer, Vocabulary: authorizer.Vocabulary(), Decider: &auth.OwnerPolicy{}}))
	t.Cleanup(s.Close)
	conformance.Run(t, s.URL, bearer, conformance.WithVocabulary(authorizer.Vocabulary()), conformance.WithSubjects(alice, bob))
}

func TestNewAuthorizer(t *testing.T) {
	a, err := auth.NewAuthorizer(auth.AuthorizerOptions{Admins: []string{root}})
	if p, ok := a.(*auth.OwnerPolicy); err != nil || !ok || p.Admins[0] != root {
		t.Fatalf("no URL: %T, %v", a, err)
	}
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()), stub.WithAllow(alice))
	a, err = auth.NewAuthorizer(auth.AuthorizerOptions{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.Authorize(t.Context(), authz.Request{Subject: alice, Issuer: issuer, Sub: "alice", Claims: map[string]any{},
		Action: authorizer.ActionAgentCreate, Resource: authz.NewResource(authorizer.KindAgent, "", nil)})
	if err != nil || !d.Allow {
		t.Fatalf("the stub's allow: %+v, %v", d, err)
	}
	if _, err := a.Authorize(t.Context(), authz.Request{Subject: alice, Action: "agent.explode", Resource: authz.NewResource(authorizer.KindAgent, "", nil)}); err == nil {
		t.Fatal("the client sent an action outside the vocabulary")
	}
}

// decider answers from a function, for the guard's rows.
type decider func(authz.Request) (authz.Decision, error)

func (f decider) Authorize(_ context.Context, r authz.Request) (authz.Decision, error) { return f(r) }

// TestGuardAnswers: a denied read answers not_found, a denied mutation
// forbidden when the caller may read the object and not_found when it
// may not, and no decision is authorizer_unavailable.
func TestGuardAnswers(t *testing.T) {
	down := errors.New("connection refused")
	readable := decider(func(r authz.Request) (authz.Decision, error) {
		return authz.Decision{Allow: r.Action == authorizer.ActionSessionRead}, nil
	})
	unreadable := decider(func(authz.Request) (authz.Decision, error) { return authz.Decision{Reason: authz.ReasonNotOwner}, nil })
	readFails := decider(func(r authz.Request) (authz.Decision, error) {
		if r.Action == authorizer.ActionSessionRead {
			return authz.Decision{}, down
		}
		return authz.Decision{}, nil
	})
	unavailable := decider(func(authz.Request) (authz.Decision, error) { return authz.Decision{}, down })
	req := func(action, id string) authz.Request {
		return authz.Request{Subject: bob, Action: action, Resource: authz.NewResource(authorizer.Kind(action), id, nil)}
	}
	for _, row := range []struct {
		name string
		a    authz.Authorizer
		req  authz.Request
		want string
	}{
		{"denied read", unreadable, req(authorizer.ActionSessionRead, "ses_1"), auth.CodeNotFound},
		{"denied mutation, readable", readable, req(authorizer.ActionSessionDelete, "ses_1"), auth.CodeForbidden},
		{"denied mutation, unreadable", unreadable, req(authorizer.ActionSessionDelete, "ses_1"), auth.CodeNotFound},
		{"denied create", unreadable, req(authorizer.ActionSessionCreate, ""), auth.CodeForbidden},
		{"denied list", unreadable, req(authorizer.ActionSessionList, ""), auth.CodeForbidden},
		{"read unavailable", readFails, req(authorizer.ActionSessionEnd, "ses_1"), auth.CodeAuthorizerUnavailable},
		{"unavailable", unavailable, req(authorizer.ActionSessionRead, "ses_1"), auth.CodeAuthorizerUnavailable},
		{"kind without read", unreadable, authz.Request{Subject: bob, Action: "widget.poke", Resource: authz.NewResource("widget", "w_1", nil)}, auth.CodeForbidden},
	} {
		if _, err := (auth.Guard{Authorizer: row.a}).Ask(t.Context(), row.req); code(t, err) != row.want {
			t.Errorf("%s: %v, want %s", row.name, err, row.want)
		}
	}
	if d, err := (auth.Guard{Authorizer: readable}).Ask(t.Context(), req(authorizer.ActionSessionRead, "ses_1")); err != nil || !d.Allow {
		t.Fatalf("an allow: %+v, %v", d, err)
	}
	if auth.Code(down) != "" {
		t.Fatal("a plain error has a code")
	}
}

// TestAForbiddenCarriesTheReason: a forbidden refusal carries the
// authorizer's reason when it is a reason token, and none when the deny
// gave none or gave prose; a refusal answered not_found carries none,
// since it discloses nothing. Disclose keeps the reason only when the
// caller may read the object it asks about.
func TestAForbiddenCarriesTheReason(t *testing.T) {
	reasoned := func(reason string) authz.Authorizer {
		return decider(func(r authz.Request) (authz.Decision, error) {
			return authz.Decision{Allow: r.Action == authorizer.ActionSessionRead && r.Resource.ID == "ses_1", Reason: reason}, nil
		})
	}
	req := func(action, id string) authz.Request {
		return authz.Request{Subject: bob, Action: action, Resource: authz.NewResource(authorizer.Kind(action), id, nil)}
	}
	refusal := func(a authz.Authorizer, r authz.Request) *auth.Error {
		t.Helper()
		_, err := auth.Guard{Authorizer: a}.Ask(t.Context(), r)
		e, ok := errors.AsType[*auth.Error](err)
		if !ok {
			t.Fatalf("%s: %v is not a refusal", r.Action, err)
		}
		return e
	}
	for _, row := range []struct {
		name, reason string
		req          authz.Request
		code, want   string
	}{
		{"a create", "agent_exceeds_initiator", req(authorizer.ActionSessionCreate, ""), auth.CodeForbidden, "agent_exceeds_initiator"},
		{"a list", "agents_not_enabled", req(authorizer.ActionSessionList, ""), auth.CodeForbidden, "agents_not_enabled"},
		{"a readable mutation", "plan_suspended", req(authorizer.ActionSessionEnd, "ses_1"), auth.CodeForbidden, "plan_suspended"},
		{"no reason", "", req(authorizer.ActionSessionCreate, ""), auth.CodeForbidden, ""},
		{"prose", "Alice's agent holds repo.write", req(authorizer.ActionSessionCreate, ""), auth.CodeForbidden, ""},
		{"a denied read", "not_owner", req(authorizer.ActionSessionRead, "ses_2"), auth.CodeNotFound, ""},
		{"an unreadable mutation", "not_owner", req(authorizer.ActionSessionEnd, "ses_2"), auth.CodeNotFound, ""},
	} {
		e := refusal(reasoned(row.reason), row.req)
		if e.Code != row.code || e.Reason != row.want || (row.reason != "" && strings.Contains(e.Message, row.reason)) {
			t.Errorf("%s: %+v, want %s with reason %q", row.name, e, row.code, row.want)
		}
	}

	denied := refusal(reasoned("agent_budget_unassigned"), req(authorizer.ActionSessionCreate, ""))
	read := req(authorizer.ActionAgentRead, "agent_1")
	var asked int
	counting := func(allow bool, err error) authz.Authorizer {
		return decider(func(authz.Request) (authz.Decision, error) {
			asked++
			return authz.Decision{Allow: allow}, err
		})
	}
	for _, row := range []struct {
		name string
		a    authz.Authorizer
		keep bool
	}{
		{"a readable object", counting(true, nil), true},
		{"an unreadable object", counting(false, nil), false},
		{"no answer", counting(true, errors.New("connection refused")), false},
	} {
		got := auth.Guard{Authorizer: row.a}.Disclose(t.Context(), denied, read)
		e, ok := errors.AsType[*auth.Error](got)
		if !ok || e.Code != auth.CodeForbidden || e.Message != denied.Message || (e.Reason != "") != row.keep {
			t.Errorf("%s: %+v", row.name, got)
		}
	}
	if denied.Reason != "agent_budget_unassigned" {
		t.Fatalf("Disclose changed the refusal it was handed: %+v", denied)
	}
	asked = 0
	bare := &auth.Error{Code: auth.CodeForbidden, Message: "the authorizer denied session.create"}
	other := errors.New("disk")
	missing := &auth.Error{Code: auth.CodeNotFound, Reason: "not_owner"}
	for _, err := range []error{bare, other, missing} {
		if got := (auth.Guard{Authorizer: counting(false, nil)}).Disclose(t.Context(), err, read); !errors.Is(got, err) {
			t.Errorf("Disclose(%v) = %v", err, got)
		}
	}
	if asked != 0 {
		t.Fatalf("Disclose asked %d questions of refusals with no reason to withhold", asked)
	}
}

// TestLimitsAtCreate: an allow's limits decode through the guard, and
// limits that do not decode refuse the create as authorizer_unavailable.
func TestLimitsAtCreate(t *testing.T) {
	limits := func(raw string) authz.Authorizer {
		return decider(func(authz.Request) (authz.Decision, error) {
			return authz.Decision{Allow: true, Limits: json.RawMessage(raw)}, nil
		})
	}
	req := authz.Request{Subject: alice, Action: authorizer.ActionSessionCreate, Resource: authz.NewResource(authorizer.KindSession, "", nil)}
	l, err := auth.Guard{Authorizer: limits(`{"budget_usd_micro":1000,"turn_timeout":"10m"}`)}.Create(t.Context(), req)
	if err != nil || *l.BudgetUSDMicro != 1000 || l.TurnTimeout.Minutes() != 10 {
		t.Fatalf("limits %+v, %v", l, err)
	}
	if _, err := (auth.Guard{Authorizer: limits(`{"turn_timeout":"soon"}`)}).Create(t.Context(), req); code(t, err) != auth.CodeAuthorizerUnavailable {
		t.Fatalf("unreadable limits: %v", err)
	}
	deny := decider(func(authz.Request) (authz.Decision, error) { return authz.Decision{}, nil })
	if _, err := (auth.Guard{Authorizer: deny}).Create(t.Context(), req); code(t, err) != auth.CodeForbidden {
		t.Fatalf("a denied create: %v", err)
	}
}

func TestEnvelope(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/sessions", nil)
	r.RemoteAddr = "203.0.113.4:5555"
	r.Header.Set("X-Request-Id", "req_1")
	r.Header.Set("User-Agent", "topos/1")
	c := auth.Caller{Subject: alice, Issuer: issuer, Sub: "alice", Claims: map[string]any{"sub": "alice"}}
	e := auth.Envelope(c, authorizer.ActionSessionCreate, authz.NewResource(authorizer.KindSession, "", nil), r)
	if e.Subject != alice || e.Sub != "alice" || e.Claims["sub"] != "alice" || e.Request != (authz.Caller{ID: "req_1", IP: "203.0.113.4", UserAgent: "topos/1"}) {
		t.Fatalf("Envelope = %+v", e)
	}
	r.RemoteAddr = "pipe"
	if e := auth.Envelope(c, authorizer.ActionSessionCreate, authz.Resource{}, r); e.Request.IP != "pipe" {
		t.Fatalf("an address without a port: %+v", e.Request)
	}
	if e := auth.Envelope(c, authorizer.ActionSessionCreate, authz.Resource{}, nil); e.Request != (authz.Caller{}) {
		t.Fatalf("no request: %+v", e.Request)
	}
}
