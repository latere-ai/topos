// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"strings"
	"testing"

	v1 "latere.ai/x/topos/manifest/v1"
)

// TestTheReasoningLevelIsReadUnderEitherName: a manifest names a model's
// reasoning level as reasoning, or as effort, the name it had before
// (spec 048). Either resolves to the bytes a version stored before the
// rename was hashed over, with the level under effort, so an agent stored
// by an earlier release and applied again under the new name keeps its
// version.
func TestTheReasoningLevelIsReadUnderEitherName(t *testing.T) {
	s := newStore()
	const rest = "instructions: Review the diff."
	stored := one(t, agent("reviewer", "model: {name: m, effort: high}", rest), fixed(s))
	s.apply([]Resolved{stored})
	if !strings.Contains(string(stored.Spec), `"effort":"high"`) || strings.Contains(string(stored.Spec), `"reasoning"`) {
		t.Fatalf("the stored spec renders %s", stored.Spec)
	}
	o := fixed(s)
	o.NewID = func(string) string { t.Fatal("a stored agent took a new id"); return "" }
	for name, model := range map[string]string{
		"reasoning":           "model: {name: m, reasoning: high}",
		"both, the same":      "model: {name: m, reasoning: high, effort: high}",
		"effort, as it was":   "model: {name: m, effort: high}",
		"reasoning, restyled": "model:\n    name: m\n    reasoning: high",
	} {
		r := one(t, agent("reviewer", model, rest), o)
		if r.Agent.Status != stored.Agent.Status || string(r.Spec) != string(stored.Spec) {
			t.Errorf("%s: resolves to %+v %s, want %+v %s", name, r.Agent.Status, r.Spec, stored.Agent.Status, stored.Spec)
		}
		if m := r.Agent.Spec.Model; m.Effort != "high" || m.Reasoning != "" {
			t.Errorf("%s: the resolved model is %+v", name, m)
		}
	}
	// The resolved spec reads back from its bundle with its digest.
	bundle, err := stored.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadBundle(bundle)
	if err != nil || back.Digest != stored.Digest {
		t.Fatalf("ReadBundle = %+v, %v", back.Digest, err)
	}
}

// TestEveryModelOfASpecHoldsItsLevelUnderEffort: the advisor's model and an
// inline subagent's are resolved as the agent's own is.
func TestEveryModelOfASpecHoldsItsLevelUnderEffort(t *testing.T) {
	r := one(t, agent("lead",
		"model: {name: m, reasoning: low}",
		"advisor: {model: {name: a, reasoning: high}}",
		"subagents: [{name: helper, spec: {model: {name: s, reasoning: minimal}, instructions: Help.}}]",
	), fixed(newStore()))
	spec := r.Agent.Spec
	if spec.Model.Effort != "low" || spec.Advisor.Model.Effort != "high" || spec.Subagents[0].Spec.Model.Effort != "minimal" {
		t.Fatalf("the resolved models are %+v, %+v, %+v", spec.Model, spec.Advisor.Model, spec.Subagents[0].Spec.Model)
	}
	if strings.Contains(string(r.Spec), `"reasoning"`) {
		t.Fatalf("the rendered spec names reasoning: %s", r.Spec)
	}
	answered := spec.Answered()
	if answered.Model.Reasoning != "low" || answered.Advisor.Model.Reasoning != "high" || answered.Subagents[0].Spec.Model.Reasoning != "minimal" ||
		answered.Model.Effort != "" || answered.Advisor.Model.Effort != "" || answered.Subagents[0].Spec.Model.Effort != "" {
		t.Fatalf("the answered models are %+v, %+v, %+v", answered.Model, answered.Advisor.Model, answered.Subagents[0].Spec.Model)
	}
	if spec.Model.Effort != "low" || spec.Advisor.Model.Effort != "high" || spec.Subagents[0].Spec.Model.Effort != "minimal" {
		t.Fatalf("answering changed the spec: %+v", spec)
	}
}

// TestTwoLevelsForOneModelAreRefused: a model that names both names with
// different levels, or a level outside the four, is an invalid manifest,
// and the problem names the field the manifest wrote.
func TestTwoLevelsForOneModelAreRefused(t *testing.T) {
	for _, c := range []struct{ body, path, detail string }{
		{agent("a", "model: {name: m, reasoning: high, effort: low}"), "spec.model.reasoning", `"high", and effort names "low"`},
		{agent("a", "model: {name: m, reasoning: max}"), "spec.model.reasoning", "not one of minimal"},
		{agent("a", "model: {name: m}", "advisor: {model: {name: a, reasoning: low, effort: high}}"), "spec.advisor.model.reasoning", "name the level once"},
		{agent("a", "model: {name: m}", "subagents: [{name: h, spec: {model: {name: s, reasoning: low, effort: high}, instructions: x}}]"), "spec.subagents[0].spec.model.reasoning", "name the level once"},
	} {
		e := refused(t, CodeInvalidManifest, c.body, fixed(newStore()))
		if !hasProblem(e, c.path, c.detail) {
			t.Errorf("%s: problems %+v", c.body, e.Problems)
		}
	}
}

// TestALevelReadsUnderEitherName: Level reads reasoning, and effort where
// reasoning is empty; Stored and Answered move it between the two.
func TestALevelReadsUnderEitherName(t *testing.T) {
	for _, m := range []v1.AgentModel{{Name: "m", Effort: "high"}, {Name: "m", Reasoning: "high"}, {Name: "m", Effort: "high", Reasoning: "high"}} {
		if m.Level() != "high" || m.Stored() != (v1.AgentModel{Name: "m", Effort: "high"}) || m.Answered() != (v1.AgentModel{Name: "m", Reasoning: "high"}) {
			t.Errorf("%+v: level %q, stored %+v, answered %+v", m, m.Level(), m.Stored(), m.Answered())
		}
	}
	if own := (v1.AgentModel{Name: "m"}); own.Level() != "" || own.Stored() != own || own.Answered() != own {
		t.Errorf("the model's own default moved: %+v", own)
	}
}
