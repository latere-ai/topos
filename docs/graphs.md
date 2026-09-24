# Graphs

A region has one way of deciding who runs next. Work that needs two, such as
a planning step where the model decides whom to consult followed by a fixed
implement, test, and commit chain, is a graph: several regions run as one,
wired by the text one region hands the next.

## Running a graph

```go
g := topos.Graph{
	Regions: []topos.GraphRegion{
		{ID: "plan", Region: topos.Region{
			Autonomy: topos.Dynamic,
			Entry:    topos.AgentSpec{Name: "lead", Role: "lead"},
			Peers: []topos.AgentSpec{
				{Name: "architect", Role: "design", Description: "proposes a design"},
			},
		}},
		{ID: "ship", Region: topos.Region{
			Autonomy: topos.Pinned,
			Entry:    topos.AgentSpec{Name: "impl", Role: "impl"},
			Peers:    []topos.AgentSpec{{Name: "commit", Role: "commit"}},
		}},
	},
	// plan's final text becomes ship's task.
	Edges: []topos.GraphEdge{{From: "plan", To: "ship"}},
}

res, err := r.RunGraph(ctx, g, "design the feature")
if err != nil {
	return err
}
fmt.Println(res.Final) // the output of the last region
for _, e := range res.Trace.Edges {
	fmt.Println(e.From, "->", e.To, e.Kind)
}
```

Regions run one after another in topological order. A region with an incoming
edge receives its source's final text as its task; a region with none
receives the graph's task. The result's `Final` is the output of the last
region in that order.

Each region runs in a sandbox of its own, and nothing but text passes between
regions: a later region does not see the files an earlier one wrote. Inside a
pinned region the rule is different: every step receives the region's
original task, and one step's output is not piped into the next.

## What a graph may look like

A graph needs at least one region, and every region needs a unique, non-empty
id. Edges must connect known regions and must not point a region at itself.
Chains and fan-out, where one region feeds several, are allowed. Fan-in, a
region with more than one incoming edge, is refused, because there is no rule
yet for merging several texts into one task. Cycles are refused.

`RunGraph` checks all of this before any region runs and returns the problem
as an error. `topos.ValidateGraph` runs the same checks without running
anything, for an editor that wants to point at a mistake as it is made.

## The trace of a graph

The result's trace holds every region's nodes and edges, plus one edge of kind
`next` from each source region's entry agent to its target region's entry
agent, which is the flow between regions. Region ids prefix node ids
(`<session>/<region>/<agent>`), so two regions can each have an agent called
`lead` without their nodes colliding.

## Budgets in a graph

`Options.BudgetUSD` applies to each region separately. A graph of two regions
under a $10 cap may spend $20. A region stopped by the cap ends the graph, and
`RunGraph` returns the trace up to that point and that region's partial output
with an error matching `billing.ErrBudgetExceeded`.

## A graph a person authors and a host stores

`topos.Graph` is the shape the runner executes. It carries no JSON tags and
names its concepts for execution. A graph that a person builds in an editor,
that a host stores in a database, or that travels over an API uses
`latere.ai/x/topos/graph` instead:

```json
{
  "regions": [
    {
      "id": "plan",
      "coordination": "lead",
      "entry": {"name": "lead", "role": "lead"},
      "peers": [{"ref": "architect"}]
    },
    {
      "id": "ship",
      "coordination": "sequence",
      "entry": {"name": "impl", "role": "impl", "tools": ["read", "write", "exec"]},
      "peers": [{"name": "commit", "role": "commit"}]
    }
  ],
  "edges": [{"from": "plan", "to": "ship"}],
  "max_handoff_depth": 3
}
```

A region states how its agents coordinate with one field, `coordination`,
instead of the runtime's two:

| `coordination` | Runs as |
|---|---|
| `sequence` | `Pinned`: the entry agent, then each peer in order |
| `lead` | `Dynamic` with `OrchestratorWorker`: only the entry agent delegates |
| `mesh` | `Dynamic` with `Mesh`: any agent may delegate, down to the depth bound |

The JSON field names are the storage format and do not follow renames in the
Go types, so a stored graph keeps loading across releases. In `tools`, a
missing field grants every builtin and an empty list grants none, and that
distinction survives a round trip through JSON.

An agent is either inline, with its fields written out, or a reference: `ref`
names a shared definition kept somewhere else. The package never reads a
registry. The host supplies a resolver, and `Resolve` returns a copy of the
graph with every reference replaced:

```go
var authored graph.Graph
if err := json.Unmarshal(stored, &authored); err != nil {
	return err
}

resolved, err := authored.Resolve(func(ref string) (graph.Agent, error) {
	return registry.Lookup(ref) // the host's own store of shared agents
})
if err != nil {
	return err
}

g, err := resolved.ToRuntime()
if err != nil {
	return err // names the region or edge at fault
}

r, err := topos.NewRunner(topos.Options{
	Model:           model,
	MaxHandoffDepth: resolved.MaxHandoffDepth,
})
if err != nil {
	return err
}
res, err := r.RunGraph(ctx, g, "design the feature")
```

A `name` set on a reference is kept as the agent's name in this graph, so its
trace labels stay stable; otherwise the resolved agent's name is used. The
resolver must return an inline agent.

`ToRuntime` checks the authored fields first (a region needs an id and an
entry agent with a name, `coordination` must be one of the three values, an
edge needs both ends), then the structural rules above, and refuses a graph
that still holds a reference. A graph that lowers without an error runs
without a configuration error. `max_handoff_depth` is a property of the run
rather than of the graph, so `ToRuntime` does not carry it: pass it to
`Options.MaxHandoffDepth`, as above.

[`examples/graph`](../examples/graph) and
[`examples/authoredgraph`](../examples/authoredgraph) run both forms offline.
