// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package topos is the public, embeddable surface of the Topos agent runtime.
//
// A host application imports this package to define agents, compose them into
// regions, and run a single region ([Runner.Run]) or a graph of regions
// ([Runner.RunGraph]) locally and in-process — without importing anything under
// internal/. Everything exported here uses only topos-defined or standard-library
// types, so the boundary holds across the module edge (Go forbids cross-module
// internal/ imports; this package is the curated public API on the inside of
// that edge).
//
// The runner executes agents through the real agentic loop (internal/runtime/loop):
// the model is the brain (configured via ModelOptions — Lux, a direct provider, or
// the deterministic fake), and a handoff is an agents-as-tools delegation — a
// `delegate` tool registered into the loop whose Invoke performs a real attenuated
// Spawner spawn, runs the chosen peer as a nested loop, and returns its result into
// the parent transcript.
package topos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
// and dynamic regions; [Runner.Run] runs a single region, [Runner.RunGraph] a graph.
type Region struct {
	Autonomy Autonomy
	Topology Topology    // dynamic only; default OrchestratorWorker
	Entry    AgentSpec   // the agent that starts the region
	Peers    []AgentSpec // discoverable peers (dynamic) or the ordered chain (pinned)
}

// GraphRegion is a named region within a [Graph]. ID is unique across the graph
// and namespaces the region's node ids, so agents that share a Name in different
// regions still get distinct lineage ids.
type GraphRegion struct {
	ID     string
	Region Region
}

// GraphEdge composes two regions by data flow: the source region's Final seeds the
// target region's task. This is deliberately different from a pinned region's
// internal chain, where every step receives the original task and outputs are not
// piped — regions compose by threading text, steps within a region do not.
type GraphEdge struct {
	From string // source region ID; its Final becomes To's input task
	To   string // target region ID
}

// Graph composes several regions — pinned or dynamic, in any mix — into one run.
// Regions execute in topological order; an edge From->To seeds To's task with
// From's Final, and a region with no incoming edge receives the graph's input
// task. Composition is text-only (a region's observable output is its Final; the
// contract carries no sandbox handle), so each region runs in its own isolated
// sandbox. Execution is sequential, which keeps lineage mutation single-threaded
// (OQ-4). Fan-in (a region with more than one incoming edge) is rejected: merging
// several upstream Finals into one task is an unspecified product decision, so
// v1 allows linear chains and fan-out (a forest) only.
type Graph struct {
	Regions []GraphRegion
	Edges   []GraphEdge
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

// Event is a single observation emitted during a run. It mirrors one internal
// hook dispatch in a subpackage-free shape so embedders can subscribe without
// importing internal types. SessionID is the emitting agent's loop session id,
// which equals the corresponding Lineage node id for agentic runs — so a live
// consumer can join events to graph nodes. AgentID is the agent name when the
// underlying payload carries one (else ""). PayloadJSON is the full typed payload
// marshalled to JSON (audit/replay grade).
type Event struct {
	Name        string          // event name; compare against the Event* constants
	SessionID   string          // emitting agent's loop session id == Lineage node id
	AgentID     string          // agent name when available, else ""
	At          time.Time       // dispatch time (UTC)
	PayloadJSON json.RawMessage // full payload, JSON-marshalled
}

// Event name constants an embedder is likely to switch on. They mirror the
// internal hook names as plain strings so observers need no subpackage import.
const (
	EventSessionStart     = "SessionStart"
	EventUserPromptSubmit = "UserPromptSubmit"
	EventAssistantMessage = "AssistantMessage"
	// EventTextDelta carries one streamed fragment of assistant text (a token or
	// few). An observer receives many of these per turn for token-by-token
	// rendering, followed by the assembled EventAssistantMessage for the turn.
	EventTextDelta = "TextDelta"
	// EventUsage carries running token usage after each turn (for a cost/usage HUD).
	EventUsage         = "Usage"
	EventPostToolUse   = "PostToolUse"
	EventSubagentStart = "SubagentStart"
	EventSubagentStop  = "SubagentStop"
	EventStop          = "Stop"
	EventSessionEnd    = "SessionEnd"
)

// Options configure a Runner.
type Options struct {
	SessionID       string       // stable run id; deterministic child ids derive from it
	Model           ModelOptions // the brain connection (Lux / direct / fake)
	BudgetUSD       float64      // region spend cap, sub-allocated to delegates
	MaxHandoffDepth int          // max delegation depth in a Mesh region (default 3); bounds recursion

	// Observer, when non-nil, receives every event the run emits (lifecycle,
	// tool use, delegation, per-turn assistant text), in dispatch order. It is
	// purely observational — the return value cannot alter control flow — so a
	// host can render a live trace. It is called synchronously on the run's
	// goroutine(s): a slow observer backpressures the run, so a host should push
	// to a buffered channel and return. A panic in Observer is recovered and
	// logged; it never crashes the run. Mesh peers may run and emit such that
	// events from different agents interleave; demultiplex on SessionID.
	Observer func(Event)

	// Sandbox is the execution backend for the run and every delegated peer.
	// When nil, the runner uses the local temp-directory provider
	// (sandbox/local), so the zero-config path needs no external services. A
	// host wanting hosted compute injects a backend here (e.g. cella.New(...))
	// as the interface; the root package never imports a concrete backend.
	Sandbox sandbox.Provider

	// Workdir, when non-empty and Sandbox is nil, roots the default local
	// provider at this existing host directory (typically a git worktree)
	// instead of a temp dir, so the run's tools read and write that directory
	// directly. It is the zero-import seam for an embedding host that wants
	// in-process execution against a real checkout without depending on the
	// sandbox subpackage. Ignored when Sandbox is set (the injected backend
	// owns execution location).
	Workdir string

	// Brain, when non-nil, is the model the runner uses directly, ignoring
	// Model. It lets a host plug in its own models.Model (a custom provider
	// adapter, or a scripted model for tests and examples) instead of the
	// built-in Lux, Direct, or Fake kinds.
	Brain models.Model
}

// Runner executes regions in-process through the real agentic loop, against the
// real attenuating Spawner and hook bus.
type Runner struct {
	opts    Options
	model   models.Model
	bus     *hooks.Bus
	spawner *harness.Spawner
}

// NewRunner builds a Runner. It uses Options.Brain when set, otherwise it
// constructs the model from Options.Model.
func NewRunner(opts Options) (*Runner, error) {
	m := opts.Brain
	if m == nil {
		var err error
		if m, err = buildModel(opts.Model); err != nil {
			return nil, err
		}
	}
	bus := hooks.New()
	if opts.Observer != nil {
		registerObserver(bus, opts.Observer)
	}
	return &Runner{opts: opts, model: m, bus: bus, spawner: harness.NewSpawner(bus)}, nil
}

// registerObserver bridges the internal hook bus to a host's Observer. It adapts
// every dispatched event into a subpackage-free topos.Event and never influences
// control flow (always returns Allow). The host callback is wrapped in a recover
// so a buggy observer cannot panic the run.
func registerObserver(bus *hooks.Bus, observer func(Event)) {
	bus.Register("topos.observer", nil, func(name hooks.EventName, payload any) hooks.Decision {
		raw, err := json.Marshal(payload)
		if err != nil {
			raw = nil
		}
		// SessionID / AgentID live in the typed payloads under stable json tags;
		// pull them generically so this adapter is payload-type-agnostic.
		var meta struct {
			SessionID string `json:"session_id"`
			AgentID   string `json:"agent_id"`
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &meta)
		}
		ev := Event{
			Name:        string(name),
			SessionID:   meta.SessionID,
			AgentID:     meta.AgentID,
			At:          time.Now().UTC(),
			PayloadJSON: raw,
		}
		func() {
			defer func() { _ = recover() }()
			observer(ev)
		}()
		return hooks.Decision{Verdict: hooks.VerdictAllow}
	})
}

// Run executes a region in-process (a sandbox is created for the run via the
// configured provider, or sandbox/local when none is set) and returns its
// lineage graph. task is the user request handed to the entry agent.
func (r *Runner) Run(ctx context.Context, region Region, task string) (RunResult, error) {
	return r.runRegion(ctx, r.provider(), r.session(), region, task)
}

// runRegion provisions an isolated sandbox for one region, dispatches to the
// autonomy-specific runner, and tears the sandbox down. regionSession namespaces
// the region's node and child ids (r.session() for a single-region Run; a
// per-region namespace for a graph). It is the shared unit both Run and RunGraph
// execute so region isolation and lifecycle stay identical across the two paths.
func (r *Runner) runRegion(ctx context.Context, p sandbox.Provider, regionSession string, region Region, task string) (RunResult, error) {
	sb, err := p.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		return RunResult{}, fmt.Errorf("topos: create sandbox: %w", err)
	}
	defer p.Destroy(ctx, sb.ID) //nolint:errcheck
	if err := waitRunning(ctx, p, sb.ID); err != nil {
		return RunResult{}, fmt.Errorf("topos: sandbox not ready: %w", err)
	}

	switch region.Autonomy {
	case Dynamic:
		return r.runDynamic(ctx, p, sb.ID, regionSession, region, task)
	case Pinned:
		return r.runPinned(ctx, p, sb.ID, regionSession, region, task)
	default:
		return RunResult{}, fmt.Errorf("topos: unknown autonomy %q", region.Autonomy)
	}
}

// RunGraph executes a multi-region graph as one run and returns the merged
// lineage and the final region's output. Regions run in topological order, each in
// its own sandbox; a region's input is the graph task, or — when an edge points at
// it — the source region's Final. The returned Lineage concatenates every region's
// nodes and edges, plus an EdgeNext from each source region's entry node to its
// target region's entry node so a consumer can see the region-level flow. Region
// ids namespace node ids (<session>/<regionID>/<agent>), so agents sharing a name
// across regions do not collide. Final is the output of the last region in
// topological order. A cycle, an unknown edge endpoint, a duplicate/empty region
// id, or a fan-in (more than one incoming edge) is a configuration error returned
// before any region runs.
func (r *Runner) RunGraph(ctx context.Context, g Graph, task string) (RunResult, error) {
	order, err := planGraph(g)
	if err != nil {
		return RunResult{}, err
	}
	source := map[string]string{} // region ID -> its single upstream region ID
	for _, e := range g.Edges {
		source[e.To] = e.From
	}

	p := r.provider()
	sess := r.session()
	merged := Lineage{}
	finals := map[string]string{}   // region ID -> Final
	entryIDs := map[string]string{} // region ID -> entry node id (first node)
	var last string
	for _, gr := range order {
		in := task
		if src, ok := source[gr.ID]; ok {
			in = finals[src]
		}
		res, err := r.runRegion(ctx, p, sess+"/"+gr.ID, gr.Region, in)
		merged.Nodes = append(merged.Nodes, res.Lineage.Nodes...)
		merged.Edges = append(merged.Edges, res.Lineage.Edges...)
		if len(res.Lineage.Nodes) > 0 {
			entryIDs[gr.ID] = res.Lineage.Nodes[0].ID
		}
		if src, ok := source[gr.ID]; ok {
			if from, fromOK := entryIDs[src]; fromOK {
				if to, toOK := entryIDs[gr.ID]; toOK {
					merged.Edges = append(merged.Edges, LineageEdge{From: from, To: to, Kind: EdgeNext})
				}
			}
		}
		if err != nil {
			return RunResult{Lineage: merged}, fmt.Errorf("topos: region %q: %w", gr.ID, err)
		}
		finals[gr.ID] = res.Final
		last = res.Final
	}
	return RunResult{Lineage: merged, Final: last}, nil
}

// planGraph validates a graph and returns its regions in a deterministic
// topological order (Kahn's algorithm, seeded and expanded in declared order). It
// rejects empty/duplicate region ids, edges referencing unknown regions, self
// edges, fan-in (an unsupported product decision in v1), and cycles.
func planGraph(g Graph) ([]GraphRegion, error) {
	if len(g.Regions) == 0 {
		return nil, fmt.Errorf("topos: graph has no regions")
	}
	byID := make(map[string]GraphRegion, len(g.Regions))
	for _, gr := range g.Regions {
		if gr.ID == "" {
			return nil, fmt.Errorf("topos: graph region has an empty id")
		}
		if _, dup := byID[gr.ID]; dup {
			return nil, fmt.Errorf("topos: duplicate region id %q", gr.ID)
		}
		byID[gr.ID] = gr
	}
	indeg := make(map[string]int, len(g.Regions))
	adj := make(map[string][]string, len(g.Edges))
	for _, e := range g.Edges {
		if _, ok := byID[e.From]; !ok {
			return nil, fmt.Errorf("topos: edge from unknown region %q", e.From)
		}
		if _, ok := byID[e.To]; !ok {
			return nil, fmt.Errorf("topos: edge to unknown region %q", e.To)
		}
		if e.From == e.To {
			return nil, fmt.Errorf("topos: self edge on region %q", e.From)
		}
		indeg[e.To]++
		adj[e.From] = append(adj[e.From], e.To)
	}
	for _, gr := range g.Regions {
		if indeg[gr.ID] > 1 {
			return nil, fmt.Errorf("topos: region %q has %d incoming edges; fan-in is not yet supported", gr.ID, indeg[gr.ID])
		}
	}
	order := make([]GraphRegion, 0, len(g.Regions))
	queue := make([]string, 0, len(g.Regions))
	for _, gr := range g.Regions {
		if indeg[gr.ID] == 0 {
			queue = append(queue, gr.ID)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, byID[id])
		for _, nb := range adj[id] {
			indeg[nb]--
			if indeg[nb] == 0 {
				queue = append(queue, nb)
			}
		}
	}
	if len(order) != len(g.Regions) {
		return nil, fmt.Errorf("topos: graph has a cycle")
	}
	return order, nil
}

// TurnInput configures a single interactive turn run by [Runner.Turn]. Unlike a
// Region run, a turn is one agent against a caller-owned sandbox, seeded from a
// prior transcript — the building block of a multi-turn, resumable session.
type TurnInput struct {
	// Sandbox is the execution backend the turn's tools run against. Turn does
	// NOT create or destroy it: the caller owns this sandbox across the whole
	// session, so a persistent workspace survives between turns. Required.
	Sandbox sandbox.Provider
	// SandboxID is the caller-owned sandbox instance to run tools in. Required.
	SandboxID string
	// AgentID labels the agent for event payloads and lineage joins.
	AgentID string
	// SystemPrompt is the agent's static system instruction.
	SystemPrompt string
	// Tools is the tool registry offered to the model this turn. When nil, the
	// built-in tool set (tools.Builtins) is used. A host injects its own
	// registry here to add MCP, skills, or governed tools.
	Tools *tools.Registry
	// InitialTranscript seeds the conversation with the prior turns. For the
	// first turn it is empty; for every later turn it is the transcript returned
	// by the previous Turn, so the model sees the full history.
	InitialTranscript []models.Message
	// UserPrompt is the new user message for this turn. It is appended after
	// InitialTranscript. Empty means "continue without new input" (e.g. resuming
	// an interrupted turn).
	UserPrompt string
	// MaxTokens caps the model response size (0 = provider default).
	MaxTokens int
}

// TurnResult is the outcome of a single [Runner.Turn].
type TurnResult struct {
	// Transcript is the full conversation after this turn (the seed plus the new
	// user, assistant, and tool messages). It is the canonical state to persist
	// and to feed as the next turn's InitialTranscript. On an interrupted turn
	// it holds the conversation up to the cut, including the partial assistant
	// turn in progress.
	Transcript []models.Message
	// Final is the last assistant text of the turn (may be empty if the turn
	// ended on a tool call or was interrupted before any text).
	Final string
	// StopReason is the model's terminal signal for the turn.
	StopReason models.StopReason
	// Usage is the token accounting for this turn.
	Usage models.Usage
	// ToolCalls is the number of tool calls executed this turn.
	ToolCalls int
	// Interrupted is true when the turn was cut short by context cancellation
	// (the caller cancelled to interrupt). It is the sentinel that distinguishes
	// a user interrupt — a normal control action whose partial Transcript is
	// kept — from a genuine failure (which Turn reports as a non-nil error). On
	// an interrupted turn the error is nil and Interrupted is true.
	Interrupted bool
}

// Turn runs a single interactive turn of one agent and returns the updated
// transcript. It is the stable entry point a host (such as the Topos control
// plane) uses to drive a multi-turn, resumable session: thread TurnResult.
// Transcript back in as the next TurnInput.InitialTranscript, turn after turn.
//
// Unlike [Runner.Run], Turn neither creates nor destroys the sandbox (the caller
// owns a persistent workspace for the session's lifetime) and runs exactly one
// agent (no delegation). Cancelling ctx interrupts the turn: Turn then returns
// the partial transcript with Interrupted set and a nil error, because an
// interrupt is an expected user action, not a failure. A genuine infrastructure
// failure is returned as a non-nil error (with whatever partial transcript was
// captured).
//
// Observability flows through Options.Observer exactly as for Run: the host sees
// token deltas (EventTextDelta), assistant messages, tool use, and lifecycle
// events. Because the runner's bus is shared across a session's turns, a single
// Observer registered at NewRunner sees every turn.
func (r *Runner) Turn(ctx context.Context, in TurnInput) (TurnResult, error) {
	reg := in.Tools
	if reg == nil {
		reg = tools.Builtins()
	}
	res, err := loop.Run(ctx, loop.Config{
		Model:             r.model,
		Sandbox:           in.Sandbox,
		SandboxID:         in.SandboxID,
		Tools:             reg,
		Bus:               r.bus,
		SessionID:         r.session(),
		AgentID:           in.AgentID,
		SystemPrompt:      in.SystemPrompt,
		UserPrompt:        in.UserPrompt,
		InitialTranscript: in.InitialTranscript,
		MaxTokens:         in.MaxTokens,
	})
	// loop.Run returns a non-nil partial result on interrupt, and a nil result
	// only on an unrecoverable infrastructure failure.
	if res == nil {
		return TurnResult{}, err
	}
	out := TurnResult{
		Transcript: res.Transcript,
		Final:      res.FinalText,
		StopReason: res.StopReason,
		Usage:      res.TotalUsage,
		ToolCalls:  res.ToolCallCount,
	}
	if errors.Is(err, loop.ErrInterrupted) {
		// An interrupt is a normal control action: surface it via the flag and a
		// nil error so the host keeps the partial transcript without treating it
		// as a failure.
		out.Interrupted = true
		return out, nil
	}
	return out, err
}

// readyTimeout and readyInterval bound how long Run waits for a freshly created
// sandbox to reach the running state before giving up. They are vars (not
// consts) so tests can shrink them; production code does not change them.
var (
	readyTimeout  = 30 * time.Second
	readyInterval = 200 * time.Millisecond
)

// waitRunning polls HealthCheck until the sandbox is running, the context ends,
// or readyTimeout elapses. A backend whose Create already returns a running
// sandbox (e.g. sandbox/local) passes on the first check, so this is a no-op
// there; an async backend (e.g. Cella, which may return "creating") is given
// time to warm up. A vanished sandbox (ErrNotFound) fails immediately.
func waitRunning(ctx context.Context, p sandbox.Provider, id string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	for {
		err := p.HealthCheck(ctx, id)
		if err == nil {
			return nil
		}
		if errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("sandbox %s not running: %w (last: %s)", id, ctx.Err(), err.Error())
		case <-time.After(readyInterval):
		}
	}
}

// provider returns the configured sandbox backend, defaulting to the local
// temp-directory provider when the host did not inject one.
func (r *Runner) provider() sandbox.Provider {
	if r.opts.Sandbox != nil {
		return r.opts.Sandbox
	}
	if r.opts.Workdir != "" {
		return local.NewAt(r.opts.Workdir)
	}
	return local.New()
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
// regionSession is the id namespace for this region's nodes and child ids. For a
// single-region Run it is r.session(); for a multi-region RunGraph the graph
// namespaces each region (<session>/<regionID>) so agents sharing a name across
// regions get distinct ids. It seeds both the lineage node id (loopSession) and
// the Spawner child id contract (<regionSession>/sub/<label>).
func (r *Runner) runDynamic(ctx context.Context, sb sandbox.Provider, sandboxID, regionSession string, region Region, task string) (RunResult, error) {
	sess := regionSession
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
		// loopSession matches the lineage node id (entryID), consistent with
		// delegated peers (child.ID) and pinned steps (id). This makes an event's
		// SessionID a reliable join key to its Lineage node. Child id derivation
		// uses parent.SessionID (sess), so it is unaffected.
		task: task, lin: lin, nodeID: entryID, loopSession: entryID, path: "",
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
func (r *Runner) runPinned(ctx context.Context, sb sandbox.Provider, sandboxID, regionSession string, region Region, task string) (RunResult, error) {
	sess := regionSession
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

	// Per-child sandbox: the peer runs in its own sandbox. A provisioning
	// failure is the child's, not the parent's.
	box, err := sb.Create(ctx, sandbox.CreateOptions{})
	if err != nil {
		d.appendChild(child, peer, StatusFailed, "")
		d.runner.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "sandbox create failed: " + err.Error()}, nil
	}
	defer sb.Destroy(ctx, box.ID) //nolint:errcheck
	if err := waitRunning(ctx, sb, box.ID); err != nil {
		d.appendChild(child, peer, StatusFailed, "")
		d.runner.spawner.Stop(ctx, child)
		return models.ToolResult{IsError: true, Content: "sandbox not ready: " + err.Error()}, nil
	}
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
