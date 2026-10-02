---
title: "Decision services: a decider seam, decisions recorded with their review probability, answers forwarded"
status: drafted
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 012-permissions-and-approvals.md, 016-runners.md]
affects: [harness/, session/, internal/decisions/, internal/toposcli/]
effort: medium
created: 2026-10-02
updated: 2026-10-02
author: changkun
---

# Decision services

## Overview

[[012-permissions-and-approvals]] decides each tool call from the mode,
the lists and a rule-feature score, `rules/1`, and reserves a learned
source for later. This spec opens that seam. The harness asks a
`Decider` for every validated call; the built-in decider is today's
rules, unchanged. An installation may also name a decision service: a
separate program, reached over HTTP, that predicts how likely the person
a session acts for is to approve a call and suggests a verdict. The
harness stays the decision point. It composes the suggestion with the
ceiling its own layers leave open, draws a random review of a share of
automatic verdicts, applies the verdict, and records on the
`agent.tool_use` the probability, fixed before the call ran, that a
person sees it. It sends every decision and every confirmation to the
service, so the service learns from the same answers the person gives
in `confirm` mode, and so the error rate of any source of decisions,
the rules included, is measurable: an answer to a call seen with
probability `p` counts with weight `1/p`.

The two modes this serves are the ones a person meets: `confirm`, where
every call outside the lists waits for the person and the service only
learns, and `progressive`, where the service's suggestion decides the
calls the rules leave open. A client may present them as manual and
auto; the mode names do not change.

## Current state

`harness/harness.go`'s `turn.plan` calls `Score` and `Policy.Decide`
directly and records the risk, the verdict, the reason and the mode on
the `agent.tool_use`. Nothing records how likely a person was to see a
call, so no answer can be weighted, and nothing outside the session
learns from the confirmations. The verdict vocabulary is
`latere.ai/x/pkg/verdict`.

## Design

### The seam

```go
type Call struct {
	Session     session.Session
	ToolUseID   string
	Name        string
	Props       tools.Properties
	Input       json.RawMessage
	MachineKind string
	Remembered  []string
}

type Decider interface {
	Decide(ctx context.Context, c Call) (session.Risk, Decision, error)
}
```

`Decision` carries the verdict and the reason as before, and the review
probability, the draw and the service's suggestion recorded below. The
rules decider is `Score` and `Policy.Decide`. `Config.Decider` replaces
it; nil keeps it. A decider's error fails the step like any other
internal error, so a decider that wants to fail closed returns a
decision, not an error.

### What `agent.tool_use` records

| Field | Type | Meaning |
|---|---|---|
| `review_probability` | number in [0, 1] | the probability, fixed before the call ran, that a person sees it: 1 for an `ask` and for a `flag` the rules force, the audit rate for an automatic verdict under random review whether or not the draw picked it, 0 otherwise; absent on events written before this spec |
| `draw` | number in [0, 1) | the uniform draw the review used, made once per call; a resumed session reads the recorded verdict and never draws again |
| `suggestion` | object | present when a decision service suggested the verdict: `source`, `verdict` (`allow`, `ask` or `block`), `approve`, `uncertainty`, `thresholds` (`allow_above`, `block_below`), `audit_rate`, `reason` |

The fields are optional additions to schema v1 ([[004-session-log]]);
a reader that does not know them ignores them. The rules decider
reviews nothing at random, so its probabilities are 1 for a shown
verdict and 0 otherwise, which is `verdict.Decide(v, allow, 0, u)`.

### The decision service

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `TOPOS_DECISIONS_URL` | no | unset | the decision service's base URL; unset, the harness decides by its rules alone and sends nothing |
| `TOPOS_DECISIONS_TOKEN` | with the URL | none | the bearer the harness sends it |

The protocol is three JSON routes. Each body is keyed by `org`,
`subject` (the person whose approvals are learned) and `action_id`,
which the harness sets to `<session id>/<tool_use_id>`. A local session
has no organization and sends the initiator's subject as `org`.

| Route | Body | Answer |
|---|---|---|
| `POST /v1/predictions` | the action: the key, `actor` (`kind` `agent`, `id` the agent's id, `session`), `kind` `tool`, `name` the tool, `effect` the tool's (`none`, `read`, `write`, `external`), `input`, `environment` (`kind` `host` or `sandbox`, `credentialed`), `signals` (`[{"source":"rules/1","score":...}]`) | 200 and the prediction: `verdict`, `approve`, `uncertainty`, `thresholds`, `audit_rate`, `source`, `reason` |
| `POST /v1/decisions` | the key, `verdict` applied, `review_probability`, `source` (`topos/confirm` or `topos/progressive`), and `action` when the service gave no prediction | 204 |
| `POST /v1/answers` | the key, `approve`, and `by`, the subject that answered | 204 |

Every request has a two-second timeout. The service is idempotent on
the key: a repeated prediction returns the first one, and a second
decision for a key is refused, which the harness accepts as already
recorded.

### Deciding with the service

In `progressive` mode, for each call:

1. The rules decide as today, which gives the risk and the rules'
   verdict.
2. A call on `always_confirm` asks, and a call the score puts at or
   above `block_at` is blocked. These are the ceiling the service cannot
   lift, and the service is not asked.
3. A read-only call, or one an `always_allow` or remembered pattern
   matches, is allowed without asking.
4. Every other call is open: the harness asks for a prediction and
   applies `verdict.Decide(suggestion, allow, audit_rate, u)`, so an
   automatic allow drawn for review becomes a `flag` and an automatic
   block an `ask`. A prediction that fails or does not answer in time is
   `verdict.OnFailure`, an `ask`, and the decision sent carries the
   action.

Hooks, once dispatched ([[012-permissions-and-approvals]]), join the
ceiling. `progressive` on a host with no operating-system sandbox stays
refused `sandbox_unavailable`, with or without a service.

In `confirm` mode the rules decide alone. With a service configured,
the harness still asks for a prediction, which it records as the
suggestion without applying it, and sends the decision, so the service
learns from every confirmation and its suggestions are measured against
them before they decide anything. In `plan` mode the service is not
asked.

### Answers

When `resume` ([[005-harness-loop]]) settles an asked call that has a
`user.tool_confirmation`, it sends the answer, `allow` as an approval and
`deny` as a denial, with the confirmation's sender as `by`. Every runner
passes through `resume`, so the answer is sent wherever the session
continues. Sending is best effort: a failure leaves the call to run or
be answered as it would without a service, and the service holds the
decision without its answer.

## Not in this spec

The decision service itself, its model and its reports; a review queue
for flagged calls, which a console reads from the service; the hosted
runner's wiring of the service, which needs the session to record its
organization; trigger-started sessions, whose subject is the trigger's
owner; how a client names the modes.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The harness decides every validated call through `Config.Decider`, and the rules decider decides exactly as before | `harness.TestRulesDeciderMatchesPolicy`, the existing permission tests unchanged | built |
| Every `agent.tool_use` records `review_probability`: 1 for an ask and a rule-forced flag, 0 for an allow or a block under the rules | `harness.TestToolUseRecordsReviewProbability` | built |
| In `progressive` mode with a service, an open call takes the service's suggestion through `verdict.Decide`, a drawn allow is a flag with the audit rate as its probability, and the draw and the suggestion are recorded | `internal/decisions.TestProgressiveTakesTheSuggestion` | built |
| The ceiling holds: an always-confirm call asks and a call at or above `block_at` is blocked whatever the service suggests, and neither asks it | `internal/decisions.TestCeilingIsNotAsked` | built |
| A service that fails or times out yields an ask, and the decision sent carries the action | `internal/decisions.TestFailureAsks` | built |
| In `confirm` mode the rules decide, the suggestion is recorded and not applied, and the decision is sent | `internal/decisions.TestConfirmModeTeaches` | built |
| A confirmation is sent as an answer when resume settles the call, and a resumed session never draws again | `harness.TestResumeForwardsAnswers` | built |
| `topos run` reads `TOPOS_DECISIONS_URL` and `TOPOS_DECISIONS_TOKEN`, and the URL without the token is refused | `internal/toposcli.TestDecisionServiceConfiguration` | built |
