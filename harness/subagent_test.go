// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package harness_test

import (
	"context"
	"testing"
	"time"

	"github.com/latere-ai/topos/billing"
	"github.com/latere-ai/topos/harness"
	"github.com/latere-ai/topos/harness/hooks"
)

func parentCtx() harness.ParentContext {
	return harness.ParentContext{
		SessionID: "sess_1",
		AgentID:   "agent_primary",
		Perms: harness.Permissions{
			Scopes:       []string{"read:agents", "run:agents", "write:agents"},
			Tools:        []string{"bash", "read", "write"},
			AllowRecurse: false,
		},
		Budget: billing.Budget{LimitUSD: 10, LimitTokens: 100000, LimitWallTime: time.Hour},
	}
}

func TestSpawnAttenuatesToSubset(t *testing.T) {
	bus := hooks.New()
	sp := harness.NewSpawner(bus)
	parent := parentCtx()

	// Request includes a scope/tool the parent lacks — must be dropped.
	child, err := sp.Spawn(context.Background(), parent, harness.SpawnRequest{
		Label:  "worker1",
		Scopes: []string{"run:agents", "admin:agents"}, // admin not held by parent
		Tools:  []string{"bash", "exec"},               // exec not held by parent
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !isSubset(child.Perms.Scopes, parent.Perms.Scopes) {
		t.Fatalf("child scopes %v not a subset of parent %v", child.Perms.Scopes, parent.Perms.Scopes)
	}
	for _, s := range child.Perms.Scopes {
		if s == "admin:agents" {
			t.Fatal("child gained a scope the parent lacks")
		}
	}
	if len(child.Perms.Tools) != 1 || child.Perms.Tools[0] != "bash" {
		t.Fatalf("child tools = %v, want [bash] (exec dropped)", child.Perms.Tools)
	}

	// SubagentStart recorded under the parent session.
	var started bool
	for _, e := range bus.EventLog() {
		if e.EventName == hooks.EventSubagentStart {
			started = true
		}
	}
	if !started {
		t.Fatal("SubagentStart not emitted")
	}
}

func TestSpawnSubAllocatesBudget(t *testing.T) {
	sp := harness.NewSpawner(nil)
	parent := parentCtx()

	// Request more than the parent has → capped at the parent's.
	child, _ := sp.Spawn(context.Background(), parent, harness.SpawnRequest{
		Label:  "w",
		Budget: billing.Budget{LimitUSD: 1000, LimitTokens: 5000},
	})
	if child.Budget.LimitUSD != 10 {
		t.Fatalf("child usd budget = %v, want capped at parent's 10", child.Budget.LimitUSD)
	}
	if child.Budget.LimitTokens != 5000 {
		t.Fatalf("child tokens = %d, want 5000 (under parent cap, honoured)", child.Budget.LimitTokens)
	}
}

func TestSpawnDeterministicIdentity(t *testing.T) {
	sp := harness.NewSpawner(nil)
	parent := parentCtx()
	a, _ := sp.Spawn(context.Background(), parent, harness.SpawnRequest{Label: "alpha"})
	// Re-spawn with the same parent session + label → same id (registry-free,
	// reconnects after a crash).
	b, _ := sp.Spawn(context.Background(), parent, harness.SpawnRequest{Label: "alpha"})
	if a.ID != b.ID {
		t.Fatalf("ids differ: %q vs %q — not deterministic", a.ID, b.ID)
	}
	if a.ID == "" {
		t.Fatal("empty sub-agent id")
	}
}

func TestSpawnRequiresLabel(t *testing.T) {
	sp := harness.NewSpawner(nil)
	if _, err := sp.Spawn(context.Background(), parentCtx(), harness.SpawnRequest{}); err == nil {
		t.Fatal("spawn without label should error (no deterministic identity)")
	}
}

func TestSubAgentCannotRecurseByDefault(t *testing.T) {
	sp := harness.NewSpawner(nil)
	parent := parentCtx() // AllowRecurse: false

	child, _ := sp.Spawn(context.Background(), parent, harness.SpawnRequest{Label: "w", AllowRecurse: true})
	if child.Perms.AllowRecurse {
		t.Fatal("child gained recursion despite parent not granting it")
	}
	// The child trying to spawn is refused.
	if _, err := sp.Spawn(context.Background(), child.AsParent(), harness.SpawnRequest{Label: "gw"}); err != harness.ErrRecursionDenied {
		t.Fatalf("sub-agent spawn = %v, want ErrRecursionDenied", err)
	}
}

func TestRecursionAllowedWithGrant(t *testing.T) {
	sp := harness.NewSpawner(nil)
	parent := parentCtx()
	parent.Perms.AllowRecurse = true // primary granted recursion

	child, _ := sp.Spawn(context.Background(), parent, harness.SpawnRequest{Label: "w", AllowRecurse: true})
	if !child.Perms.AllowRecurse {
		t.Fatal("child should carry the granted recursion")
	}
	grandchild, err := sp.Spawn(context.Background(), child.AsParent(), harness.SpawnRequest{Label: "gw"})
	if err != nil {
		t.Fatalf("granted recursion spawn: %v", err)
	}
	if grandchild.ParentSessionID != parent.SessionID {
		t.Fatalf("grandchild parent session = %q", grandchild.ParentSessionID)
	}
}

func TestSpawnStopEmitsEvent(t *testing.T) {
	bus := hooks.New()
	sp := harness.NewSpawner(bus)
	child, _ := sp.Spawn(context.Background(), parentCtx(), harness.SpawnRequest{Label: "w"})
	sp.Stop(context.Background(), child)
	var stopped bool
	for _, e := range bus.EventLog() {
		if e.EventName == hooks.EventSubagentStop {
			stopped = true
		}
	}
	if !stopped {
		t.Fatal("SubagentStop not emitted")
	}
}

// ---- mailbox ----

func TestHierarchicalTopology(t *testing.T) {
	topo := harness.NewHierarchical("parent", "childA", "childB")
	cases := []struct {
		from, to string
		want     bool
	}{
		{"parent", "childA", true},  // parent → child
		{"childA", "parent", true},  // child → parent
		{"childA", "childB", false}, // sibling → sibling denied
		{"outsider", "parent", false},
		{"parent", "outsider", false},
	}
	for _, c := range cases {
		if got := topo.CanSend(c.from, c.to); got != c.want {
			t.Errorf("CanSend(%q,%q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

func TestMailboxGatedSendReceive(t *testing.T) {
	mb := harness.NewMailbox(harness.NewHierarchical("parent", "child"))

	if err := mb.Send("parent", "child", []byte("do task")); err != nil {
		t.Fatalf("parent→child send: %v", err)
	}
	if err := mb.Send("child", "parent", []byte("done")); err != nil {
		t.Fatalf("child→parent send: %v", err)
	}
	// Sibling send is refused.
	if err := mb.Send("child", "other", []byte("x")); err != harness.ErrNotPermitted {
		t.Fatalf("disallowed send = %v, want ErrNotPermitted", err)
	}

	got := mb.Receive("child")
	if len(got) != 1 || string(got[0].Body) != "do task" {
		t.Fatalf("child inbox = %+v", got)
	}
	// Receive drains.
	if len(mb.Receive("child")) != 0 {
		t.Fatal("Receive did not drain the box")
	}
}

func isSubset(sub, super []string) bool {
	set := map[string]bool{}
	for _, s := range super {
		set[s] = true
	}
	for _, s := range sub {
		if !set[s] {
			return false
		}
	}
	return true
}
