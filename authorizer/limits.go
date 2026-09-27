// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package authorizer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/pkg/authz"
)

// Thresholds are the progressive permission mode's score cut-offs
// (spec 012), each a risk score between 0 and 1.
type Thresholds struct {
	FlagAt  float64 `json:"flag_at"`
	AskAt   float64 `json:"ask_at"`
	BlockAt float64 `json:"block_at"`
}

// Limits are what an allow of session.create granted, decoded from the
// answer's limits object. The session takes the lowest of each against
// the agent's and the request's own (spec 005). A member the answer left
// out is its zero here: no list, no thresholds, no ceiling.
type Limits struct {
	// AlwaysConfirm and AlwaysAllow are the organization's permission
	// patterns (spec 012).
	AlwaysConfirm []string
	AlwaysAllow   []string
	// Thresholds replaces the mode's cut-offs; nil keeps them.
	Thresholds *Thresholds
	// BudgetUSDMicro is a ceiling on the session's spend in micro-USD;
	// nil is no ceiling, and a ceiling of zero allows no model request.
	BudgetUSDMicro *int64
	// TurnTimeout and MaxAge are ceilings; zero is none.
	TurnTimeout time.Duration
	MaxAge      time.Duration
	// Scope is the session's starting scope, one grant per entry in the
	// grammar of an agent's permissions (spec 018).
	Scope []json.RawMessage
	// Retention is how long the session is kept after it ends; zero keeps
	// it until it is deleted.
	Retention time.Duration
}

// WireLimits is the limits object as an answer carries it, so an
// authorizer renders its answer through the type toposd decodes. Every
// member is optional and a member left nil is left out.
type WireLimits struct {
	AlwaysConfirm  []string          `json:"always_confirm,omitempty"`
	AlwaysAllow    []string          `json:"always_allow,omitempty"`
	Thresholds     *Thresholds       `json:"thresholds,omitempty"`
	BudgetUSDMicro *int64            `json:"budget_usd_micro,omitempty"`
	TurnTimeout    string            `json:"turn_timeout,omitempty"`
	MaxAge         string            `json:"max_age,omitempty"`
	Scope          []json.RawMessage `json:"scope,omitempty"`
	Retention      string            `json:"retention,omitempty"`
}

// DecodeLimits reads a decision's limits object. A decision with none is
// the zero Limits, and a member toposd does not know is ignored. A known
// member that does not decode is an error, and toposd refuses the create
// as authorizer_unavailable: a ceiling it cannot read is one it cannot
// apply.
func DecodeLimits(d authz.Decision) (Limits, error) {
	var w WireLimits
	if err := d.DecodeLimits(&w); err != nil {
		return Limits{}, fmt.Errorf("limits: %w", err)
	}
	l := Limits{AlwaysConfirm: w.AlwaysConfirm, AlwaysAllow: w.AlwaysAllow, Scope: w.Scope}
	if t := w.Thresholds; t != nil {
		if t.FlagAt < 0 || t.FlagAt > t.AskAt || t.AskAt > t.BlockAt || t.BlockAt > 1 {
			return Limits{}, fmt.Errorf("limits.thresholds %+v are not 0 <= flag_at <= ask_at <= block_at <= 1", *t)
		}
		l.Thresholds = t
	}
	if b := w.BudgetUSDMicro; b != nil {
		if *b < 0 {
			return Limits{}, fmt.Errorf("limits.budget_usd_micro is %d, below zero", *b)
		}
		l.BudgetUSDMicro = b
	}
	for _, f := range []struct {
		name string
		src  string
		dst  *time.Duration
	}{
		{"turn_timeout", w.TurnTimeout, &l.TurnTimeout},
		{"max_age", w.MaxAge, &l.MaxAge},
		{"retention", w.Retention, &l.Retention},
	} {
		if f.src == "" {
			continue
		}
		v, err := time.ParseDuration(f.src)
		if err != nil {
			return Limits{}, fmt.Errorf("limits.%s: %w", f.name, err)
		}
		if v <= 0 {
			return Limits{}, fmt.Errorf("limits.%s is %s, not a positive duration", f.name, f.src)
		}
		*f.dst = v
	}
	for i, g := range w.Scope {
		if t := bytes.TrimSpace(g); len(t) == 0 || t[0] != '{' {
			return Limits{}, fmt.Errorf("limits.scope[%d] is not a grant object", i)
		}
	}
	return l, nil
}
