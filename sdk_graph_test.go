// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/models"
)

// echoBrain finishes every turn (no delegation) and returns "seen:" prefixed to
// the last user prompt it received. That makes a region's Final a function of its
// input task, so a graph edge's data flow (source Final -> target task) is
// observable in the target region's output.
type echoBrain struct{}

func (echoBrain) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
		}
	}
	return &cannedStream{events: endTurn("seen:" + prompt)}, nil
}

func pinnedRegion(name string) Region {
	return Region{Autonomy: Pinned, Entry: AgentSpec{Name: name, Role: name}}
}

// A graph edge threads the source region's Final into the target region's task, so
// a two-region chain compounds the echo prefix once per region.
func TestRunGraphThreadsFinalAlongEdges(t *testing.T) {
	r := newTestRunner(t, echoBrain{})
	g := Graph{
		Regions: []GraphRegion{
			{ID: "a", Region: pinnedRegion("impl")},
			{ID: "b", Region: pinnedRegion("impl")}, // same agent name as region a
		},
		Edges: []GraphEdge{{From: "a", To: "b"}},
	}
	res, err := r.RunGraph(context.Background(), g, "task")
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	// a: input "task" -> "seen:task"; b: input "seen:task" -> "seen:seen:task".
	if res.Final != "seen:seen:task" {
		t.Errorf("Final = %q, want %q", res.Final, "seen:seen:task")
	}
}

// Region ids namespace node ids, so agents sharing a Name across regions do not
// collide, and a cross-region EdgeNext links the two region entries.
func TestRunGraphNamespacesIdsAndLinksRegions(t *testing.T) {
	r := newTestRunner(t, echoBrain{})
	g := Graph{
		Regions: []GraphRegion{
			{ID: "a", Region: pinnedRegion("impl")},
			{ID: "b", Region: pinnedRegion("impl")},
		},
		Edges: []GraphEdge{{From: "a", To: "b"}},
	}
	res, err := r.RunGraph(context.Background(), g, "task")
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	ids := map[string]bool{}
	for _, n := range res.Lineage.Nodes {
		if ids[n.ID] {
			t.Errorf("duplicate node id %q", n.ID)
		}
		ids[n.ID] = true
		if n.Status != StatusDone {
			t.Errorf("node %q status = %q, want done", n.ID, n.Status)
		}
	}
	if !ids["run-1/a/impl"] || !ids["run-1/b/impl"] {
		t.Errorf("missing namespaced ids; got %v", ids)
	}
	want := LineageEdge{From: "run-1/a/impl", To: "run-1/b/impl", Kind: EdgeNext}
	if !hasEdge(res.Lineage.Edges, want) {
		t.Errorf("missing cross-region edge %+v in %+v", want, res.Lineage.Edges)
	}
}

// A graph mixes a dynamic region into a pinned chain: both regions' nodes appear
// in the merged lineage and Final is the last region's output.
func TestRunGraphMixesDynamicAndPinned(t *testing.T) {
	r := newTestRunner(t, echoBrain{}) // echoBrain never delegates, so dynamic entry just finishes
	g := Graph{
		Regions: []GraphRegion{
			{ID: "plan", Region: Region{Autonomy: Dynamic, Entry: AgentSpec{Name: "lead", Role: "lead"}}},
			{ID: "ship", Region: pinnedRegion("commit")},
		},
		Edges: []GraphEdge{{From: "plan", To: "ship"}},
	}
	res, err := r.RunGraph(context.Background(), g, "go")
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	var names []string
	for _, n := range res.Lineage.Nodes {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "lead,commit" {
		t.Errorf("node names = %v, want [lead commit]", names)
	}
	if res.Final != "seen:seen:go" {
		t.Errorf("Final = %q, want %q", res.Final, "seen:seen:go")
	}
}

func TestRunGraphRejectsBadTopology(t *testing.T) {
	r := newTestRunner(t, echoBrain{})
	cases := []struct {
		name string
		g    Graph
		want string
	}{
		{"empty", Graph{}, "no regions"},
		{"empty id", Graph{Regions: []GraphRegion{{ID: "", Region: pinnedRegion("x")}}}, "empty id"},
		{"duplicate id", Graph{Regions: []GraphRegion{
			{ID: "a", Region: pinnedRegion("x")}, {ID: "a", Region: pinnedRegion("y")},
		}}, "duplicate region id"},
		{"unknown from", Graph{
			Regions: []GraphRegion{{ID: "a", Region: pinnedRegion("x")}},
			Edges:   []GraphEdge{{From: "z", To: "a"}},
		}, "unknown region"},
		{"unknown to", Graph{
			Regions: []GraphRegion{{ID: "a", Region: pinnedRegion("x")}},
			Edges:   []GraphEdge{{From: "a", To: "z"}},
		}, "unknown region"},
		{"self edge", Graph{
			Regions: []GraphRegion{{ID: "a", Region: pinnedRegion("x")}},
			Edges:   []GraphEdge{{From: "a", To: "a"}},
		}, "self edge"},
		{"cycle", Graph{
			Regions: []GraphRegion{{ID: "a", Region: pinnedRegion("x")}, {ID: "b", Region: pinnedRegion("y")}},
			Edges:   []GraphEdge{{From: "a", To: "b"}, {From: "b", To: "a"}},
		}, "cycle"},
		{"fan-in", Graph{
			Regions: []GraphRegion{
				{ID: "a", Region: pinnedRegion("x")},
				{ID: "b", Region: pinnedRegion("y")},
				{ID: "c", Region: pinnedRegion("z")},
			},
			Edges: []GraphEdge{{From: "a", To: "c"}, {From: "b", To: "c"}},
		}, "fan-in is not yet supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunGraph(context.Background(), tc.g, "task")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

// Fan-out (one source, two independent targets) is a valid graph: both targets run
// and receive the source Final.
func TestRunGraphFanOut(t *testing.T) {
	r := newTestRunner(t, echoBrain{})
	g := Graph{
		Regions: []GraphRegion{
			{ID: "root", Region: pinnedRegion("root")},
			{ID: "left", Region: pinnedRegion("left")},
			{ID: "right", Region: pinnedRegion("right")},
		},
		Edges: []GraphEdge{{From: "root", To: "left"}, {From: "root", To: "right"}},
	}
	res, err := r.RunGraph(context.Background(), g, "x")
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if len(res.Lineage.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(res.Lineage.Nodes))
	}
	for _, want := range []LineageEdge{
		{From: "run-1/root/root", To: "run-1/left/left", Kind: EdgeNext},
		{From: "run-1/root/root", To: "run-1/right/right", Kind: EdgeNext},
	} {
		if !hasEdge(res.Lineage.Edges, want) {
			t.Errorf("missing fan-out edge %+v", want)
		}
	}
}

// A failing region aborts the graph: RunGraph returns an error naming the region
// and the partial lineage accumulated so far (the failed region's node marked
// failed), so a consumer can render how far the run got.
func TestRunGraphReportsFailingRegion(t *testing.T) {
	r := newTestRunner(t, failBrain{})
	g := Graph{
		Regions: []GraphRegion{{ID: "boom", Region: pinnedRegion("impl")}},
	}
	res, err := r.RunGraph(context.Background(), g, "task")
	if err == nil || !strings.Contains(err.Error(), `region "boom"`) {
		t.Fatalf("err = %v, want naming region boom", err)
	}
	if len(res.Lineage.Nodes) != 1 || res.Lineage.Nodes[0].Status != StatusFailed {
		t.Errorf("lineage = %+v, want one failed node", res.Lineage.Nodes)
	}
}

func hasEdge(edges []LineageEdge, want LineageEdge) bool {
	return slices.Contains(edges, want)
}
