// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	"latere.ai/x/topos/billing"
	"latere.ai/x/topos/models"
)

// delegatingSpender delegates once to a fixed peer while it holds the delegate
// tool, finishes after the peer returns, and spends a fixed usage on every turn
// it takes — entry turns and peer turns alike. It counts its turns so a test can
// assert how much of a region ran before the shared cap stopped it.
type delegatingSpender struct {
	usage models.Usage
	peer  string
	turns atomic.Int32
}

func (b *delegatingSpender) Stream(_ context.Context, req models.Request) (models.Stream, error) {
	b.turns.Add(1)
	u := b.usage
	// A prior tool result means the delegate already returned — finish.
	for _, m := range req.Messages {
		if m.Role == models.RoleTool {
			return &budgetStream{events: []models.Event{
				{Kind: models.KindTextDelta, TextDelta: "wrapping up"},
				{Kind: models.KindUsage, Usage: &u},
				{Kind: models.KindDone, StopReason: models.StopEndTurn},
			}}, nil
		}
	}
	// Holding a delegate tool (the entry agent) — delegate.
	for _, td := range req.Tools {
		if td.Name == "delegate" {
			input := []byte(`{"peer":"` + b.peer + `","task":"help"}`)
			return &budgetStream{events: []models.Event{
				{Kind: models.KindTextDelta, TextDelta: "delegating"},
				{Kind: models.KindToolCallDone, ToolCall: &models.ToolCall{ID: "call_1", Name: "delegate", Input: input}},
				{Kind: models.KindUsage, Usage: &u},
				{Kind: models.KindDone, StopReason: models.StopToolUse},
			}}, nil
		}
	}
	// A peer (no delegate tool) — finish.
	return &budgetStream{events: []models.Event{
		{Kind: models.KindTextDelta, TextDelta: "peer done"},
		{Kind: models.KindUsage, Usage: &u},
		{Kind: models.KindDone, StopReason: models.StopEndTurn},
	}}, nil
}

// spendRunner builds a runner whose every input token costs one USD, so a
// budget test states its arithmetic in turns rather than in a rate card.
func spendRunner(t *testing.T, brain models.Model, budgetUSD float64) *Runner {
	t.Helper()
	r, err := NewRunner(Options{
		SessionID:  "run-1",
		Model:      ModelOptions{Kind: ModelFake, Model: "house-model"},
		Brain:      brain,
		BudgetUSD:  budgetUSD,
		CostSource: houseRate{usdPerInputToken: 1},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// chainRegion is a pinned region whose named agents each run one metered turn,
// in order.
func chainRegion(names ...string) Region {
	region := Region{Autonomy: Pinned, Entry: AgentSpec{Name: names[0], Role: "step"}}
	for _, n := range names[1:] {
		region.Peers = append(region.Peers, AgentSpec{Name: n, Role: "step"})
	}
	return region
}

// statuses lists a lineage's node statuses in order, for whole-graph assertions.
func statuses(lin Lineage) []NodeStatus {
	out := make([]NodeStatus, 0, len(lin.Nodes))
	for _, n := range lin.Nodes {
		out = append(out, n.Status)
	}
	return out
}

// TestPinnedRegionSharesOneBudgetAcrossSteps is the defining case for a
// region-wide cap: three agents that each spend under the limit on their own
// exceed it together, and the region must stop. Metering each loop.Run
// separately would let a region of n agents spend n times the cap.
func TestPinnedRegionSharesOneBudgetAcrossSteps(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1}}
	r := spendRunner(t, brain, 2)

	res, err := r.Run(context.Background(), chainRegion("a", "b", "c"), "go")
	if got := brain.turns.Load(); got != 2 {
		t.Fatalf("region ran %d turns of $1 under a $2 cap, want 2: the agents share one region budget rather than each getting the full cap", got)
	}
	if !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("Run error = %v, want billing.ErrBudgetExceeded", err)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusDone, StatusStopped}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v (the third step must not start)", got, want)
	}
	if res.Final != "spending" {
		t.Fatalf("Final = %q, want the partial output of the capped step alongside the error", res.Final)
	}
}

// TestDynamicRegionSharesOneBudgetWithDelegatedPeer asserts the shared cap
// reaches a delegated peer: the entry spends half the budget, the peer spends
// the rest, and the entry does not get another turn.
func TestDynamicRegionSharesOneBudgetWithDelegatedPeer(t *testing.T) {
	brain := &delegatingSpender{usage: models.Usage{InputTokens: 1}, peer: "helper"}
	r := spendRunner(t, brain, 2)

	region := Region{
		Autonomy: Dynamic,
		Entry:    AgentSpec{Name: "lead", Role: "lead"},
		Peers:    []AgentSpec{{Name: "helper", Role: "help", Description: "helps"}},
	}
	res, err := r.Run(context.Background(), region, "go")
	if got := brain.turns.Load(); got != 2 {
		t.Fatalf("region ran %d turns of $1 under a $2 cap, want 2: the entry and its peer share one budget", got)
	}
	if !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("Run error = %v, want billing.ErrBudgetExceeded", err)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusStopped, StatusStopped}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v", got, want)
	}
	if res.Final != "delegating" {
		t.Fatalf("Final = %q, want the entry's partial output alongside the error", res.Final)
	}
}

// TestRunGraphSurfacesBudgetStop asserts the sentinel and the partial result
// survive the graph path, which wraps every region error in its own context.
func TestRunGraphSurfacesBudgetStop(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1}}
	r := spendRunner(t, brain, 2)

	g := Graph{Regions: []GraphRegion{{ID: "one", Region: chainRegion("a", "b", "c")}}}
	res, err := r.RunGraph(context.Background(), g, "go")
	if !errors.Is(err, billing.ErrBudgetExceeded) {
		t.Fatalf("RunGraph error = %v, want billing.ErrBudgetExceeded", err)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusDone, StatusStopped}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v", got, want)
	}
	if res.Final != "spending" {
		t.Fatalf("Final = %q, want the capped region's partial output alongside the error", res.Final)
	}
}

// TestGraphBudgetIsPerRegionNotPerGraph pins the decision: BudgetUSD is a region
// cap, and a graph is a composition of regions, so each region is metered
// against the full budget. Two regions that would together exceed one graph-wide
// cap both complete.
func TestGraphBudgetIsPerRegionNotPerGraph(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1}}
	r := spendRunner(t, brain, 2)

	g := Graph{
		Regions: []GraphRegion{
			{ID: "one", Region: chainRegion("a")},
			{ID: "two", Region: chainRegion("b")},
		},
		Edges: []GraphEdge{{From: "one", To: "two"}},
	}
	res, err := r.RunGraph(context.Background(), g, "go")
	if err != nil {
		t.Fatalf("RunGraph: %v", err)
	}
	if got := brain.turns.Load(); got != 2 {
		t.Fatalf("graph ran %d turns, want 2: each region gets its own $2 budget", got)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusDone, StatusDone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v", got, want)
	}
}

// TestUnmeteredRegionRunsEveryStep is the regression for the unbudgeted path: no
// cap means no meter, and nothing about the run changes.
func TestUnmeteredRegionRunsEveryStep(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1_000_000}}
	r, err := NewRunner(Options{
		SessionID: "run-1",
		Model:     ModelOptions{Kind: ModelFake, Model: "house-model"},
		Brain:     brain,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	res, err := r.Run(context.Background(), chainRegion("a", "b", "c"), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := brain.turns.Load(); got != 3 {
		t.Fatalf("unmetered region ran %d turns, want all 3", got)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusDone, StatusDone, StatusDone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v", got, want)
	}
	if res.Final != "spending" {
		t.Fatalf("Final = %q, want the last step's output", res.Final)
	}
}

// TestRegionUnderBudgetCompletesEveryStep asserts the shared meter does not fire
// early: a region whose aggregate spend stays under the cap completes with a nil
// error and a done node per agent.
func TestRegionUnderBudgetCompletesEveryStep(t *testing.T) {
	brain := &costlyBrain{usage: models.Usage{InputTokens: 1}}
	r := spendRunner(t, brain, 100)

	res, err := r.Run(context.Background(), chainRegion("a", "b", "c"), "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := brain.turns.Load(); got != 3 {
		t.Fatalf("under-budget region ran %d turns, want all 3", got)
	}
	if got, want := statuses(res.Lineage), []NodeStatus{StatusDone, StatusDone, StatusDone}; !reflect.DeepEqual(got, want) {
		t.Fatalf("lineage statuses = %v, want %v", got, want)
	}
}
