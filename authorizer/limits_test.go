// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
)

func decision(t *testing.T, w any) authz.Decision {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return authz.Decision{Allow: true, Limits: raw}
}

func TestDecodeLimitsReadsEveryMember(t *testing.T) {
	budget := int64(2_000_000)
	w := WireLimits{
		AlwaysConfirm:  []string{"bash(git push*)"},
		AlwaysAllow:    []string{"read"},
		Thresholds:     &Thresholds{FlagAt: 0.2, AskAt: 0.4, BlockAt: 0.8},
		BudgetUSDMicro: &budget,
		TurnTimeout:    "30m",
		MaxAge:         "72h",
		Scope:          []json.RawMessage{json.RawMessage(`{"action":"repo.push","resource":"*"}`)},
		Retention:      "720h",
	}
	l, err := DecodeLimits(decision(t, w))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.AlwaysConfirm, w.AlwaysConfirm) || !slices.Equal(l.AlwaysAllow, w.AlwaysAllow) || *l.Thresholds != *w.Thresholds ||
		*l.BudgetUSDMicro != budget || l.TurnTimeout != 30*time.Minute || l.MaxAge != 72*time.Hour || l.Retention != 720*time.Hour || len(l.Scope) != 1 {
		t.Fatalf("DecodeLimits = %+v", l)
	}
	none, err := DecodeLimits(authz.Decision{Allow: true})
	if err != nil || none.BudgetUSDMicro != nil || none.Thresholds != nil || none.TurnTimeout != 0 {
		t.Fatalf("no limits decoded to %+v, %v", none, err)
	}
	zero := int64(0)
	if l, err := DecodeLimits(decision(t, WireLimits{BudgetUSDMicro: &zero})); err != nil || l.BudgetUSDMicro == nil || *l.BudgetUSDMicro != 0 {
		t.Fatalf("a zero ceiling is a ceiling: %+v, %v", l, err)
	}
	if _, err := DecodeLimits(decision(t, map[string]any{"a_member_from_later": 1})); err != nil {
		t.Fatalf("an unknown member: %v", err)
	}
}

func TestDecodeLimitsRefusesWhatItCannotApply(t *testing.T) {
	neg := int64(-1)
	for name, w := range map[string]any{
		"negative budget":   WireLimits{BudgetUSDMicro: &neg},
		"bad duration":      WireLimits{TurnTimeout: "soon"},
		"zero duration":     WireLimits{MaxAge: "0s"},
		"negative duration": WireLimits{Retention: "-1h"},
		"unordered":         WireLimits{Thresholds: &Thresholds{FlagAt: 0.6, AskAt: 0.4, BlockAt: 0.9}},
		"above one":         WireLimits{Thresholds: &Thresholds{FlagAt: 0.1, AskAt: 0.4, BlockAt: 1.5}},
		"scope entry":       WireLimits{Scope: []json.RawMessage{json.RawMessage(`"repo.push"`)}},
		"wrong type":        map[string]any{"turn_timeout": 30},
	} {
		if _, err := DecodeLimits(decision(t, w)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
	if _, err := DecodeLimits(authz.Decision{Allow: true, Limits: json.RawMessage(`[`)}); err == nil || !strings.Contains(err.Error(), "limits") {
		t.Errorf("a malformed object: %v", err)
	}
}
