// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package graph is the authored, serializable shape of a topos agent graph: the
// definition a person edits and a host persists, distinct from the in-memory
// execution shape ([latere.ai/x/topos.Graph]) it lowers to.
//
// The runtime types in the root package carry no JSON tags and name their
// concepts for execution (Autonomy, Topology). This package is the one canonical
// authoring model both a control plane and a graph-building UI serialize: every
// field is snake_case JSON-tagged and round-trips through encoding/json, and a
// region declares its coordination with a single first-class [Coordination] field
// instead of the runtime's autonomy+topology pair, which is the authoring insight
// (a person picks "sequence", "lead", or "mesh", not two orthogonal enums).
//
// The types are deliberately decoupled from the runtime's Go field names: a
// rename in the SDK must not break a persisted graph, so [Graph.ToRuntime] is the
// single mapping seam that lowers an authored graph to a runnable
// [latere.ai/x/topos.Graph] and encodes coordination back into autonomy+topology.
//
// An [Agent] is either inline (its spec fields carry the full definition) or a
// reference (a registry slug in [Agent.Ref] naming a shared definition). This
// package stays registry-free: it defines the reference shape and the
// [Graph.Resolve] hook a consumer calls to swap refs for inline agents, but it
// never reads a registry itself, and [Graph.ToRuntime] rejects a graph that still
// holds a ref.
package graph

import (
	"fmt"

	"latere.ai/x/topos"
)

// Coordination is how a region's agents decide who runs, chosen as one value
// rather than the runtime's orthogonal autonomy+topology pair. It is the authoring
// model's first-class field: an editor offers these three modes directly.
type Coordination string

const (
	// Sequence is a fixed deterministic chain: the entry agent then each peer in
	// order, no model-driven handoff. It lowers to a pinned region.
	Sequence Coordination = "sequence"
	// Lead is orchestrator-worker delegation: only the entry (lead) agent
	// delegates to peers, and a delegated peer runs without delegating further. It
	// lowers to a dynamic region with orchestrator-worker topology.
	Lead Coordination = "lead"
	// Mesh is peer-to-peer delegation: any agent may delegate to a peer, bounded
	// by the run's handoff depth. It lowers to a dynamic region with mesh topology.
	Mesh Coordination = "mesh"
)

// Agent is a declarative agent within a region. It is EITHER inline (the spec
// fields carry the full definition) OR a reference (Ref names a registry-defined
// agent whose spec a consumer supplies via [Graph.Resolve]). The JSON field names
// are the stable persistence contract, decoupled from [latere.ai/x/topos.AgentSpec]'s
// Go field names so an SDK rename cannot break a stored graph.
//
// When Ref is set the agent is a reference: the spec fields (Role, Description,
// SystemPrompt, Tools, Scopes) are ignored until [Graph.Resolve] replaces the ref
// with its resolved inline agent. Name is the exception: it is the in-graph
// identity and spawn label, so an author may set it on a ref to give the
// referenced agent a graph-local name (lineage labels stay stable). Resolve
// preserves an authored Name and otherwise adopts the resolved agent's Name; it
// takes the resolved agent's spec fields wholesale and merges no inline fields the
// ref itself carried. topos-lib never reads a registry: resolving a ref to an
// inline spec is the consumer's job, and [Graph.ToRuntime] rejects any graph that
// still holds a ref.
type Agent struct {
	Name         string   `json:"name,omitempty"`
	Ref          string   `json:"ref,omitempty"` // registry slug; set means this agent is a reference, resolved by a consumer before running
	Role         string   `json:"role,omitempty"`
	Description  string   `json:"description,omitempty"` // when-to-use; shown to a dynamic lead for discovery
	SystemPrompt string   `json:"system_prompt,omitempty"`
	Tools        []string `json:"tools,omitempty"`  // tool families this agent may use
	Scopes       []string `json:"scopes,omitempty"` // permission scopes this agent holds
}

// IsRef reports whether the agent is a reference to a registry-defined agent
// (Ref set) rather than an inline definition. A reference must be resolved to its
// inline form via [Graph.Resolve] before [Graph.ToRuntime] will lower the graph.
func (a Agent) IsRef() bool { return a.Ref != "" }

// Region is one named part of a graph: an entry agent, the way its agents
// coordinate, and its peers (the ordered chain for sequence, the delegable
// directory for lead and mesh). ID is unique across the graph and namespaces the
// region's runtime node ids.
type Region struct {
	ID           string       `json:"id"`
	Coordination Coordination `json:"coordination"` // sequence | lead | mesh
	Entry        Agent        `json:"entry"`
	Peers        []Agent      `json:"peers,omitempty"`
}

// Edge composes two regions by data flow: the source region's final output seeds
// the target region's task. It is the authored form of
// [latere.ai/x/topos.GraphEdge].
type Edge struct {
	From string `json:"from"` // source region id; its final output becomes To's task
	To   string `json:"to"`   // target region id
}

// Graph is a complete authored agent graph: regions and the edges between them.
// It is the persist/serialize/edit shape; [Graph.ToRuntime] lowers it to a
// runnable [latere.ai/x/topos.Graph].
//
// MaxHandoffDepth records the delegation-depth bound a persisted graph was
// authored with, so it survives a round-trip; it is not a property of the runtime
// graph but of the run, so [Graph.ToRuntime] does not consume it. A caller that
// runs the graph routes it into [latere.ai/x/topos.Options.MaxHandoffDepth].
type Graph struct {
	Regions         []Region `json:"regions"`
	Edges           []Edge   `json:"edges,omitempty"`
	MaxHandoffDepth int      `json:"max_handoff_depth,omitempty"`
}

// ToRuntime validates the authored graph and lowers it to a runnable
// [latere.ai/x/topos.Graph]. It runs two layers of checks and returns the first
// failure: authored-level field checks (a region needs an id and an entry agent
// name, a coordination must be one of sequence|lead|mesh, an edge needs both
// endpoints), then the structural checks [latere.ai/x/topos.ValidateGraph]
// enforces (at least one region, unique ids, known edge endpoints, no fan-in, no
// cycle). A returned error names the offending region or edge so an editor can
// point at it.
//
// Coordination lowers to autonomy+topology: sequence -> pinned; lead -> dynamic
// with orchestrator-worker; mesh -> dynamic with mesh.
func (g Graph) ToRuntime() (topos.Graph, error) {
	if len(g.Regions) == 0 {
		return topos.Graph{}, fmt.Errorf("graph has no regions")
	}
	var out topos.Graph
	for _, r := range g.Regions {
		if r.ID == "" {
			return topos.Graph{}, fmt.Errorf("region has no id")
		}
		if r.Entry.IsRef() {
			return topos.Graph{}, fmt.Errorf("region %q entry is an unresolved ref %q: resolve refs before running", r.ID, r.Entry.Ref)
		}
		if r.Entry.Name == "" {
			return topos.Graph{}, fmt.Errorf("region %q has no entry agent", r.ID)
		}
		autonomy, topology, err := r.Coordination.lower()
		if err != nil {
			return topos.Graph{}, fmt.Errorf("region %q: %w", r.ID, err)
		}
		region := topos.Region{
			Autonomy: autonomy,
			Topology: topology,
			Entry:    r.Entry.toRuntime(),
		}
		for _, p := range r.Peers {
			if p.IsRef() {
				return topos.Graph{}, fmt.Errorf("region %q peer is an unresolved ref %q: resolve refs before running", r.ID, p.Ref)
			}
			region.Peers = append(region.Peers, p.toRuntime())
		}
		out.Regions = append(out.Regions, topos.GraphRegion{ID: r.ID, Region: region})
	}
	for _, e := range g.Edges {
		if e.From == "" || e.To == "" {
			return topos.Graph{}, fmt.Errorf("edge needs both from and to")
		}
		out.Edges = append(out.Edges, topos.GraphEdge{From: e.From, To: e.To})
	}
	if err := topos.ValidateGraph(out); err != nil {
		return topos.Graph{}, err
	}
	return out, nil
}

// Resolve returns a copy of the graph with every referenced agent (one whose Ref
// is set) replaced by its inline definition, obtained by calling resolve with the
// ref slug. It is the seam by which a consumer supplies inline specs from its own
// registry: topos-lib never reads a registry itself, so the resolver is the only
// place a ref becomes an inline agent. Inline agents pass through untouched, so a
// fully-inline graph is returned unchanged (a no-op resolve).
//
// An authored Name on a ref is preserved as the in-graph identity and spawn label;
// otherwise the resolved agent's Name is adopted. The resolved agent's spec fields
// are taken wholesale, and the resolver must return an inline agent: if it returns
// one that is itself a ref, Resolve fails rather than lower an unresolved graph.
// The original graph is not mutated.
func (g Graph) Resolve(resolve func(ref string) (Agent, error)) (Graph, error) {
	out := g
	out.Regions = make([]Region, len(g.Regions))
	for i, r := range g.Regions {
		entry, err := r.Entry.resolve(resolve)
		if err != nil {
			return Graph{}, fmt.Errorf("region %q entry: %w", r.ID, err)
		}
		r.Entry = entry
		if r.Peers != nil {
			peers := make([]Agent, len(r.Peers))
			for j, p := range r.Peers {
				resolved, err := p.resolve(resolve)
				if err != nil {
					return Graph{}, fmt.Errorf("region %q peer %d: %w", r.ID, j, err)
				}
				peers[j] = resolved
			}
			r.Peers = peers
		}
		out.Regions[i] = r
	}
	return out, nil
}

// resolve replaces a referenced agent with its inline definition; an inline agent
// is returned unchanged. An authored Name overrides the resolved agent's Name so a
// graph-local identity survives resolution.
func (a Agent) resolve(resolve func(ref string) (Agent, error)) (Agent, error) {
	if !a.IsRef() {
		return a, nil
	}
	inline, err := resolve(a.Ref)
	if err != nil {
		return Agent{}, fmt.Errorf("resolve ref %q: %w", a.Ref, err)
	}
	if inline.IsRef() {
		return Agent{}, fmt.Errorf("ref %q resolved to another ref %q", a.Ref, inline.Ref)
	}
	if a.Name != "" {
		inline.Name = a.Name
	}
	return inline, nil
}

// lower maps a coordination mode to the runtime's autonomy and topology. A
// sequence has no topology (it never delegates); lead and mesh are dynamic. An
// empty or unknown mode is an error, never a silent default, so a malformed
// authored graph cannot lower to an empty autonomy the runtime would reject with
// a less specific message.
func (c Coordination) lower() (topos.Autonomy, topos.Topology, error) {
	switch c {
	case Sequence:
		return topos.Pinned, "", nil
	case Lead:
		return topos.Dynamic, topos.OrchestratorWorker, nil
	case Mesh:
		return topos.Dynamic, topos.Mesh, nil
	case "":
		return "", "", fmt.Errorf("coordination is required (sequence|lead|mesh)")
	default:
		return "", "", fmt.Errorf("unknown coordination %q (want sequence|lead|mesh)", c)
	}
}

func (a Agent) toRuntime() topos.AgentSpec {
	return topos.AgentSpec{
		Name:         a.Name,
		Role:         a.Role,
		Description:  a.Description,
		SystemPrompt: a.SystemPrompt,
		Tools:        a.Tools,
		Scopes:       a.Scopes,
	}
}
