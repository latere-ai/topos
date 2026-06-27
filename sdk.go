// Package sdk is the public, embeddable surface of the Topos agent runtime.
//
// A foreign Go module (the first consumer is latere.ai/x/wallfacer, running
// agents locally and in-process) imports this package to define agents, compose
// them into a region, and run the region — without importing anything under
// internal/. Everything exported here uses only sdk-defined or standard-library
// types, so the boundary holds across the module edge (Go forbids cross-module
// internal/ imports; this package is the curated public API on the inside of
// that edge).
//
// This is the M0 spike surface (see specs/architecture/agent-sdk-mesh-foundation.md):
// it proves directory-based peer discovery, the delegate primitive over the real
// attenuating Spawner, and a reproducible lineage graph. The production brain
// (adapting internal/models + the agentic loop) and the live event stream are
// later milestones; the brain is a seam (ModelProvider) so a fake provider makes
// runs deterministic.
package sdk

import (
	"context"
	"fmt"

	"latere.ai/x/agents/internal/billing"
	"latere.ai/x/agents/internal/harness"
	"latere.ai/x/agents/internal/harness/hooks"
)

// Autonomy declares how a region decides its handoffs.
type Autonomy string

const (
	// Pinned is a deterministic chain: the entry agent then each peer in order.
	// This is what a wallfacer flow (implement = impl → test → commit) compiles to.
	Pinned Autonomy = "pinned"
	// Dynamic hands the entry agent a directory of peers and lets it decide whom
	// to delegate to. Discovery is workspace-wide; whom it may message stays
	// capability-gated (orchestrator+worker by default, full mesh opt-in).
	Dynamic Autonomy = "dynamic"
)

// AgentSpec is a declarative agent in a region.
type AgentSpec struct {
	Name        string   // identity within the region (and the spawn label)
	Role        string   // short role label, e.g. "reviewer"
	Description string   // when-to-use; published into the directory for discovery
	Tools       []string // tool families this agent may use
	Scopes      []string // permission scopes this agent holds
}

// PeerCard is what a dynamic agent sees in the directory: enough to decide whether
// to delegate, never the peer's permissions.
type PeerCard struct {
	Name        string
	Role        string
	Description string
}

// Region is one part of a run with a single autonomy mode. One graph mixes pinned
// and dynamic regions; M0 runs a single region.
type Region struct {
	Autonomy Autonomy
	Entry    AgentSpec   // the agent that starts the region
	Peers    []AgentSpec // discoverable peers (dynamic) or the ordered chain (pinned)
}

// Turn is the decision context handed to the brain. For a dynamic region the
// directory (Peers) is injected so the agent can choose a handoff.
type Turn struct {
	Agent string
	Role  string
	Task  string
	Peers []PeerCard
}

// ActionKind discriminates a brain decision.
type ActionKind string

const (
	ActionDelegate ActionKind = "delegate"
	ActionFinal    ActionKind = "final"
)

// Action is the brain's decision for a turn.
type Action struct {
	Kind      ActionKind
	Delegate  *DelegateAction // set when Kind == ActionDelegate
	FinalText string          // set when Kind == ActionFinal
}

// DelegateAction names a peer from the directory and the subtask handed to it.
type DelegateAction struct {
	Peer string
	Task string
}

// ModelProvider is the brain seam. A fake/scripted provider makes a run fully
// reproducible (used in tests and the embed check); the production provider
// adapts internal/models + the agentic loop in a later milestone.
type ModelProvider interface {
	Decide(ctx context.Context, turn Turn) (Action, error)
}

// LineageNode is one agent in the run graph.
type LineageNode struct {
	ID     string
	Name   string
	Role   string
	Status string   // "running" | "done" | "failed"
	Grants []string // tool families actually granted after attenuation (audit-visible)
}

// LineageEdge records a relationship between two nodes.
type LineageEdge struct {
	From string
	To   string
	Kind string // "delegate" | "deliver" | "next"
}

// Lineage is the renderable run graph (who delegated/handed off to whom). Node and
// edge order are deterministic given the same inputs, so a consumer (wallfacer's
// GraphCanvas) can diff runs and resume reconnects to stable ids.
type Lineage struct {
	Nodes []LineageNode
	Edges []LineageEdge
}

// RunResult is the outcome of running a region.
type RunResult struct {
	Lineage Lineage
	Final   string
}

// Options configure a Runner.
type Options struct {
	SessionID string        // stable run id; deterministic child ids derive from it
	Provider  ModelProvider // the brain seam (required)
	BudgetUSD float64       // region spend cap, sub-allocated to delegates
}

// Runner executes regions against the real attenuating Spawner and hook bus.
type Runner struct {
	opts    Options
	bus     *hooks.Bus
	spawner *harness.Spawner
}

// NewRunner builds a Runner with its own hook bus and spawner.
func NewRunner(opts Options) *Runner {
	bus := hooks.New()
	return &Runner{opts: opts, bus: bus, spawner: harness.NewSpawner(bus)}
}

// Run executes a region and returns its lineage graph.
func (r *Runner) Run(ctx context.Context, region Region) (RunResult, error) {
	if r.opts.Provider == nil {
		return RunResult{}, fmt.Errorf("sdk: nil ModelProvider")
	}
	switch region.Autonomy {
	case Dynamic:
		return r.runDynamic(ctx, region)
	case Pinned:
		return r.runPinned(ctx, region)
	default:
		return RunResult{}, fmt.Errorf("sdk: unknown autonomy %q", region.Autonomy)
	}
}

// session returns the run id, defaulting when unset so ids stay well-formed.
func (r *Runner) session() string {
	if r.opts.SessionID == "" {
		return "session"
	}
	return r.opts.SessionID
}

// runDynamic runs the entry agent with the directory injected; if it delegates,
// the chosen peer runs as a real attenuated spawn and the handoff is recorded.
func (r *Runner) runDynamic(ctx context.Context, region Region) (RunResult, error) {
	sess := r.session()
	entryID := sess + "/" + region.Entry.Name
	lin := Lineage{Nodes: []LineageNode{{
		ID: entryID, Name: region.Entry.Name, Role: region.Entry.Role,
		Status: "running", Grants: region.Entry.Tools,
	}}}

	dir := make([]PeerCard, 0, len(region.Peers))
	for _, p := range region.Peers {
		dir = append(dir, PeerCard{Name: p.Name, Role: p.Role, Description: p.Description})
	}

	act, err := r.opts.Provider.Decide(ctx, Turn{
		Agent: region.Entry.Name, Role: region.Entry.Role, Peers: dir,
	})
	if err != nil {
		setStatus(&lin, entryID, "failed")
		return RunResult{Lineage: lin}, err
	}

	switch act.Kind {
	case ActionFinal:
		setStatus(&lin, entryID, "done")
		return RunResult{Lineage: lin, Final: act.FinalText}, nil

	case ActionDelegate:
		if act.Delegate == nil {
			setStatus(&lin, entryID, "failed")
			return RunResult{Lineage: lin}, fmt.Errorf("sdk: delegate action with nil target")
		}
		peer, ok := findAgent(region.Peers, act.Delegate.Peer)
		if !ok {
			setStatus(&lin, entryID, "failed")
			return RunResult{Lineage: lin}, fmt.Errorf("sdk: delegate to unknown peer %q", act.Delegate.Peer)
		}

		// Real attenuated spawn: child scopes/tools are the intersection with the
		// entry's, budget is sub-allocated, identity is deterministic. The hook bus
		// emits SubagentStart/Stop under the session.
		parent := harness.ParentContext{
			SessionID: sess,
			AgentID:   region.Entry.Name,
			Perms:     harness.Permissions{Scopes: region.Entry.Scopes, Tools: region.Entry.Tools},
			Budget:    billing.Budget{LimitUSD: r.opts.BudgetUSD},
		}
		child, err := r.spawner.Spawn(ctx, parent, harness.SpawnRequest{
			Label:  peer.Name,
			Scopes: peer.Scopes,
			Tools:  peer.Tools,
			Budget: billing.Budget{LimitUSD: r.opts.BudgetUSD},
		})
		if err != nil {
			setStatus(&lin, entryID, "failed")
			return RunResult{Lineage: lin}, fmt.Errorf("sdk: spawn peer %q: %w", peer.Name, err)
		}
		lin.Nodes = append(lin.Nodes, LineageNode{
			ID: child.ID, Name: peer.Name, Role: peer.Role,
			Status: "running", Grants: child.Perms.Tools,
		})
		lin.Edges = append(lin.Edges, LineageEdge{From: entryID, To: child.ID, Kind: "delegate"})

		pact, err := r.opts.Provider.Decide(ctx, Turn{Agent: peer.Name, Role: peer.Role, Task: act.Delegate.Task})
		if err != nil {
			setStatus(&lin, child.ID, "failed")
			r.spawner.Stop(ctx, child)
			return RunResult{Lineage: lin}, err
		}
		setStatus(&lin, child.ID, "done")
		r.spawner.Stop(ctx, child)
		lin.Edges = append(lin.Edges, LineageEdge{From: child.ID, To: entryID, Kind: "deliver"})
		setStatus(&lin, entryID, "done")
		return RunResult{Lineage: lin, Final: pact.FinalText}, nil

	default:
		setStatus(&lin, entryID, "failed")
		return RunResult{Lineage: lin}, fmt.Errorf("sdk: unknown action kind %q", act.Kind)
	}
}

// runPinned runs a deterministic chain: entry then each peer in order, no
// directory. This is the shape a wallfacer flow compiles to.
func (r *Runner) runPinned(ctx context.Context, region Region) (RunResult, error) {
	sess := r.session()
	chain := append([]AgentSpec{region.Entry}, region.Peers...)
	var lin Lineage
	var final string
	prevID := ""
	for _, step := range chain {
		id := sess + "/" + step.Name
		lin.Nodes = append(lin.Nodes, LineageNode{
			ID: id, Name: step.Name, Role: step.Role, Status: "running", Grants: step.Tools,
		})
		if prevID != "" {
			lin.Edges = append(lin.Edges, LineageEdge{From: prevID, To: id, Kind: "next"})
		}
		act, err := r.opts.Provider.Decide(ctx, Turn{Agent: step.Name, Role: step.Role})
		if err != nil {
			setStatus(&lin, id, "failed")
			return RunResult{Lineage: lin}, err
		}
		final = act.FinalText
		setStatus(&lin, id, "done")
		prevID = id
	}
	return RunResult{Lineage: lin, Final: final}, nil
}

func findAgent(in []AgentSpec, name string) (AgentSpec, bool) {
	for _, a := range in {
		if a.Name == name {
			return a, true
		}
	}
	return AgentSpec{}, false
}

func setStatus(lin *Lineage, id, status string) {
	for i := range lin.Nodes {
		if lin.Nodes[i].ID == id {
			lin.Nodes[i].Status = status
			return
		}
	}
}
