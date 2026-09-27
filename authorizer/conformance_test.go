// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/authz/conformance"
	"latere.ai/x/pkg/authz/server"
	"latere.ai/x/pkg/authz/stub"

	"latere.ai/x/topos/authorizer"
)

const (
	issuer = "https://login.example.com"
	alice  = issuer + "|alice"
	bob    = issuer + "|bob"
	bearer = "conformance-bearer"
)

// TestAuthorizerConformanceStub: the shared stub told Topos's table
// passes the conformance suite driven from that same table.
func TestAuthorizerConformanceStub(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	conformance.Run(t, s.URL(), s.Token(),
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))
}

// TestScaffoldSpeaksTheVocabulary: an endpoint written on
// latere.ai/x/pkg/authz/server with the vocabulary and a decider of a
// dozen lines passes the same suite, and its limits decode as toposd
// reads them.
func TestScaffoldSpeaksTheVocabulary(t *testing.T) {
	h := server.New(server.Options{Bearer: bearer, Vocabulary: authorizer.Vocabulary(), Decider: owners{}})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	conformance.Run(t, s.URL, bearer,
		conformance.WithVocabulary(authorizer.Vocabulary()),
		conformance.WithSubjects(alice, bob))

	c, err := authz.NewClient(authz.Options{URL: s.URL, Token: bearer, HTTP: &http.Client{}, Vocabulary: authorizer.Vocabulary()})
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Authorize(t.Context(), authz.Request{Subject: alice, Issuer: issuer, Sub: "alice", Claims: map[string]any{},
		Action: authorizer.ActionSessionCreate, Resource: authz.NewResource(authorizer.KindSession, "", map[string]any{"initiator": alice})})
	if err != nil || !d.Allow {
		t.Fatalf("session.create: %+v, %v", d, err)
	}
	l, err := authorizer.DecodeLimits(d)
	if err != nil || l.BudgetUSDMicro == nil || *l.BudgetUSDMicro != 5_000_000 {
		t.Fatalf("the allow's limits: %+v, %v", l, err)
	}
}

// TestClientRefusesAnActionOutsideTheTable: a typo costs no round trip
// and is named as Topos's own mistake.
func TestClientRefusesAnActionOutsideTheTable(t *testing.T) {
	s := stub.New(t, stub.WithVocabulary(authorizer.Vocabulary()))
	c, err := authz.NewClient(authz.Options{URL: s.URL(), Token: s.Token(), HTTP: &http.Client{}, Vocabulary: authorizer.Vocabulary()})
	if err != nil {
		t.Fatal(err)
	}
	var unknown *authz.UnknownAction
	_, err = c.Authorize(t.Context(), authz.Request{Subject: alice, Action: "session.explode", Resource: authz.NewResource(authorizer.KindSession, "ses_x", nil)})
	if !errors.As(err, &unknown) || unknown.Core != authorizer.Core || len(s.Requests()) != 0 {
		t.Fatalf("Authorize(session.explode) = %v after %d requests", err, len(s.Requests()))
	}
}

// owners is the owner frame over the owner the resource carries, with a
// budget ceiling on every allow of session.create.
type owners struct{}

func (owners) Decide(_ context.Context, req authz.Request) (authz.Decision, error) {
	if authz.IsList(req.Action) {
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{req.Subject}}}, nil
	}
	obj := authz.Object{}
	if owner := req.Resource.String("owner"); owner != "" {
		obj = authz.Object{Exists: true, Owner: owner}
	}
	d := authz.Policy{Create: authorizer.Create(authorizer.Kind(req.Action))}.Decide(req, obj)
	if d.Allow && req.Action == authorizer.ActionSessionCreate {
		budget := int64(5_000_000)
		raw, err := json.Marshal(authorizer.WireLimits{BudgetUSDMicro: &budget})
		if err != nil {
			return authz.Decision{}, err
		}
		d.Limits = raw
	}
	return d, nil
}
