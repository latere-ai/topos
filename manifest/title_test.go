// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"strings"
	"testing"
)

// TestAnAgentTurnsTitlesOn: spec.title decodes with and without a model,
// is kept in the resolved spec, which it versions, and an agent without
// it holds none; a model that is empty once trimmed, and a member the
// object does not define, are refused at their path (spec 083).
func TestAnAgentTurnsTitlesOn(t *testing.T) {
	m := "model: {name: claude-haiku-4-5}"
	plain := one(t, agent("a", m), Options{})
	if plain.Agent.Spec.Title != nil || strings.Contains(string(plain.Spec), `"title"`) {
		t.Fatalf("an agent without spec.title holds %+v: %s", plain.Agent.Spec.Title, plain.Spec)
	}
	on := one(t, agent("a", m, "title: {}"), Options{})
	if on.Agent.Spec.Title == nil || on.Agent.Spec.Title.Model != nil || !strings.Contains(string(on.Spec), `"title":{}`) {
		t.Fatalf("title {} resolved to %+v: %s", on.Agent.Spec.Title, on.Spec)
	}
	named := one(t, agent("a", m, "title: {model: tier/quick}"), Options{})
	if tl := named.Agent.Spec.Title; tl == nil || tl.Model == nil || *tl.Model != "tier/quick" {
		t.Fatalf("title with a model resolved to %+v", tl)
	}
	if plain.Digest == on.Digest || on.Digest == named.Digest {
		t.Fatal("spec.title does not change the agent's digest")
	}
	for _, c := range []struct{ line, path, detail string }{
		{"title: {model: ''}", "spec.title.model", "empty"},
		{"title: {model: '  '}", "spec.title.model", "empty"},
		{"title: {model: 7}", "spec.title.model", "want a string, got a number"},
		{"title: {name: quick}", "spec.title.name", "unknown field"},
		{"title: on", "spec.title", "want a mapping"},
	} {
		_, err := Resolve(t.Context(), []byte(agent("a", m, c.line)), Options{})
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Code != CodeInvalidManifest || !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: want %s: %s, got %v", c.line, c.path, c.detail, err)
		}
	}
}
