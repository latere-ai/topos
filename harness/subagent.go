// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package harness

import (
	"context"
	"fmt"
	"time"

	"latere.ai/x/topos/billing"
	"latere.ai/x/topos/harness/hooks"
)

// Permissions is an agent's delegated authority: the scopes it holds, the tool
// pool it may use, and whether it may itself spawn sub-agents.
type Permissions struct {
	Scopes       []string
	Tools        []string
	AllowRecurse bool
}

// ParentContext is the spawning agent's authority at the spawn boundary. The
// child's authority is derived by intersection against it — trust decreases
// with distance from the primary.
type ParentContext struct {
	SessionID  string
	AgentID    string
	Perms      Permissions
	Budget     billing.Budget
	IsSubAgent bool // true when the parent is itself a sub-agent (recursion gate)
}

// SpawnRequest is what a parent asks for; the actual grant is the intersection
// with the parent's authority.
type SpawnRequest struct {
	// Label deterministically identifies the child under its parent (unique per
	// parent). The child id is reconstructable from (parent session, label)
	// without a registry — so resume reconnects after a crash.
	Label        string
	Scopes       []string
	Tools        []string
	Budget       billing.Budget
	AllowRecurse bool
}

// SubAgent is a spawned, bounded child. Its authority is captured at spawn (not
// recomputed) so it cannot drift above the parent's.
type SubAgent struct {
	ID              string
	ParentSessionID string
	Perms           Permissions
	Budget          billing.Budget
}

// AsParent returns the ParentContext for a sub-agent acting as a spawner — with
// IsSubAgent set, so the default no-recursion rule applies unless the child was
// granted AllowRecurse.
func (s *SubAgent) AsParent() ParentContext {
	return ParentContext{
		SessionID: s.ParentSessionID, AgentID: s.ID,
		Perms: s.Perms, Budget: s.Budget, IsSubAgent: true,
	}
}

// ErrRecursionDenied is returned when a sub-agent attempts to spawn without an
// explicit recursion grant.
var ErrRecursionDenied = fmt.Errorf("harness: sub-agents cannot spawn sub-agents without an explicit grant")

// Spawner allocates bounded sub-agents and records their lifecycle on the bus.
type Spawner struct {
	bus *hooks.Bus
}

// NewSpawner returns a Spawner emitting Subagent events on bus (bus may be nil).
func NewSpawner(bus *hooks.Bus) *Spawner { return &Spawner{bus: bus} }

// Spawn derives a sub-agent from the parent context and request, enforcing
// attenuation at the boundary, sub-allocating the budget, and emitting
// SubagentStart under the parent session_id. A sub-agent parent without an
// AllowRecurse grant is refused.
func (s *Spawner) Spawn(_ context.Context, parent ParentContext, req SpawnRequest) (*SubAgent, error) {
	if parent.IsSubAgent && !parent.Perms.AllowRecurse {
		return nil, ErrRecursionDenied
	}
	if req.Label == "" {
		return nil, fmt.Errorf("harness: spawn requires a label for deterministic identity")
	}

	child := &SubAgent{
		ID:              subAgentID(parent.SessionID, req.Label),
		ParentSessionID: parent.SessionID,
		Perms: Permissions{
			Scopes: intersect(parent.Perms.Scopes, req.Scopes),
			Tools:  intersect(parent.Perms.Tools, req.Tools),
			// Recursion only if the parent both holds and grants it.
			AllowRecurse: req.AllowRecurse && parent.Perms.AllowRecurse,
		},
		Budget: subAllocate(parent.Budget, req.Budget),
	}

	if s.bus != nil {
		s.bus.Dispatch(hooks.EventSubagentStart, &hooks.Payload{
			"version":     "1",
			"session_id":  parent.SessionID,
			"subagent_id": child.ID,
			"parent_id":   parent.AgentID,
			"scopes":      child.Perms.Scopes,
		})
	}
	return child, nil
}

// Stop emits SubagentStop for a child under the parent session.
func (s *Spawner) Stop(_ context.Context, child *SubAgent) {
	if s.bus != nil && child != nil {
		s.bus.Dispatch(hooks.EventSubagentStop, &hooks.Payload{
			"version":     "1",
			"session_id":  child.ParentSessionID,
			"subagent_id": child.ID,
		})
	}
}

// subAgentID is the deterministic, registry-free child identity.
func subAgentID(parentSessionID, label string) string {
	return parentSessionID + "/sub/" + label
}

// intersect returns the elements of want that are also in have (so the result
// is always a subset of have — the parent's pool). Order follows want; deduped.
func intersect(have, want []string) []string {
	haveSet := make(map[string]bool, len(have))
	for _, h := range have {
		haveSet[h] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, w := range want {
		if haveSet[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// subAllocate caps each requested budget axis at the parent's, so a child's
// budget is a sub-allocation of the parent's, never additive. A zero parent
// limit (unlimited) passes the request through.
func subAllocate(parent, req billing.Budget) billing.Budget {
	return billing.Budget{
		LimitUSD:      capFloat(parent.LimitUSD, req.LimitUSD),
		LimitTokens:   capInt(parent.LimitTokens, req.LimitTokens),
		LimitWallTime: capDuration(parent.LimitWallTime, req.LimitWallTime),
	}
}

func capFloat(parent, req float64) float64 {
	if parent > 0 && (req == 0 || req > parent) {
		return parent
	}
	return req
}

func capInt(parent, req int) int {
	if parent > 0 && (req == 0 || req > parent) {
		return parent
	}
	return req
}

func capDuration(parent, req time.Duration) time.Duration {
	if parent > 0 && (req == 0 || req > parent) {
		return parent
	}
	return req
}
