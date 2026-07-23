---
title: Spend Cap Enforcement
status: proposed
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

### Composition with child budgets

`BudgetUSD` keeps its existing sub-allocation role on the spawn path. Enforcement
composes: a parent's remaining budget bounds what it can grant a child, and a
child's own enforcement is what stops the child. This spec does not change
`DeriveChildBudget`.

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
