---
title: "Hosted sessions decided by a decision service: toposd and the runner role ask the installation's decision service, a server with an identity provider with its own token for the service's audience, and a server without one or a runner with a fixed bearer"
status: complete
track: core
depends_on: [002-scaffold-and-configuration.md, 016-runners.md, 018-credentials-and-secrets.md, 037-decision-services.md]
affects: [internal/decisions/, internal/config/, internal/hosted/, cmd/toposd/, docs/]
effort: small
created: 2026-11-01
updated: 2026-11-01
author: changkun
---

# Hosted sessions decided by a decision service

## Overview

[[037-decision-services]] opened the decider seam and gave it to
`topos run`: a decision service suggests a verdict for each call, the
harness composes it with its rules, and every decision and confirmation
is sent to the service. Hosted sessions kept deciding by the rules
alone. This spec gives the same decider to every hosted session, in
`toposd serve` and in `toposd runner`, so that the service learns from
the confirmations of hosted sessions too, and decides in `progressive`
mode where the rules leave a call open.

## Current state

`internal/toposcli` builds a `decisions.Decider` from
`TOPOS_DECISIONS_URL` and `TOPOS_DECISIONS_TOKEN`. `internal/hosted`
builds each session's `harness.Config` without a decider, and
`internal/config` reads neither variable. The decider sends one fixed
bearer.

## Design

- **One decider for each process.** `toposd` builds one
  `decisions.Decider` when `TOPOS_DECISIONS_URL` is set and gives it to
  `hosted.Options.Decider`; the hosted builder sets it on every
  session's `harness.Config`. The decider keys each call by the session
  and its initiator, so one decider serves every session. Without the
  URL, sessions decide by the rules, as before.
- **The token.** `decisions.Options` gains `Tokens`, a source asked for
  the bearer at each request, beside the fixed `Token`; exactly one is
  set. A server with an identity provider (`TOPOS_IDENTITY_URL`) asks
  the service with its own token for `TOPOS_DECISIONS_AUDIENCE`, the
  client credentials grant of its host client
  ([[018-credentials-and-secrets]]), which `identity.Client.ServiceToken`
  holds and renews. Every other process, a server without an identity
  provider and the runner role, which holds none, sends the fixed
  `TOPOS_DECISIONS_TOKEN`. A token source that fails is a failure to
  reach the service, so the call waits for a person.
- **Configuration.** `TOPOS_DECISIONS_URL` must be an absolute `http` or
  `https` URL. A server with an identity provider needs
  `TOPOS_DECISIONS_AUDIENCE` and refuses `TOPOS_DECISIONS_TOKEN` beside
  it, since the fixed bearer would be a second credential beside the
  one it replaces; a server without one, and a runner, need
  `TOPOS_DECISIONS_TOKEN`. The audience or the token without the URL is
  refused. The audience has no default: the installation names its
  decision service.
- **Failures are logged.** A failure the decider recovers from, a
  prediction that failed or a record not sent, is logged at warning
  level as `decision service`.
- **The organization.** A session records no organization, so the
  decider keys each call with the initiator's subject as its
  organization, as [[037-decision-services]] does for `topos run`.

## Not in this spec

A session's organization; a review queue for flagged calls; the
decision service's authorization of the server as the decision point of
its sessions, which is the installation's; how a client presents the
modes.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A token source is asked at each request, a fixed token and a source together are refused, and a failing source makes the call wait | `internal/decisions.TestTokenSource`, `internal/decisions.TestNewRefuses` | built |
| `toposd` reads `TOPOS_DECISIONS_URL`, `TOPOS_DECISIONS_AUDIENCE` and `TOPOS_DECISIONS_TOKEN` and refuses each combination the design refuses | `internal/config.TestDecisions` | built |
| A server with an identity provider asks the service for a hosted session's call with its own token for the audience, keyed by the session and its initiator | `cmd/toposd.TestServeAsksTheDecisionServiceWithItsOwnToken` | built |
| A server without an identity provider sends the fixed bearer | `cmd/toposd.TestServeAsksTheDecisionServiceWithItsBearer` | built |
| In `progressive` mode, a hosted call the rules leave open waits for the person when the service fails, and the failure is logged | `cmd/toposd.TestHostedCallWaitsWhenTheDecisionServiceFails` | built |
| No URL decides by the rules, and a URL the client refuses stops the start | `cmd/toposd.TestDeciderFromTheConfiguration` | built |

## Outcome

Built as designed, with every criterion's test. The runner role reads
`TOPOS_DECISIONS_URL` and `TOPOS_DECISIONS_TOKEN` and not the audience,
because it holds no identity provider; an installation whose runners
run in the serve role, as the hosted one does, needs only the audience.
