# Topos Runtime

**Topos Runtime** is the embeddable Go agent runtime at the core of
[Topos](https://topos.latere.ai), the Latere agent platform. A host application
defines agents, composes them into a region, and runs them in-process. The runtime
provides sub-agent spawning with attenuated permissions, peer discovery for
multi-agent work, and a deterministic lineage graph of everything that ran.

The Topos platform is one host built on this runtime; any Go application can be
another.

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

## Interactive, resumable turns

`Runner.Run` runs a region start to finish. For a back-and-forth session — a chat
assistant, a coding agent you steer turn by turn — use `Runner.Turn` instead. A
turn is one agent against a sandbox **you** own, seeded from the conversation so
far:

```go
r, _ := topos.NewRunner(topos.Options{
    SessionID: "sess-42",
    Model:     topos.ModelOptions{Kind: topos.ModelLux},
    Observer:  func(e topos.Event) { /* render e.Name == topos.EventTextDelta live */ },
})

// You create and keep the sandbox for the whole session, so the workspace
// (files, installed deps) survives between turns.
var transcript []models.Message
for _, prompt := range []string{"add a test for parse()", "now make it pass"} {
    res, _ := r.Turn(ctx, topos.TurnInput{
        Sandbox: sb, SandboxID: sbID,
        InitialTranscript: transcript, // the history threads forward
        UserPrompt:        prompt,
    })
    transcript = res.Transcript // persist this; it is the canonical state
    fmt.Println(res.Final)
}
```

Three properties make a turn safe to drive from a server:

- **The transcript is the state.** `TurnResult.Transcript` is the full
  conversation; feed it back as the next turn's `InitialTranscript`. Persist it
  and you can resume the session later, even on another machine.
- **Interrupt keeps the work.** Cancel the context to interrupt a long turn
  (a user hitting Esc). `Turn` returns the *partial* transcript with
  `Interrupted == true` and a nil error — an interrupt is a control action, not a
  failure.
- **Tokens stream.** The `Observer` receives `EventTextDelta` for each fragment
  as the model writes, then the assembled `EventAssistantMessage` for the turn.
  The observer is synchronous, so a host should hand off to a buffered channel
  and return rather than block on I/O.

## Sandboxes

Every run executes in a sandbox, and each delegated peer gets its own. The
backend is pluggable through the `sandbox.Provider` interface. By default the
runner uses `sandbox/local`, a temp-directory implementation that needs no
external services. It is the zero-config path for development and tests.

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
backend; a host wires one in as the interface.

### Authenticating to Cella

The host owns the token; the provider asks the configured `TokenSource` for it on
every request and sends it as `Authorization: Bearer …`. The provider stores no
token, so a rotated credential flows through automatically. Choose the source
that matches the host's ownership model:

| Source | Use when | Refresh behaviour |
|---|---|---|
| `StaticTokenSource("tok")` | one fixed token for the process (CLI, service account, dev) | none; fixed at construction |
| `TokenFunc(func(ctx) (string, error))` | the host holds the token and rotates it out of band | **picks up refreshes**; called per request, returns the current token |
| `ContextTokenSource{}` | multi-tenant: a different user's token per request, set with `sandbox.WithBearer(ctx, tok)` | per-request, but fixed for the context passed (a long run will not see a mid-run refresh) |

```go
// Host-held token that may be refreshed elsewhere. The recommended shape when
// the host owns the credential and rotation should flow through with no re-wiring:
prov := cella.New(cella.Options{
    BaseURL: "https://cella.latere.ai",
    Token: cella.TokenFunc(func(ctx context.Context) (string, error) {
        return auth.CurrentToken(), nil // the host's cached, out-of-band-refreshed token
    }),
})
```

Cella issues the token (dashboard/CLI, or `POST /v1/tokens/exchange`); obtaining
and refreshing it is the host's job, not the provider's.

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
