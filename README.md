# Topos

**Latere Topos** is an embeddable Go agent runtime: define agents, compose them
into a region, and run them in-process — with attenuated sub-agent spawning, mesh
peer discovery, and a deterministic lineage graph. It is the open runtime that
powers both [Wallfacer](https://github.com/latere-ai) locally and the Topos cloud
service.

```go
import "latere.ai/x/topos"

r, _ := topos.NewRunner(topos.Options{
    Model: topos.ModelOptions{Kind: topos.ModelLux, BaseURL: "http://localhost:8080/anthropic"},
})
res, _ := r.Run(ctx, topos.Region{
    Autonomy: topos.Dynamic,
    Topology: topos.Mesh, // or topos.OrchestratorWorker (default)
    Entry:    topos.AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write"}},
    Peers: []topos.AgentSpec{
        {Name: "reviewer", Role: "review", Description: "reviews diffs", Tools: []string{"read"}},
    },
}, "ship the change")

fmt.Println(res.Final)
for _, n := range res.Lineage.Nodes { fmt.Println(n.ID, n.Status, n.Sandbox) }
```

## Concepts

- **Agent** — a name, role, system prompt, declared tools/scopes, and a model.
- **Region** — one unit of work with an autonomy mode (`Pinned` deterministic
  chain, or `Dynamic` model-decided handoffs) and a `Topology`
  (`OrchestratorWorker` default, or `Mesh`).
- **Delegation** is agents-as-tools: a `delegate` tool spawns the chosen peer with
  **attenuated** authority (a strict subset of the parent's), runs it in its own
  sandbox, and returns its result into the parent transcript.
- **Bounded recursion** — under `Mesh`, peers may delegate again, capped by
  `Options.MaxHandoffDepth` (default 3), so a run can't fan out unbounded.
- **Lineage** — every run yields a deterministic graph (who delegated/handed off to
  whom, with per-node status, granted tools, and sandbox), suitable for live render.

## Models via Lux

The model connection goes through [Lux](https://lux.latere.ai), the model gateway,
so provider secrets never live in the embedding app. `ModelOptions.Kind` selects
`ModelLux` (cloud, metered) / `ModelDirect` / `ModelFake` (deterministic, for
tests). Local development can point at a stateless `luxd` with your own keys.

## Status

Early. The root `topos` package is the supported, stable surface; the engine
subpackages (`harness`, `runtime/loop`, `models`, `sandbox`, …) are public for
advanced/host use but may change.

## License

[Apache-2.0](LICENSE).
