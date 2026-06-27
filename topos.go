// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package topos is the public, embeddable surface of the Topos agent runtime.
//
// A host application imports this package to define agents, compose them into a
// region, and run the region locally and in-process — without importing anything
// under internal/. Everything exported here uses only topos-defined or standard-library
// types, so the boundary holds across the module edge (Go forbids cross-module
// internal/ imports; this package is the curated public API on the inside of
// that edge).
//
// The runner executes agents through the real agentic loop (internal/runtime/loop):
// the model is the brain (configured via ModelOptions — Lux, a direct provider, or
// the deterministic fake), and a handoff is an agents-as-tools delegation — a
// `delegate` tool registered into the loop whose Invoke performs a real attenuated
// Spawner spawn, runs the chosen peer as a nested loop, and returns its result into
// the parent transcript. See specs/architecture/agent-sdk-mesh-foundation.md.
package topos

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/topos/billing"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/hooks"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runtime/loop"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/local"
)

// Autonomy declares how a region decides its handoffs.
type Autonomy string

const (
	// Pinned is a deterministic chain: the entry agent then each peer in order.
	// This is what a static flow (implement = impl → test → commit) compiles to.
	Pinned Autonomy = "pinned"
	// Dynamic gives the entry agent a `delegate` tool over a directory of peers and
	// lets the model decide whom to hand off to. Discovery is workspace-wide; whom it
	// may message stays capability-gated (orchestrator+worker by default, mesh opt-in).
	Dynamic Autonomy = "dynamic"
)

// AgentSpec is a declarative agent in a region.
type AgentSpec struct {
	Name         string   // identity within the region (and the spawn label)
	Role         string   // short role label, e.g. "reviewer"
	Description  string   // when-to-use; published into the directory for discovery
	SystemPrompt string   // the agent's system prompt
	Tools        []string // tool families this agent may use
	Scopes       []string // permission scopes this agent holds
}

// PeerCard is what a dynamic agent sees in the directory: enough to decide whether
// to delegate, never the peer's permissions.
type PeerCard struct {
	Name        string
	Role        string
	Description string
}

// Topology decides whom a dynamic agent may delegate to.
type Topology string

const (
	// OrchestratorWorker (default) — only the entry agent delegates to peers; a
	// delegated peer runs without a delegate tool. The safe default the
	// architecture memo argues for.
	OrchestratorWorker Topology = "orchestrator-worker"
	// Mesh — any agent may delegate to a peer (recursive), bounded by
	// Options.MaxHandoffDepth. Opt-in.
	Mesh Topology = "mesh"
)

// Region is one part of a run with a single autonomy mode. One graph mixes pinned
// and dynamic regions; M1 runs a single region.
type Region struct {
	Autonomy Autonomy
	Topology Topology    // dynamic only; default OrchestratorWorker
	Entry    AgentSpec   // the agent that starts the region
	Peers    []AgentSpec // discoverable peers (dynamic) or the ordered chain (pinned)
}

// NodeStatus is the lifecycle state of a lineage node.
type NodeStatus string

// Lifecycle states a lineage node moves through during a run.
const (
	StatusRunning NodeStatus = "running"
	StatusDone    NodeStatus = "done"
	StatusFailed  NodeStatus = "failed"
)

// LineageNode is one agent in the run graph.
type LineageNode struct {
	ID      string
	Name    string
	Role    string
	Status  NodeStatus
	Grants  []string // tool families actually granted after attenuation (audit-visible)
	Sandbox string   // the sandbox this agent ran in (a delegated peer gets its own)
}

// EdgeKind is the relationship a lineage edge represents.
type EdgeKind string

// Relationships a lineage edge can represent between two nodes.
const (
	EdgeDelegate EdgeKind = "delegate"
	EdgeDeliver  EdgeKind = "deliver"
	EdgeNext     EdgeKind = "next"
)

// LineageEdge records a relationship between two nodes.
type LineageEdge struct {
	From string
	To   string
	Kind EdgeKind
}

// Lineage is the renderable run graph (who delegated/handed off to whom). Ids are
// deterministic (<session>/<name>, child <session>/sub/<label>), so a consumer
// (e.g. a live graph view) can diff runs and resume reconnects to stable ids.
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
	SessionID       string       // stable run id; deterministic child ids derive from it
	Model           ModelOptions // the brain connection (Lux / direct / fake)
	BudgetUSD       float64      // region spend cap, sub-allocated to delegates
	MaxHandoffDepth int          // max delegation depth in a Mesh region (default 3); bounds recursion
}

// Runner executes regions in-process through the real agentic loop, against the
// real attenuating Spawner and hook bus.
type Runner struct {
	opts    Options
	model   models.Model
	bus     *hooks.Bus
	spawner *harness.Spawner
}

// NewRunner builds a Runner, constructing the model from Options.Model.
func NewRunner(opts Options) (*Runner, error) {
	m, err := buildModel(opts.Model)
	if err != nil {
		return nil, err
	}
	bus := hooks.New()
	return &Runner{opts: opts, model: m, bus: bus, spawner: harness.NewSpawner(bus)}, nil
}

// Run executes a region in-process (a local sandbox is created for the run) and
// returns its lineage graph. task is the user request handed to the entry agent.
func (r *Runner) Run(ctx context.Context, region Region, task string) (RunResult, error) {
	p := local.New()
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		return RunResult{}, fmt.Errorf("topos: create sandbox: %w", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	switch region.Autonomy {
	case Dynamic:
		return r.runDynamic(ctx, p, sb.ID, region, task)
	case Pinned:
		return r.runPinned(ctx, p, sb.ID, region, task)
	default:
		return RunResult{}, fmt.Errorf("topos: unknown autonomy %q", region.Autonomy)
	}
}

func (r *Runner) session() string {
	if r.opts.SessionID == "" {
		return "session"
	}
	return r.opts.SessionID
}

// dynRun bundles the inputs for running one dynamic agent (the entry or a delegated
// peer) so the recursive runAgent / delegate path stays readable.
type dynRun struct {
	sb          sandbox.Provider
	sandboxID   string
	agent       AgentSpec
	parent      harness.ParentContext // context used to spawn THIS agent's children
	dir         []AgentSpec
	topology    Topology
	depth       int
	task        string
	lin         *Lineage
	nodeID      string // this agent's lineage node id
	loopSession string // loop.Config.SessionID for this agent
	path        string // delegation label path ("" for the entry), keeps child ids unique
}

// runDynamic runs the entry agent, then recurses through delegations.
func (r *Runner) runDynamic(ctx context.Context, sb sandbox.Provider, sandboxID string, region Region, task string) (RunResult, error) {
	sess := r.session()
	entryID := sess + "/" + region.Entry.Name
	lin := &Lineage{Nodes: []LineageNode{{
		ID: entryID, Name: region.Entry.Name, Role: region.Entry.Role,
		Status: StatusRunning, Grants: region.Entry.Tools, Sandbox: sandboxID,
	}}}
	parent := harness.ParentContext{
		SessionID: sess,
		AgentID:   region.Entry.Name,
		Perms:     harness.Permissions{Scopes: region.Entry.Scopes, Tools: region.Entry.Tools, AllowRecurse: region.Topology == Mesh},
		Budget:    billing.Budget{LimitUSD: r.opts.BudgetUSD},
	}
	final, err := r.runAgent(ctx, dynRun{
		sb: sb, sandboxID: sandboxID, agent: region.Entry, parent: parent,
		dir: region.Peers, topology: region.Topology, depth: 0,
		task: task, lin: lin, nodeID: entryID, loopSession: sess, path: "",
	})
	if err != nil {
		setStatus(lin, entryID, StatusFailed)
		return RunResult{Lineage: *lin}, err
	}
	setStatus(lin, entryID, StatusDone)
	return RunResult{Lineage: *lin, Final: final}, nil
}

// runAgent runs one dynamic agent through the loop. It offers the `delegate` tool
// only when the agent may delegate: the entry always may (orchestrator+worker); a
// peer may only under Mesh topology; and never at or past MaxHandoffDepth. That
// depth gate is what bounds recursion and prevents runaway fan-out.
func (r *Runner) runAgent(ctx context.Context, rc dynRun) (string, error) {
	reg := tools.Builtins()
	sysPrompt := rc.agent.SystemPrompt
	canDelegate := len(rc.dir) > 0 && rc.depth < r.maxDepth() && (rc.depth == 0 || rc.topology == Mesh)
	if canDelegate {
		reg.Register(&delegateTool{
			runner: r, dir: rc.dir, parent: rc.parent, topology: rc.topology,
			depth: rc.depth, entryID: rc.nodeID, lineage: rc.lin, path: rc.path,
		})
		sysPrompt = composeSystem(sysPrompt, renderDirectory(toCards(rc.dir)))
	}
	res, err := loop.Run(ctx, loop.Config{
		Model:        r.model,
		Sandbox:      rc.sb,
		SandboxID:    rc.sandboxID,
		Tools:        reg,
		Bus:          r.bus,
		SessionID:    rc.loopSession,
		AgentID:      rc.agent.Name,
		SystemPrompt: sysPrompt,
		UserPrompt:   rc.task,
	})
	if err != nil {
		return "", err
	}
	return res.FinalText, nil
}

// runPinned runs a deterministic chain: entry then each peer in order, each as its
// own loop. This is the shape a static flow compiles to.
func (r *Runner) runPinned(ctx context.Context, sb sandbox.Provider, sandboxID string, region Region, task string) (RunResult, error) {
	sess := r.session()
	chain := append([]AgentSpec{region.Entry}, region.Peers...)
	lin := &Lineage{}
	var final string
	prevID := ""
	for _, step := range chain {
		id := sess + "/" + step.Name
		lin.Nodes = append(lin.Nodes, LineageNode{
			ID: id, Name: step.Name, Role: step.Role, Status: StatusRunning, Grants: step.Tools, Sandbox: sandboxID,
		})
		if prevID != "" {
			lin.Edges = append(lin.Edges, LineageEdge{From: prevID, To: id, Kind: EdgeNext})
		}
		res, err := loop.Run(ctx, loop.Config{
			Model:        r.model,
			Sandbox:      sb,
			SandboxID:    sandboxID,
			Tools:        tools.Builtins(),
			Bus:          r.bus,
			SessionID:    id,
			AgentID:      step.Name,
			SystemPrompt: step.SystemPrompt,
			UserPrompt:   task,
		})
		if err != nil {
			setStatus(lin, id, StatusFailed)
			return RunResult{Lineage: *lin}, err
		}
		final = res.FinalText
		setStatus(lin, id, "done")
		prevID = id
	}
	return RunResult{Lineage: *lin, Final: final}, nil
}

// delegateTool is the agents-as-tools handoff primitive: it looks up a peer in the
// directory, spawns it with attenuated authority (granting recursion only under
// Mesh with depth budget left), runs the peer via runAgent in its own sandbox,
// records the lineage, and returns the peer's output as the tool result.
type delegateTool struct {
	runner   *Runner
	dir      []AgentSpec
	parent   harness.ParentContext
	topology Topology
	depth    int
	entryID  string
	lineage  *Lineage
	path     string // the delegating agent's label path; child labels extend it
}

func (d *delegateTool) Name() string { return "delegate" }

func (d *delegateTool) Def() models.ToolDef {
	names := make([]string, 0, len(d.dir))
	for _, p := range d.dir {
		names = append(names, p.Name)
	}
	desc := "Delegate a subtask to a peer agent. Available peers: " + strings.Join(names, ", ")
	return models.ToolDef{
		Name:        "delegate",
		Description: desc,
		InputSchema: json.RawMessage(`{"type":"object","properties":{"peer":{"type":"string"},"task":{"type":"string"}},"required":["peer","task"]}`),
	}
}

func (d *delegateTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.Provider, sandboxID string) (models.ToolResult, error) {
	var args struct {
		Peer string `json:"peer"`
		Task string `json:"task"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return models.ToolResult{IsError: true, Content: "bad delegate input"}, nil
	}
	peer, ok := findAgent(d.dir, args.Peer)
	if !ok {
		return models.ToolResult{IsError: true, Content: "unknown peer: " + args.Peer}, nil
	}

	// A path-prefixed label keeps child ids unique across the run: the harness's
	// deterministic id is <rootSession>/sub/<label> (flat, via AsParent), so a peer
	// reused at different depths would collide without the path.
	childLabel := peer.Name
	if d.path != "" {
		childLabel = d.path + "." + peer.Name
	}

	// Grant the child recursion only under Mesh with depth budget left to delegate
	// again; the Spawner enforces the grant, and runAgent withholds the delegate
	// tool past the bound.
	allowRecurse := d.topology == Mesh && d.depth+1 < d.runner.maxDepth()
	child, err := d.runner.spawner.Spawn(ctx, d.parent, harness.SpawnRequest{
		Label: childLabel, Scopes: peer.Scopes, Tools: peer.Tools,
		Budget: d.parent.Budget, AllowRecurse: allowRecurse,
	})
	if err != nil {
		return models.ToolResult{IsError: true, Content: "spawn failed: " + err.Error()}, nil
	}

	// Per-child sandbox (M3): the peer runs in its own sandbox. A provisioning
	// failure is the child's, not the parent's.
	box, err := sb.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		d.appendChild(child, peer, StatusFailed, "")
		d.runner.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "sandbox create failed: " + err.Error()}, nil
	}
	defer sb.Destroy(ctx, box.ID) //nolint:errcheck
	d.appendChild(child, peer, StatusRunning, box.ID)

	// Run the peer recursively: under Mesh it may itself delegate, until the bound.
	peerFinal, err := d.runner.runAgent(ctx, dynRun{
		sb: sb, sandboxID: box.ID, agent: peer, parent: child.AsParent(),
		dir: d.dir, topology: d.topology, depth: d.depth + 1,
		task: args.Task, lin: d.lineage, nodeID: child.ID, loopSession: child.ID, path: childLabel,
	})
	if err != nil {
		setStatus(d.lineage, child.ID, StatusFailed)
		d.runner.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "peer run failed: " + err.Error()}, nil
	}
	setStatus(d.lineage, child.ID, StatusDone)
	d.runner.spawner.Stop(ctx, child)
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: child.ID, To: d.entryID, Kind: EdgeDeliver})
	return models.ToolResult{Content: peerFinal}, nil
}

// appendChild records a delegated peer as a lineage node (with the sandbox it ran
// in) plus the delegate edge from the entry.
func (d *delegateTool) appendChild(child *harness.SubAgent, peer AgentSpec, status NodeStatus, box string) {
	d.lineage.Nodes = append(d.lineage.Nodes, LineageNode{
		ID: child.ID, Name: peer.Name, Role: peer.Role, Status: status,
		Grants: child.Perms.Tools, Sandbox: box,
	})
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: d.entryID, To: child.ID, Kind: EdgeDelegate})
}

func findAgent(in []AgentSpec, name string) (AgentSpec, bool) {
	for _, a := range in {
		if a.Name == name {
			return a, true
		}
	}
	return AgentSpec{}, false
}

func setStatus(lin *Lineage, id string, status NodeStatus) {
	for i := range lin.Nodes {
		if lin.Nodes[i].ID == id {
			lin.Nodes[i].Status = status
			return
		}
	}
}

// maxDepth is the recursion bound for Mesh delegation (default 3).
func (r *Runner) maxDepth() int {
	if r.opts.MaxHandoffDepth > 0 {
		return r.opts.MaxHandoffDepth
	}
	return 3
}

func toCards(in []AgentSpec) []PeerCard {
	cards := make([]PeerCard, 0, len(in))
	for _, a := range in {
		cards = append(cards, PeerCard{Name: a.Name, Role: a.Role, Description: a.Description})
	}
	return cards
}
