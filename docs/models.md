# Models and budgets

`Options.Model` says how every agent of a run reaches a model. Changing the
model behind a run changes a model id, not host code.

## Choosing a connection

`ModelOptions.Kind` picks one of three connections:

| Kind | Reaches | Credential |
|---|---|---|
| `ModelLux` | any model a [Lux](https://github.com/latere-ai/lux) gateway routes. `BaseURL` is the gateway root and defaults to `https://lux.latere.ai`; a local `luxd` running with its own provider keys works the same way | a Lux virtual key in `APIKey`, or a token from `BearerSource` |
| `ModelDirect` | one provider endpoint, with no gateway in between. `Provider` names it: `anthropic` (the default), `openai`, `gemini`, `openrouter`, `ollama`, `moonshot`, `xai`, or `zhipu`. `BaseURL` defaults to the provider's public endpoint | the provider's own key in `APIKey`, or a token from `BearerSource` |
| `ModelFake` | a deterministic model with no network, for tests and examples. It is also what an empty `Kind` selects | none |

`Model` is the model id, such as `claude-sonnet-4-6`. With `ModelLux` and no
`Model`, the gateway is asked for `claude-opus-4-8`.

Give one credential, not both. `APIKey` is fixed for the life of the runner.
`BearerSource` is a function called before each request, for a token that
expires and is renewed, such as one minted per session. For `ModelDirect`
with Anthropic, `APIKey` travels as `x-api-key` and a `BearerSource` token as
`Authorization: Bearer`.

`ModelDirect` sends the same request format as the gateway and translates it
to the provider's API inside the process, with the translation the gateway
itself uses. A direct connection keeps the provider key in the host process;
with `ModelLux`, provider keys stay in the gateway and the host holds only a
virtual key it can revoke.

## A model of the host's own

`ModelOptions.Client` takes any value that implements `models.Model`: an
adapter for a provider the built-in kinds do not cover, a connection with an
authentication scheme they do not offer, or a scripted model that makes a run
reproducible in a test. When `Client` is set, nothing is built: `Kind`,
`Provider`, `BaseURL`, `APIKey`, and `BearerSource` are not read.

`Model` is still read, as the model id used for pricing. Set it next to
`Client` when a budget is configured, so that `NewRunner` can check the price
before anything runs; see below.

[`examples/delegation`](../examples/delegation) drives a run with a scripted
`Client`.

## The spend cap

`Options.BudgetUSD` is the most a region may spend, in US dollars. Every agent
in the region, the entry agent, each pinned step, and every delegated peer,
draws from the same budget, so the cap bounds the region's total rather than
each agent's share. Zero means no cap.

After each turn the runtime prices the turn's token usage and adds it to the
region's total. The turn that reaches the cap is the last one. The run then
returns what it has, the trace of what ran and the last agent's output,
together with an error that matches `billing.ErrBudgetExceeded`:

```go
res, err := r.Run(ctx, region, task)
switch {
case errors.Is(err, billing.ErrBudgetExceeded):
	// res.Final is partial; the stopped agent's node reads StatusStopped.
case err != nil:
	return err
}
```

The stopped agent's trace node has `StatusStopped`, which is neither `done` nor
`failed`: the agent did not finish, and nothing went wrong. Which agent trips
the cap is not guaranteed, only that the region's total stops at the turn
that reaches it.

A graph meters each region against the full cap on its own, and a session
meters each `Turn` on its own. [Graphs](graphs.md) and
[Sessions](sessions.md) describe what each returns when the cap is reached.

When the run goes through Lux, the gateway can enforce a second ceiling of its
own on the virtual key. The two are independent.

## How a turn is priced

`Options.CostSource` prices a turn. Left nil, the runner uses
`billing.DefaultCostSource()`: the cost the gateway reports for the call when
it reports one, and otherwise a rate card pinned in the `billing` package for
the Claude models it lists. A host with its own prices, a negotiated rate or a
model the card does not cover, supplies a `billing.CostSource`, or a
`billing.RateCard` of its own, or a `billing.GatewayFirst` whose `Fallback` is
its own card. `billing.DefaultRateCard()` returns a fresh copy of the pinned
card each time, so a host can add entries to it without changing the
runtime's.

A budget that cannot be priced is refused rather than run unenforced.
`NewRunner` returns an error when a budget is set and `Model` names a model
the cost source cannot price. Pricing reads the id in `Model`, not the model a
connection falls back to, so with `Model` empty the check happens at the end
of the first turn instead: a turn the gateway did not price and the card
cannot price stops the run there. Either way it fails closed. Set `Model`
whenever `BudgetUSD` is set, so the check happens before anything is spent.
