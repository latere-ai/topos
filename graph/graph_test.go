// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package graph_test

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/topos"
	"latere.ai/x/topos/graph"
)

// A fully populated authored graph round-trips through JSON unchanged: the
// snake_case wire form is the persistence contract, so marshalling then
// unmarshalling must reproduce the value exactly.
func TestGraphJSONRoundTrip(t *testing.T) {
	g := graph.Graph{
		Regions: []graph.Region{
			{
				ID:           "plan",
				Coordination: graph.Lead,
				Entry: graph.Agent{
					Name:         "lead",
					Role:         "lead",
					Description:  "coordinates the work",
					SystemPrompt: "you are the lead",
					Tools:        []string{"read"},
					Scopes:       []string{"repo"},
				},
				Peers: []graph.Agent{
					{Name: "reviewer", Role: "review", Description: "reviews diffs", Tools: []string{"read"}},
				},
			},
			{
				ID:           "ship",
				Coordination: graph.Sequence,
				Entry:        graph.Agent{Name: "impl", Role: "impl"},
				Peers:        []graph.Agent{{Name: "commit", Role: "commit"}},
			},
		},
		Edges:           []graph.Edge{{From: "plan", To: "ship"}},
		MaxHandoffDepth: 4,
	}

	data, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back graph.Graph
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	reMarshalled, err := json.Marshal(back)
	if err != nil {
		t.Fatalf("re-Marshal: %v", err)
	}
	if string(data) != string(reMarshalled) {
		t.Errorf("round-trip changed the JSON:\n first: %s\nsecond: %s", data, reMarshalled)
	}
}

// The JSON keys are the exact snake_case contract downstream consumers depend on:
// a region carries a single "coordination" (not autonomy+topology), and an agent
// uses "system_prompt". This test pins that shape.
func TestGraphJSONShape(t *testing.T) {
	g := graph.Graph{
		Regions: []graph.Region{{
			ID:           "r",
			Coordination: graph.Mesh,
			Entry:        graph.Agent{Name: "a", SystemPrompt: "p"},
		}},
	}
	data, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		`"coordination":"mesh"`,
		`"system_prompt":"p"`,
		`"regions":[`,
		`"id":"r"`,
		`"entry":{`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON %s missing %s", got, want)
		}
	}
	// omitempty fields with no value must not appear.
	for _, absent := range []string{"peers", "edges", "max_handoff_depth", "role", "tools", "scopes", "description"} {
		if strings.Contains(got, `"`+absent+`"`) {
			t.Errorf("JSON %s should omit empty %q", got, absent)
		}
	}
}

// ToRuntime maps each coordination mode to the runtime autonomy+topology pair the
// runner switches on: sequence -> pinned (no topology); lead -> dynamic +
// orchestrator-worker; mesh -> dynamic + mesh.
func TestToRuntimeCoordinationMapping(t *testing.T) {
	cases := []struct {
		coord    graph.Coordination
		autonomy topos.Autonomy
		topology topos.Topology
	}{
		{graph.Sequence, topos.Pinned, ""},
		{graph.Lead, topos.Dynamic, topos.OrchestratorWorker},
		{graph.Mesh, topos.Dynamic, topos.Mesh},
	}
	for _, tc := range cases {
		t.Run(string(tc.coord), func(t *testing.T) {
			g := graph.Graph{Regions: []graph.Region{{
				ID:           "r",
				Coordination: tc.coord,
				Entry:        graph.Agent{Name: "entry", Role: "entry"},
				Peers:        []graph.Agent{{Name: "peer"}},
			}}}
			rt, err := g.ToRuntime()
			if err != nil {
				t.Fatalf("ToRuntime: %v", err)
			}
			region := rt.Regions[0].Region
			if region.Autonomy != tc.autonomy {
				t.Errorf("Autonomy = %q, want %q", region.Autonomy, tc.autonomy)
			}
			if region.Topology != tc.topology {
				t.Errorf("Topology = %q, want %q", region.Topology, tc.topology)
			}
		})
	}
}

// ToRuntime carries agents and edges through faithfully: the entry, every peer
// field, and the edge endpoints land on the runtime graph.
func TestToRuntimeCarriesAgentsAndEdges(t *testing.T) {
	g := graph.Graph{
		Regions: []graph.Region{
			{ID: "a", Coordination: graph.Sequence, Entry: graph.Agent{
				Name: "impl", Role: "impl", Description: "d", SystemPrompt: "s",
				Tools: []string{"read", "write"}, Scopes: []string{"repo"},
			}},
			{ID: "b", Coordination: graph.Sequence, Entry: graph.Agent{Name: "test"}},
		},
		Edges: []graph.Edge{{From: "a", To: "b"}},
	}
	rt, err := g.ToRuntime()
	if err != nil {
		t.Fatalf("ToRuntime: %v", err)
	}
	entry := rt.Regions[0].Region.Entry
	want := topos.AgentSpec{
		Name: "impl", Role: "impl", Description: "d", SystemPrompt: "s",
		Tools: []string{"read", "write"}, Scopes: []string{"repo"},
	}
	if entry.Name != want.Name || entry.Role != want.Role || entry.Description != want.Description ||
		entry.SystemPrompt != want.SystemPrompt || len(entry.Tools) != 2 || len(entry.Scopes) != 1 {
		t.Errorf("entry = %+v, want %+v", entry, want)
	}
	if len(rt.Edges) != 1 || rt.Edges[0].From != "a" || rt.Edges[0].To != "b" {
		t.Errorf("edges = %+v, want a->b", rt.Edges)
	}
}

func TestToRuntimeErrors(t *testing.T) {
	cases := []struct {
		name    string
		g       graph.Graph
		wantSub string
	}{
		{
			name:    "no regions",
			g:       graph.Graph{},
			wantSub: "no regions",
		},
		{
			name:    "region without id",
			g:       graph.Graph{Regions: []graph.Region{{Coordination: graph.Sequence, Entry: graph.Agent{Name: "a"}}}},
			wantSub: "no id",
		},
		{
			name:    "region without entry name",
			g:       graph.Graph{Regions: []graph.Region{{ID: "r", Coordination: graph.Sequence}}},
			wantSub: "no entry agent",
		},
		{
			name:    "empty coordination",
			g:       graph.Graph{Regions: []graph.Region{{ID: "r", Entry: graph.Agent{Name: "a"}}}},
			wantSub: "coordination is required",
		},
		{
			name:    "unknown coordination",
			g:       graph.Graph{Regions: []graph.Region{{ID: "r", Coordination: "swarm", Entry: graph.Agent{Name: "a"}}}},
			wantSub: "unknown coordination",
		},
		{
			name: "edge missing endpoint",
			g: graph.Graph{
				Regions: []graph.Region{{ID: "r", Coordination: graph.Sequence, Entry: graph.Agent{Name: "a"}}},
				Edges:   []graph.Edge{{From: "r", To: ""}},
			},
			wantSub: "both from and to",
		},
		{
			name: "fan-in rejected by structural validation",
			g: graph.Graph{
				Regions: []graph.Region{
					{ID: "a", Coordination: graph.Sequence, Entry: graph.Agent{Name: "a"}},
					{ID: "b", Coordination: graph.Sequence, Entry: graph.Agent{Name: "b"}},
					{ID: "c", Coordination: graph.Sequence, Entry: graph.Agent{Name: "c"}},
				},
				Edges: []graph.Edge{{From: "a", To: "c"}, {From: "b", To: "c"}},
			},
			wantSub: "fan-in",
		},
		{
			name: "cycle rejected by structural validation",
			g: graph.Graph{
				Regions: []graph.Region{
					{ID: "a", Coordination: graph.Sequence, Entry: graph.Agent{Name: "a"}},
					{ID: "b", Coordination: graph.Sequence, Entry: graph.Agent{Name: "b"}},
				},
				Edges: []graph.Edge{{From: "a", To: "b"}, {From: "b", To: "a"}},
			},
			wantSub: "cycle",
		},
		{
			name: "duplicate region id rejected by structural validation",
			g: graph.Graph{
				Regions: []graph.Region{
					{ID: "a", Coordination: graph.Sequence, Entry: graph.Agent{Name: "a"}},
					{ID: "a", Coordination: graph.Sequence, Entry: graph.Agent{Name: "b"}},
				},
			},
			wantSub: "duplicate region",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.g.ToRuntime()
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("ToRuntime error = %v, want containing %q", err, tc.wantSub)
			}
		})
	}
}

// A graph decoded from JSON lowers to a runtime graph that RunGraph accepts: the
// authored contract is runnable end to end. ValidateGraph is the same gate
// RunGraph applies, so a graph that ToRuntime returns cleanly will not fail at run
// time with a configuration error.
func TestDecodedGraphIsRunnable(t *testing.T) {
	const raw = `{
		"regions": [
			{"id": "plan", "coordination": "lead",
			 "entry": {"name": "lead", "tools": ["read"]},
			 "peers": [{"name": "reviewer", "role": "review"}]},
			{"id": "ship", "coordination": "sequence",
			 "entry": {"name": "impl"}, "peers": [{"name": "commit"}]}
		],
		"edges": [{"from": "plan", "to": "ship"}]
	}`
	var g graph.Graph
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	rt, err := g.ToRuntime()
	if err != nil {
		t.Fatalf("ToRuntime: %v", err)
	}
	if err := topos.ValidateGraph(rt); err != nil {
		t.Fatalf("runtime graph invalid: %v", err)
	}
	if len(rt.Regions) != 2 {
		t.Errorf("regions = %d, want 2", len(rt.Regions))
	}
}
