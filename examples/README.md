# Examples

Runnable programs that show how to embed the Topos Runtime. Each is a `main`
package run with `go run ./examples/<name>`.

The defaults need no API keys and no external services: they use `ModelFake` (a
deterministic, network-free model) and the local temp-directory sandbox.

| Example | Shows | Runs offline |
|---|---|---|
| [`minimal`](minimal) | the smallest loop: build a Runner, run one agent, read the result and lineage | yes |
| [`delegation`](delegation) | a dynamic region where the entry agent delegates to a peer, plus the lineage graph, driven by a scripted model via `Options.Brain` | yes |
| [`graph`](graph) | composing several regions into one run with `RunGraph`: a dynamic region feeding a pinned chain, wired by a data-flow edge | yes |
| [`sandbox`](sandbox) | selecting the execution backend (local by default, hosted Cella via `TOPOS_CELLA_URL`) | yes (local) |

```sh
go run ./examples/minimal
go run ./examples/delegation
go run ./examples/graph
go run ./examples/sandbox
```

For API-level snippets that appear on
[pkg.go.dev](https://pkg.go.dev/latere.ai/x/topos) and run under `go test`, see
the `Example` functions in `example_test.go` at the repository root.
