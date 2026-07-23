---
title: Spend Cap Enforcement
status: complete
track: runtime
depends_on:
  - specs/001-agentic-loop.md
  - specs/002-model-connection.md
  - specs/007-bounded-recursion.md
affects:
  - billing/budget.go
  - runtime/loop/loop.go
  - models/model.go
  - models/lux/adapter.go
  - topos.go
effort: medium
created: 2026-07-23
updated: 2026-07-23
author: changkun
dispatched_task_id: null
---

# Spend Cap Enforcement

## Problem

`Options.BudgetUSD` is documented at `topos.go:218` as a region spend cap. It does
not cap anything. Its only consumer is `ParentContext.Budget` (`topos.go:671`),
read on the child-spawn path (`topos.go:817`, `harness/subagent.go:155`), where it
sub-allocates budget to children but never bounds the parent's own consumption.

`billing.Enforcer` exists (`billing/budget.go:71,78`) with `NewEnforcer` and
`OnUsage`, and has zero non-test callers. `runtime/loop.Config` has no budget field,
and none of the three `loop.Run` call sites (`topos.go:551,705,738`) supply one.

Underneath that wiring gap is a harder blocker: **nothing in the runtime knows what
a turn cost.** `billing.Usage.USD` is written nowhere in the SDK path.
`models.Usage` (`models/model.go:215`) is tokens-only, and so is the wire type it
is built from, `ir.Usage` (`pkg/llmdialect/ir/ir.go:277`). Lux computes
`CostUSDMicro` internally (`lux/internal/proxy/tokens.go:22`) but does not return
it to callers in any header or response field. The USD figure in the `adversarial`
package comes from the Claude CLI's `total_cost_usd`, a separate path that does not
reach the runtime loop.

So a budget wired to the loop today would enforce against a `USD` that is always
zero. That is worse than no enforcement, because it reports a cap that silently
never triggers.

## Decision

Topos enforces the spend cap itself. Lux is not the enforcement point: the platform
grants near-unlimited LLM budget to Topos, so a gateway-side cap would not fire, and
Topos must bound its own spend to be safe to run unattended.

Cost is obtained gateway-first with a local fallback, behind one interface. Lux is
the authority on price when it reports; a pinned rate card covers the rest. A
configured budget that cannot be priced is a configuration error, refused before
any spend occurs.

## Design

### Cost source

```go
// CostSource prices a turn's usage. Implementations are consulted once per turn,
// after usage is known.
type CostSource interface {
	// CostUSD returns the cost of u for model. It returns an error when the
	// model cannot be priced, which callers treat as fatal rather than free.
	CostUSD(model string, u Usage) (float64, error)
}
```

The default implementation prefers a gateway-reported figure and falls back to a
pinned rate card. Hosts override it through `Options`.

`Usage` gains a nullable carrier for the reported figure:

```go
// CostUSDMicro is the gateway-reported cost in millionths of a USD, or nil when
// the gateway reported none. Nil means unknown, never zero.
CostUSDMicro *int64
```

Pointer, not value: zero is a legitimate cost (cached local calls report it), so a
value type cannot distinguish "free" from "unreported".

The rate card is per-model and multi-rate. A flat input/output table prices
agentic runs wrong, because cache reads and writes bill at different multiples of
the input rate (Anthropic reads at 0.1x, writes at 1.25x) and agentic loops are
cache-heavy by construction.

### Enforcement point

`runtime/loop/loop.go:316`, immediately after `result.TotalUsage.Add(turnUsage)`.
One site covers the entry, peer, and pinned loops. On breach the loop stops and
the result carries a terminal budget stop reason, distinguishable from a
steady-state or max-rounds stop.

`loop.Run` takes the enforcer as a parameter rather than a `Config` field. A struct
field zero-values silently, which reproduces the current defect in a new place; a
signature change makes an unenforced budget fail to compile. This touches three
production call sites and roughly fourteen in `runtime/loop/loop_test.go`.

### Fail-closed construction

`topos.New` returns an error when a budget is set and the configured model cannot
be priced by the resolved `CostSource`. Nothing runs and nothing is spent.

Honest limitation: config-time validation covers models declared in `Options` and
`ModelOptions`. A model resolved later at runtime cannot be checked before the run
starts, so the turn-boundary check remains as a backstop and errors on the first
unpriceable turn. Both paths fail closed; only the timing differs.

### Unit of enforcement

The cap is region-wide. One meter is built per region and shared by the entry
agent, every pinned step, and every delegated peer, so `BudgetUSD` bounds the
sum of what a region spends rather than being granted afresh to each agent. A
region of *n* agents cannot spend *n* times the cap.

The meter therefore takes a turn's usage and holds the total itself, rather than
being handed a run's accumulated total. A caller cannot reset the accumulation
by passing only its own run's figure, and several callers add up.

Which agent trips the cap is not part of the contract: whichever folds the
crossing turn first does, and under any other interleaving of agents it would be
a different one. That nondeterminism is inherent to a shared bound and is
acceptable — the guarantee is on the region's total, not on any particular
agent.

A graph is metered per region, not per graph. `BudgetUSD` is defined as a region
cap and a graph is a composition of regions, so `RunGraph` meters each region
against the full budget independently: a two-region graph under a $10 cap may
spend $20. The alternative — one budget for a whole graph — would make the
meaning of the option depend on which entry point a host called.

### Composition with child budgets

`BudgetUSD` keeps its existing sub-allocation role on the spawn path. The two
are separate axes: sub-allocation bounds what authority a parent may *grant* a
spawned child, while a meter bounds what a region may *spend* before the runtime
stops it. A child can hold a grant it never uses because the region's meter
tripped first, and the meter is unaware of how the grant was divided. This spec
does not change `DeriveChildBudget`.

## Legs

Three implementation units. Leg 3 delivers a working cap on its own; legs 1 and 2
make it authoritative rather than estimated.

**Leg 1 — `pkg`: carry cost on the wire.** Add `CostUSDMicro *int64` to `ir.Usage`,
surface it through `luxsdk`. Additive, no signature breaks. Consumers that ignore
it are unaffected.

**Leg 2 — `lux`: report cost.** Emit the already-computed `tokens.CostUSDMicro`
(`internal/proxy/proxy.go:2140-2167`) in the response usage object. Note that Lux
itself falls back to a code-pinned rate card that covers few OpenRouter models and
records `-1` when it cannot price, so this leg does not remove the need for leg 3's
fallback.

**Leg 3 — `topos`: price, enforce, refuse.** `CostSource` interface, default
implementation, `Usage.CostUSDMicro`, enforcement at the loop's usage fold,
`loop.Run` signature change, fail-closed construction.

Leg 3 has no build dependency on legs 1 and 2. Until they land, `CostUSDMicro` is
always nil and the rate card carries every run.

## Acceptance criteria

- A run with `BudgetUSD` set stops when accumulated cost reaches the limit, with a
  budget-specific stop reason, and does not stop before it.
- A region of *n* agents that each stay under the cap alone but exceed it
  together stops. The cap is not multiplied by the number of agents.
- A budget stop is reported as an error matching `billing.ErrBudgetExceeded`
  from `Run`, `RunGraph`, and `Turn`, and the partial result is returned with it.
- The lineage node of a budget-stopped agent is not `done`.
- A `Meter` shared by several goroutines loses no usage (race-detector test).
- A gateway-reported cost is preferred over the rate card when present.
- A budget set against an unpriceable model fails `topos.New` with an error naming
  the model. No turn executes.
- A model resolved after construction that cannot be priced stops the run at the
  first turn rather than running unenforced.
- Cache-heavy usage prices differently from an equivalent token count of plain
  input, i.e. the rate card is not flat.
- `loop.Run` cannot be called without an enforcer argument.
- Zero cost from the gateway is distinguished from unreported cost.

## Test plan

Every test states its pre-fix failure. The defining case is that a budget cap
must be shown *not* firing before the change and firing after, since the current
defect is precisely a cap that never fires.

- `runtime/loop`: budget breach stops the loop. Pre-change, with a budget field
  added but unenforced, the loop runs to `MaxIterations`.
- `runtime/loop`: no breach means no early stop, so the cap is not merely
  always-on.
- root: a pinned region of three agents, each spending $1 under a $2 cap, stops
  after two. Pre-change it ran all three, because each agent was metered against
  the full cap on its own.
- root: the same for a delegated peer, whose spend must count against the entry
  agent's region budget.
- `billing`: one meter, several goroutines, under `-race`. The accumulated total
  must equal every fold, since a lost update under-counts a region's spend.
- root: `Run`, `RunGraph`, and `Turn` return an error matching
  `billing.ErrBudgetExceeded` on a breach. Pre-change they returned nil.
- root: the budget-stopped agent's lineage node is not `done`. Pre-change it was.
- root: an unmetered region and an under-budget region are unaffected on every
  path — every agent runs, the error is nil, and every node is `done`.
- `models`: nil `CostUSDMicro` and zero `CostUSDMicro` take different paths.
- `billing`: rate card prices cache reads and writes at their own multiples.
- root: `topos.New` errors when a budget is set for an unpriceable model.
- root: an unpriceable model resolved at runtime stops the first turn.

## Non-goals

- Changing where Lux gets its own prices, or its `-1` fallback behavior.
- A dynamic pricing feed. The rate card is code-pinned and updated by edit.
- Enforcing wall-time or token ceilings. `billing.Budget` already declares those
  legs; wiring them is separate work and does not block this.
- Removing `BudgetUSD`'s child sub-allocation role.

## Open questions

- Where the pinned rate card lives. `billing` is the natural home, but the
  `models` package owns model identity. Deciding this is part of leg 3.
- Whether a host-supplied `CostSource` should be able to opt out of fail-closed
  behavior for a model it knowingly cannot price.

## Outcome

Leg 3 is implemented. Legs 1 and 2 are separate work in `pkg` and `lux`; until
they land `CostUSDMicro` is always nil on the wire and the rate card carries
every run.

**`billing`** owns pricing and enforcement. `cost.go` holds `CostSource`, the
per-model multi-rate `RateCard`, and `GatewayFirst`, which prefers a reported
cost and falls back to the card. `budget.go` gains `Meter`, which prices a run's
accumulated usage and evaluates the result against the existing `Enforcer`. The
open question of where the rate card lives is settled here: `billing` takes
`models.Usage`, so `billing` imports `models` and not the reverse.

A reported cost has three states, not two. Nil means the gateway said nothing; a
negative value is the gateway's cannot-price sentinel; only a non-negative value
is a cost, and zero is a real one. Both unknown states fall through to the card.

**`models`** carries `Usage.CostUSDMicro` and `StopBudgetExceeded`. `Usage.Add`
is nil-dominant on cost: a total that folds in one unreported turn is itself
unknown, so the caller prices the whole total rather than under-counting it. The
zero `Usage` stays the additive identity, including for cost. `models/fake`
reports a real zero, which keeps the zero-config path priceable under a cap
without needing a rate-card entry for a model that has no rate.

**`runtime/loop`** enforces at the usage fold, and again before each turn's
model call so an already-capped region spends nothing further. `Run` takes the
meter as a parameter; `Result` carries `BudgetBreach`. A turn that cannot be
priced ends the run with an error and a partial result, as does a breach.

**`topos`** resolves the `CostSource` (`Options.CostSource`, defaulting to
`billing.DefaultCostSource`), refuses a budget whose declared model cannot be
priced, and builds one meter per region in `runRegion` that every agent of that
region shares.

The rate card prices claude-fable-5, claude-opus-4-8, claude-opus-4-7,
claude-opus-4-6, claude-sonnet-4-6, and claude-haiku-4-5. Models whose published
price is promotional, access-restricted, or unverified are omitted rather than
estimated, so a budget against them fails closed at construction. Adding a model
is an edit to the table.

The config-time check covers models declared in `Options`; the turn-boundary
check covers everything else, including a host-supplied `Options.Brain`. Both
fail closed; only the timing differs.

### Scope of enforcement

The unit of enforcement is one region. `runRegion` builds one meter and hands it
to the entry agent, every pinned step, and every delegated peer, so `BudgetUSD`
bounds their combined spend. `billing.Meter` accumulates the turns it is given
and prices the total, which is what makes several agents add up rather than each
being measured alone.

`Meter` is safe for concurrent use. One mutex covers the fold, the pricing, the
budget check, and the breach latch, because the state is a multi-field usage
total plus the `Enforcer`'s notification latch and only a single critical
section makes that sequence indivisible; per-word atomics would still let two
agents each observe an under-cap total and both proceed. The lock is taken once
per turn boundary. The dispatcher runs a region's agents one at a time today,
and the cap deliberately does not rest on that.

Which agent trips the cap is not part of the contract, and with a shared meter
it cannot be: whichever folds the crossing turn first reports the breach. The
guarantee is on the region's total.

An agent also asks the meter before its first model call of each turn, so an
agent joining a region whose cap is already reached spends nothing rather than
one further turn.

`RunGraph` meters each region separately against the full budget: `BudgetUSD` is
a region cap and a graph is a composition of regions, so a two-region graph
under a $10 cap may spend $20. `Runner.Turn` likewise meters each turn on its
own; a session's budget is per turn, not per conversation.

`DeriveChildBudget` is unchanged. Sub-allocation bounds what a parent may grant
a child; a meter bounds what a region may spend. A child can hold a grant it
never uses because the region's meter tripped first.

### Surfacing a stop

A budget stop is an error everywhere. `loop.Run` returns
`billing.ErrBudgetExceeded` (matchable with `errors.Is` through the context each
layer wraps around it) together with the partial result, and `Runner.Run`,
`Runner.RunGraph`, and `Runner.Turn` propagate both. A caller that only checks
errors gets a loud signal; a caller that wants the truncated output still has
it, including through `RunGraph`, which returns the merged lineage and the
capped region's `Final`.

The lineage node of a budget-stopped agent is `StatusStopped`, a fourth terminal
state beside `running`, `done`, and `failed`. `done` would claim a completion
that did not happen and `failed` would claim a fault that did not occur: the
runtime enforced a limit on an agent that was working correctly.

A delegated peer that trips the cap cannot end its parent from a tool result, so
it records the stop in its tool result and its lineage node, and the parent —
sharing the same meter — finds the cap breached at the top of its next turn,
before spending again.
