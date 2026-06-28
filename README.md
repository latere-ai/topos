# Topos

**Latere Topos** is an embeddable Go agent runtime. A host application defines
agents, composes them into a region, and runs them in-process. The runtime provides
sub-agent spawning with attenuated permissions, peer discovery for multi-agent work,
and a deterministic lineage graph of everything that ran.

```go
import "latere.ai/x/topos"

r, _ := topos.NewRunner(topos.Options{
    Model: topos.ModelOptions{Kind: topos.ModelLux, BaseURL: "http://localhost:8080/anthropic"},
})
res, _ := r.Run(ctx, topos.Region{
    Autonomy: topos.Dynamic,
    Topology: topos.Mesh, // or topos.OrchestratorWorker (the default)
    Entry:    topos.AgentSpec{Name: "lead", Role: "lead", Tools: []string{"read", "write"}},
    Peers: []topos.AgentSpec{
        {Name: "reviewer", Role: "review", Description: "reviews diffs", Tools: []string{"read"}},
    },
}, "ship the change")

fmt.Println(res.Final)
for _, n := range res.Lineage.Nodes {
    fmt.Println(n.ID, n.Status, n.Sandbox)
}
```

## Concepts

**Agent.** A name, a role, a system prompt, the tools and scopes it is allowed to
use, and a model.

**Region.** One unit of work. Its autonomy mode is either `Pinned` (a deterministic
chain of agents, like a fixed pipeline) or `Dynamic` (the model decides who to hand
off to at runtime). Its `Topology` is either `OrchestratorWorker`, the default,
where only the entry agent delegates, or `Mesh`, where peers can delegate too.

**Delegation.** Handing work to a peer is a tool call. The `delegate` tool spawns
the chosen peer with attenuated authority, meaning a strict subset of the parent's
tools and scopes, runs it in its own sandbox, and returns its result back into the
parent's transcript.

**Bounded recursion.** Under `Mesh`, a peer can delegate again. `Options.MaxHandoffDepth`
(default 3) caps how deep that can go, so a run cannot fan out without limit.

**Lineage.** Every run produces a deterministic graph: who delegated or handed off
to whom, with each node's status, the tools it was granted, and the sandbox it ran
in. The ids are stable, so runs can be diffed or rendered live.

## Models through Lux

The model connection goes through [Lux](https://lux.latere.ai), a model gateway, so
provider keys never live in the host application. `ModelOptions.Kind` chooses the
backend: `ModelLux` for the gateway, `ModelDirect` for a provider endpoint, or
`ModelFake` for a deterministic model suitable for tests. For local development,
`ModelLux` can point at a stateless `luxd` running with local provider keys.

## Sandboxes

Every run executes in a sandbox, and each delegated peer gets its own. The
backend is pluggable through the `sandbox.Provider` interface. By default the
runner uses `sandbox/local`, a temp-directory implementation that needs no
external services — the zero-config path for development and tests.

For hosted compute, inject a backend via `Options.Sandbox`. The `sandbox/cella`
provider backs runs with [Latere Cella](https://cella.latere.ai), the hosted
Kubernetes sandbox platform:

```go
import (
    "latere.ai/x/topos"
    "latere.ai/x/topos/sandbox"
    "latere.ai/x/topos/sandbox/cella"
)

prov := cella.New(cella.Options{
    BaseURL: "https://cella.latere.ai",
    Token:   cella.ContextTokenSource{}, // reads the bearer set by sandbox.WithBearer
})
r, _ := topos.NewRunner(topos.Options{Sandbox: prov, Model: /* ... */})

// Scope the whole run to the session user's Cella identity.
ctx = sandbox.WithBearer(ctx, userBearer)
res, _ := r.Run(ctx, region, task)
```

The host owns minting the Cella bearer (exchanging the user's token); the
provider only presents it. The root `topos` package never imports a concrete
backend — a host wires one in as the interface.

### Secrets

Secrets the agent's workload needs (provider keys, tokens) are never passed as
plaintext. The host stores them in Cella's vault out of band and references them
by name. Mount them as read-only files at sandbox start with
`CreateOptions.SecretMounts`, or inject one into a single command with
`ExecOptions.SecretEnv` (resolved server-side, never on argv):

```go
opts := sandbox.CreateOptions{SecretMounts: []string{"OPENAI_API_KEY"}}
// ... or per command:
exec := sandbox.ExecOptions{
    Argv:      []string{"deploy"},
    SecretEnv: map[string]string{"OPENAI_API_KEY": "openai_key"}, // env var -> vault entry
}
```

A nil `SecretMounts` mounts the caller's default set; an empty slice mounts
none. The local provider has no vault and ignores both fields. (Separately, the
lift/drop deny-list keeps laptop secrets like `.env` and `*.pem` from ever
entering a sandbox.) Plain `Env` remains the channel for non-secret config.

## Status

Early. The root `topos` package is the supported surface and is what most callers
should use. The engine subpackages (`harness`, `runtime/loop`, `models`, `sandbox`,
and others) are public for advanced and host use, but their APIs may still change.

## License

[Apache-2.0](LICENSE).
