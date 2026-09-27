// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"slices"
	"testing"

	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
)

// TestAgentModelsNameEveryModelOnce: the models an agent's sessions call
// are its own, its advisor's and its subagents', inline and pinned, each
// once.
func TestAgentModelsNameEveryModelOnce(t *testing.T) {
	sub := v1.AgentSpec{Model: v1.AgentModel{Name: "m-sub"}}
	pinned := &v1.Agent{Spec: v1.AgentSpec{Model: v1.AgentModel{Name: "m-pinned"}, Advisor: &v1.Advisor{Model: v1.AgentModel{Name: "m"}}}}
	r := manifest.Resolved{
		Agent: &v1.Agent{Spec: v1.AgentSpec{
			Model:     v1.AgentModel{Name: "m"},
			Advisor:   &v1.Advisor{Model: v1.AgentModel{Name: "m-advisor"}},
			Subagents: []v1.Subagent{{Name: "a", Spec: &sub}, {Name: "b", Agent: "agent_01@1"}},
		}},
		Pinned: map[string]*v1.Agent{"agent_01@1": pinned},
	}
	if got := agentModels(r); !slices.Equal(got, []string{"m", "m-advisor", "m-sub", "m-pinned"}) {
		t.Fatalf("models %v", got)
	}
	if got := agentModels(manifest.Resolved{}); got != nil {
		t.Fatalf("no agent: %v", got)
	}
}
