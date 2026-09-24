# Adversarial review

`latere.ai/x/topos/adversarial` reviews a change by argument rather than by a
single opinion. A proposer, the agent that wrote the change, defends it; one
or more critics attack it. Only the objections that survive the exchange are
reported. It is built on the runtime and kept apart from the core packages,
so a host that does not review diffs never imports it.

## How a review runs

The review runs as several independent forks. In each fork a critic attacks
the diff one aspect at a time, and the proposer concedes or rebuts each
attack. A fork ends when the exchange stops changing, when it has used its
rounds, or when the shared token budget runs out. A ledger records every
attack and its outcome, and the returned `Summary` reports what stayed
unresolved.

```go
import "latere.ai/x/topos/adversarial"

sum, err := adversarial.Review(ctx, adversarial.ReviewOptions{
	StateDir:    stateDir, // required: sessions are written under it
	Cwd:         worktree,
	Forks:       3,
	Proposer:    proposer,  // an adversarial.Proposer
	NewCritic:   newCritic, // an adversarial.CriticFactory, called once per fork
	MaxRounds:   6,
	CostCap:     1_000_000,
	TaskContext: "add user login",
	DiffPatch:   diff,
})
```

| Field | Meaning |
|---|---|
| `StateDir` | the directory the review writes `sessions/<id>/` under. There is no default, and an empty value is an error, so the host always decides where reviews land |
| `Cwd` | the working directory the proposer and critics run in |
| `Forks` | how many independent critic forks run |
| `Proposer`, `NewCritic` | the proposer, and a function that builds the critic for each fork |
| `MaxRounds` | the round limit per fork; one exchange of attack and answer is two rounds. Zero uses `adversarial.DefaultMaxRounds`, which is 6 |
| `CostCap` | a token budget shared by all forks; zero means none |
| `TaskContext` | the task the change was made for, passed verbatim |
| `DiffPatch` | the unified diff under review |

`Review` covers the common case of one call. `adversarial.Engine` runs the
same debate with control over each fork.

## Backends

The proposer and the critics are interfaces, so any agent can play either
role. Two implementations ship with the package:

- `adversarial/claude` drives the `claude` command-line tool.
  `claude.NewProposer` resumes and forks the Claude session that made the
  change, so it works only when the change was made under Claude and its
  session id is known. `claude.NewCritic` runs one stateless `claude -p` call
  per round.
- `adversarial/critic` runs each critic round as an agent on the Topos runtime,
  with the model, sandbox, and trace that brings: `critic.NewCriticFactory`
  takes a `critic.Config` and returns a `CriticFactory`.

`adversarial/input` finds a Claude session's transcript and runs `git diff`
between two revisions of a working tree, the two inputs a review usually
starts from.

The [package reference](https://pkg.go.dev/latere.ai/x/topos/adversarial)
covers every type. The `latere review` command in
[latere-cli](https://github.com/latere-ai/latere-cli) is one host of this
package.
