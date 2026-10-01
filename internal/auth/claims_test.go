// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
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
