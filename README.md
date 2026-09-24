# Topos Runtime

[![CI](https://github.com/latere-ai/topos/actions/workflows/ci.yml/badge.svg)](https://github.com/latere-ai/topos/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/latere.ai/x/topos.svg)](https://pkg.go.dev/latere.ai/x/topos)
[![Release](https://img.shields.io/github/v/release/latere-ai/topos)](https://github.com/latere-ai/topos/releases/latest)
[![License](https://img.shields.io/github/license/latere-ai/topos)](LICENSE)

**An embeddable Go runtime for multi-agent systems.** A host application
defines agents, groups them into regions, and runs one region or a graph of
regions in its own process. Each agent works in a sandbox, hands work to a
peer through a tool call that can only narrow its authority, and leaves a
deterministic trace of what ran. A spend cap stops a region at a dollar
figure, and a model can be swapped by name without touching host code.

[Topos](https://topos.latere.ai), the Latere agent platform, is one host built
on this runtime. Any Go program can be another.

**Status:** pre-1.0. The root `topos` package is the supported surface. The
subpackages are public for advanced use and may change between minor
releases; the [changelog](CHANGELOG.md) names every breaking change.

## Install

Topos requires Go 1.27 or later.

```sh
go get latere.ai/x/topos@latest
```

The examples run offline, with a deterministic model and a temporary-directory
sandbox, so the first run needs no keys and no services:

```sh
git clone https://github.com/latere-ai/topos.git
cd topos
go run ./examples/minimal
```

## A first run

A `Runner` holds the model connection and the sandbox backend. A `Region`
names the agent that starts the work and the peers it may hand work to.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"latere.ai/x/topos"
)

func main() {
	r, err := topos.NewRunner(topos.Options{
		SessionID: "first-run",
		Model: topos.ModelOptions{
			Kind:   topos.ModelLux,
			APIKey: os.Getenv("LUX_API_KEY"), // a Lux virtual key
		},
	})
	if err != nil {
		log.Fatal(err)
	}

	res, err := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Dynamic,
		Entry: topos.AgentSpec{
			Name: "lead", Role: "lead",
			Tools: []string{"read", "write", "exec"},
		},
		Peers: []topos.AgentSpec{{
			Name: "reviewer", Role: "review",
			Description: "reviews a diff and reports problems",
			Tools:       []string{"read"},
		}},
	}, "add input validation to parse.go and have it reviewed")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(res.Final)
	for _, n := range res.Trace.Nodes {
		fmt.Println(n.ID, n.Status, n.Grants)
	}
}
```

`ModelLux` reaches `https://lux.latere.ai` unless `BaseURL` names another
[Lux](https://github.com/latere-ai/lux) gateway, such as one running locally.
`ModelFake` needs no network and no key, which is how the examples and the
test suite run.

## Concepts

**Agent.** An `AgentSpec`: a name, a role, a system prompt, the builtin tools
it may use, and the permission scopes it holds.

**Region.** One unit of work with one way of deciding who runs next.
`Pinned` runs the entry agent and then each peer in order, like a fixed
pipeline. `Dynamic` gives the entry agent a directory of its peers and a
`delegate` tool, and the model decides whom to hand work to.

**Topology.** In a dynamic region, `OrchestratorWorker` (the default) lets
only the entry agent delegate. `Mesh` lets any peer delegate again, down to
`Options.MaxHandoffDepth` levels (3 by default).

**Delegation.** A handoff is a tool call. The chosen peer is spawned with the
intersection of its own tools and scopes and its parent's, runs in a sandbox
of its own, and its answer returns into the parent's transcript. Authority
only ever narrows.

**Tool grants.** `AgentSpec.Tools` limits both the tools the model is offered
and the calls the runtime executes. The builtins are `bash`, `read_file`,
`write_file`, `edit_file`, `grep`, and `glob`, and three families name groups
of them: `read` (`read_file`, `grep`, `glob`), `write` (`write_file`,
`edit_file`), and `exec` (`bash`). A nil list grants every builtin, an empty
list grants none, and an unknown name grants nothing.

**Graph.** Several regions composed into one run. An edge from one region to
another passes the first region's final text in as the second's task.

**Trace.** Every run returns a graph of who ran, who delegated to whom, each
agent's status, the tools it was offered, and the sandbox it ran in. Node ids
derive from the session id, so two runs can be compared and a live view can
reconnect to the same ids.

**Budget.** `Options.BudgetUSD` caps what a region spends across all of its
agents. The run stops on the turn that reaches the cap and returns its partial
output with an error.

## What else it covers

| Need | Where |
|---|---|
| a chat or coding session driven turn by turn, resumable from a stored transcript, with streamed tokens and interrupts | [Sessions](docs/sessions.md) |
| several regions wired into one run, and a JSON form of a graph that a person edits and a host stores | [Graphs](docs/graphs.md) |
| Lux, a provider called directly, a custom model, pricing, and the spend cap | [Models and budgets](docs/models.md) |
| a hosted sandbox, credentials that never enter the sandbox as plaintext, a path deny-list, and approval before a command runs | [Sandboxes](docs/sandboxes.md) |
| a proposer and several critics cross-examining a diff | [Adversarial review](docs/adversarial.md) |

[`docs/`](docs/README.md) is the index of these guides. The
[Go package reference](https://pkg.go.dev/latere.ai/x/topos) is the complete
API, and [`examples/`](examples) holds five programs that run offline.

## Packages

| Package | What it is |
|---|---|
| `latere.ai/x/topos` | the supported surface: `Runner`, regions, graphs, turns, the trace, and events |
| `.../graph` | the JSON form of a graph, and the lowering to a runnable one |
| `.../billing` | pricing a turn and enforcing a budget |
| `.../sandbox` | the `Provider` interface, and the `Confine` and `Consent` wrappers |
| `.../sandbox/local` | the default provider: a temporary directory, or a directory the host names |
| `.../sandbox/cella` | a provider backed by hosted Cella sandboxes |
| `.../sandbox/rpc` | a provider served over a byte stream, so a remote machine can act as a sandbox |
| `.../models`, `.../models/lux`, `.../models/fake` | the model interface, the Lux adapter, and the deterministic model |
| `.../harness`, `.../harness/tools`, `.../harness/hooks`, `.../runtime/loop` | the engine: the spawner, the tool registry, the hook bus, and the agentic loop |
| `.../adversarial` and its subpackages | adversarial review of a diff |

The root package imports no sandbox backend beyond its local default; a
hosted one is passed in as a `sandbox.Provider`. Its signatures reach into the
engine packages only where a host hands in or reads back an engine value: a
sandbox provider, a cost source, a custom model, and a turn's transcript and
tool registry.

## Contributing and security

Contributions are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) covers the local
quality gate, the test suite, and how the code is organized.
[SECURITY.md](SECURITY.md) describes private vulnerability reporting; do not
report a vulnerability in a public issue.

## License

[Apache-2.0](LICENSE).
