# Topos Runtime guides

For a Go program that embeds the runtime. The [README](../README.md) covers
installation, a first run, and the concepts these pages build on. The
[package reference](https://pkg.go.dev/latere.ai/x/topos) is the complete API,
and the [examples](../examples) run offline.

| Guide | What it covers |
|---|---|
| [Sessions](sessions.md) | `Runner.Turn`: one agent, a sandbox the host keeps, a transcript the host stores, streamed tokens, interrupts, and the events a host observes |
| [Graphs](graphs.md) | `Runner.RunGraph`, the rules a graph must satisfy, and the JSON form in `latere.ai/x/topos/graph` |
| [Models and budgets](models.md) | Lux, a provider called directly, a custom model, how a turn is priced, and what the spend cap guarantees |
| [Sandboxes](sandboxes.md) | the local provider, a host directory, hosted Cella, secrets, the path deny-list, approval before a command runs, and serving a machine as a sandbox |
| [Adversarial review](adversarial.md) | a proposer and critics cross-examining a diff, and where the review writes its sessions |

For changing the runtime itself, [CONTRIBUTING.md](../CONTRIBUTING.md) covers
the quality gate and the code layout, and [`specs/`](../specs/README.md) holds
the design record of each capability.
