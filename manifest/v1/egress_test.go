// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
)

// agentWith is an Agent document whose spec.machine is machine.
func agentWith(machine string) string {
	return "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: a\nspec:\n  model: {name: m}\n  machine: " + machine + "\n"
}

// TestEgressMode: spec.machine.egressMode decodes, reads as allowlist when
// it is absent and stays absent in the resolved spec, so an agent that
// names none keeps its digest; egress hosts with open or none, and a mode
// outside the three, are problems of the manifest (spec 052).
func TestEgressMode(t *testing.T) {
	for _, c := range []struct{ machine, want string }{
		{"{kind: cella}", v1.EgressAllowlist},
		{"{kind: cella, egressMode: allowlist, egress: [api.example.com]}", v1.EgressAllowlist},
		{"{kind: cella, egressMode: open}", v1.EgressOpen},
		{"{kind: cella, egressMode: none}", v1.EgressNone},
	} {
		rs, err := manifest.Resolve(t.Context(), []byte(agentWith(c.machine)), manifest.Options{})
		if err != nil {
			t.Fatalf("%s: %v", c.machine, err)
		}
		m := rs[0].Agent.Spec.Machine
		if m.Mode() != c.want {
			t.Errorf("%s: mode %q, want %q", c.machine, m.Mode(), c.want)
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if named := strings.Contains(c.machine, "egressMode"); strings.Contains(string(b), "egressMode") != named {
			t.Errorf("%s: resolved machine %s", c.machine, b)
		}
	}
	for _, c := range []struct{ machine, path, detail string }{
		{"{kind: cella, egressMode: open, egress: [api.example.com]}", "spec.machine.egress", "set only with egressMode allowlist"},
		{"{kind: cella, egressMode: none, egress: [api.example.com]}", "spec.machine.egress", "set only with egressMode allowlist"},
		{"{kind: cella, egressMode: closed}", "spec.machine.egressMode", "not one of open, allowlist, none"},
	} {
		_, err := manifest.Resolve(t.Context(), []byte(agentWith(c.machine)), manifest.Options{})
		e, ok := errors.AsType[*manifest.Error](err)
		if !ok || e.Code != manifest.CodeInvalidManifest {
			t.Fatalf("%s: %v", c.machine, err)
		}
		found := false
		for _, p := range e.Problems {
			found = found || p.Path == c.path && strings.Contains(p.Detail, c.detail)
		}
		if !found {
			t.Errorf("%s: want %s: %s, got %s", c.machine, c.path, c.detail, e.Detail())
		}
	}
	if manifest.DefaultMachine(v1.Machine{EgressMode: v1.EgressOpen}) {
		t.Error("a machine that names its egress mode is the default machine")
	}
}
