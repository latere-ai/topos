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
// The runner executes agents through the real agentic loop (internal/runtime/loop):
// the model is the brain (configured via ModelOptions — Lux, a direct provider, or
// the deterministic fake), and a handoff is an agents-as-tools delegation — a
// `delegate` tool registered into the loop whose Invoke performs a real attenuated
// Spawner spawn, runs the chosen peer as a nested loop, and returns its result into
// the parent transcript. See specs/architecture/agent-sdk-mesh-foundation.md.
package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/agents/internal/billing"
	"latere.ai/x/agents/internal/harness"
	"latere.ai/x/agents/internal/harness/hooks"
	"latere.ai/x/agents/internal/harness/tools"
	"latere.ai/x/agents/internal/models"
	"latere.ai/x/agents/internal/runtime/loop"
	"latere.ai/x/agents/internal/sandbox"
	"latere.ai/x/agents/internal/sandbox/local"
)

// Autonomy declares how a region decides its handoffs.
type Autonomy string

const (
	// Pinned is a deterministic chain: the entry agent then each peer in order.
	// This is what a wallfacer flow (implement = impl → test → commit) compiles to.
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

// Region is one part of a run with a single autonomy mode. One graph mixes pinned
// and dynamic regions; M1 runs a single region.
type Region struct {
	Autonomy Autonomy
	Entry    AgentSpec   // the agent that starts the region
	Peers    []AgentSpec // discoverable peers (dynamic) or the ordered chain (pinned)
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

// Lineage is the renderable run graph (who delegated/handed off to whom). Ids are
// deterministic (<session>/<name>, child <session>/sub/<label>), so a consumer
// (wallfacer's GraphCanvas) can diff runs and resume reconnects to stable ids.
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
	SessionID string       // stable run id; deterministic child ids derive from it
	Model     ModelOptions // the brain connection (Lux / direct / fake)
	BudgetUSD float64      // region spend cap, sub-allocated to delegates
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
		return RunResult{}, fmt.Errorf("sdk: create sandbox: %w", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck

	switch region.Autonomy {
	case Dynamic:
		return r.runDynamic(ctx, p, sb.ID, region, task)
	case Pinned:
		return r.runPinned(ctx, p, sb.ID, region, task)
	default:
		return RunResult{}, fmt.Errorf("sdk: unknown autonomy %q", region.Autonomy)
	}
}

func (r *Runner) session() string {
	if r.opts.SessionID == "" {
		return "session"
	}
	return r.opts.SessionID
}

// runDynamic runs the entry agent through the loop with a `delegate` tool over the
// region's peer directory. The model drives delegation; the tool does the spawn.
func (r *Runner) runDynamic(ctx context.Context, sb sandbox.SandboxProvider, sandboxID string, region Region, task string) (RunResult, error) {
	sess := r.session()
	entryID := sess + "/" + region.Entry.Name
	lin := &Lineage{Nodes: []LineageNode{{
		ID: entryID, Name: region.Entry.Name, Role: region.Entry.Role,
		Status: "running", Grants: region.Entry.Tools,
	}}}

	parent := harness.ParentContext{
		SessionID: sess,
		AgentID:   region.Entry.Name,
		Perms:     harness.Permissions{Scopes: region.Entry.Scopes, Tools: region.Entry.Tools},
		Budget:    billing.Budget{LimitUSD: r.opts.BudgetUSD},
	}

	reg := tools.Builtins()
	reg.Register(&delegateTool{
		model: r.model, dir: region.Peers, spawner: r.spawner,
		parent: parent, entryID: entryID, lineage: lin,
	})

	// Inject the peer directory into the entry agent's system prompt so the model
	// discovers peers from their descriptions (mesh discovery, M2).
	sysPrompt := composeSystem(region.Entry.SystemPrompt, renderDirectory(region.Directory()))

	res, err := loop.Run(ctx, loop.Config{
		Model:        r.model,
		Sandbox:      sb,
		SandboxID:    sandboxID,
		Tools:        reg,
		Bus:          r.bus,
		SessionID:    sess,
		AgentID:      region.Entry.Name,
		SystemPrompt: sysPrompt,
		UserPrompt:   task,
	})
	if err != nil {
		setStatus(lin, entryID, "failed")
		return RunResult{Lineage: *lin}, err
	}
	setStatus(lin, entryID, "done")
	return RunResult{Lineage: *lin, Final: res.FinalText}, nil
}

// runPinned runs a deterministic chain: entry then each peer in order, each as its
// own loop. This is the shape a wallfacer flow compiles to.
func (r *Runner) runPinned(ctx context.Context, sb sandbox.SandboxProvider, sandboxID string, region Region, task string) (RunResult, error) {
	sess := r.session()
	chain := append([]AgentSpec{region.Entry}, region.Peers...)
	lin := &Lineage{}
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
			setStatus(lin, id, "failed")
			return RunResult{Lineage: *lin}, err
		}
		final = res.FinalText
		setStatus(lin, id, "done")
		prevID = id
	}
	return RunResult{Lineage: *lin, Final: final}, nil
}

// delegateTool is the agents-as-tools handoff primitive: it looks up a peer in the
// directory, spawns it with attenuated authority, runs the peer as a nested loop,
// records the lineage, and returns the peer's output as the tool result. A peer
// runs without a delegate tool (single-level handoff in M1; recursion is later).
type delegateTool struct {
	model   models.Model
	dir     []AgentSpec
	spawner *harness.Spawner
	parent  harness.ParentContext
	entryID string
	lineage *Lineage
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

func (d *delegateTool) Invoke(ctx context.Context, input json.RawMessage, sb sandbox.SandboxProvider, sandboxID string) (models.ToolResult, error) {
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

	child, err := d.spawner.Spawn(ctx, d.parent, harness.SpawnRequest{
		Label: peer.Name, Scopes: peer.Scopes, Tools: peer.Tools, Budget: d.parent.Budget,
	})
	if err != nil {
		return models.ToolResult{IsError: true, Content: "spawn failed: " + err.Error()}, nil
	}
	d.lineage.Nodes = append(d.lineage.Nodes, LineageNode{
		ID: child.ID, Name: peer.Name, Role: peer.Role, Status: "running", Grants: child.Perms.Tools,
	})
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: d.entryID, To: child.ID, Kind: "delegate"})

	// Run the peer as a real nested loop (its session id = the deterministic child
	// id). It reuses the parent sandbox here; per-child sandboxes are a later milestone.
	peerRes, err := loop.Run(ctx, loop.Config{
		Model:        d.model,
		Sandbox:      sb,
		SandboxID:    sandboxID,
		Tools:        tools.Builtins(),
		Bus:          hooks.New(),
		SessionID:    child.ID,
		AgentID:      peer.Name,
		SystemPrompt: peer.SystemPrompt,
		UserPrompt:   args.Task,
	})
	if err != nil {
		setStatus(d.lineage, child.ID, "failed")
		d.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "peer loop failed: " + err.Error()}, nil
	}
	setStatus(d.lineage, child.ID, "done")
	d.spawner.Stop(ctx, child)
	d.lineage.Edges = append(d.lineage.Edges, LineageEdge{From: child.ID, To: d.entryID, Kind: "deliver"})
	return models.ToolResult{Content: peerRes.FinalText}, nil
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
