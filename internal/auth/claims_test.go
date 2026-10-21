// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"encoding/json"
	"testing"

	"latere.ai/x/topos/internal/auth"
)

// TestTheContextIsTheTokensOrganization: a caller's context is the
// org_id its token carries, "" when it carries none or one that is not a
// string, and a trigger keeps that claim alone.
func TestTheContextIsTheTokensOrganization(t *testing.T) {
	for _, c := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"org_id": "acme", "roles": []any{"admin"}}, "acme"},
		{map[string]any{"org_id": ""}, ""},
		{map[string]any{"org_id": 7}, ""},
		{nil, ""},
	} {
		caller := auth.Caller{Subject: "https://issuer.test|alice", Claims: c.claims}
		if got := caller.Organization(); got != c.want {
			t.Errorf("%v: %q, want %q", c.claims, got, c.want)
		}
		if kept := auth.TriggerClaims(caller); len(kept) != 1 || kept["org_id"] != c.want {
			t.Errorf("%v: a trigger keeps %v", c.claims, kept)
		}
	}
}

// TestFiredByIsTheKeysClaimsVerbatim: the fired_by of a firing is the
// fire request's token_use and authorization_details claims as the
// token carried them, the grants left out when the token names none, and
// nil for a token whose token_use is empty or not a string, which no key
// minted. Any token_use but empty passes, since what it means is the
// authorizer's.
func TestFiredByIsTheKeysClaimsVerbatim(t *testing.T) {
	grants := []any{map[string]any{"type": "latere-authz", "actions": []any{"topos:trigger.fire"}}}
	for _, c := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"sub": "alice", "org_id": "acme", "token_use": "pat", "authorization_details": grants}, `{"authorization_details":[{"actions":["topos:trigger.fire"],"type":"latere-authz"}],"token_use":"pat"}`},
		{map[string]any{"token_use": "sak", "authorization_details": "everything"}, `{"authorization_details":"everything","token_use":"sak"}`},
		{map[string]any{"token_use": "pat"}, `{"token_use":"pat"}`},
		{map[string]any{"token_use": "elsewhere", "authorization_details": nil}, `{"authorization_details":null,"token_use":"elsewhere"}`},
		{map[string]any{"token_use": "", "authorization_details": grants}, `null`},
		{map[string]any{"token_use": 7, "authorization_details": grants}, `null`},
		{map[string]any{"sub": "alice", "org_id": "acme"}, `null`},
		{nil, `null`},
	} {
		got, err := json.Marshal(auth.FiredBy(auth.Caller{Subject: "https://issuer.test|alice", Claims: c.claims}))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("%v: %s, want %s", c.claims, got, c.want)
		}
	}
}

// TestClientOf: the client a sender records is the token's client_id,
// its azp where it carries no client_id, and "" for a token that names
// neither or names one that is not a string (spec 085).
func TestClientOf(t *testing.T) {
	for _, c := range []struct {
		claims map[string]any
		want   string
	}{
		{map[string]any{"client_id": "web", "azp": "other"}, "web"},
		{map[string]any{"azp": "desk"}, "desk"},
		{map[string]any{"client_id": "", "azp": "desk"}, "desk"},
		{map[string]any{"client_id": 7}, ""},
		{map[string]any{}, ""},
		{nil, ""},
	} {
		if got := auth.ClientOf(auth.Caller{Claims: c.claims}); got != c.want {
			t.Errorf("ClientOf(%v) = %q, want %q", c.claims, got, c.want)
		}
	}
}
