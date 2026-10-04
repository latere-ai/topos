---
title: "Changing a session's approval mode: the policy member of PATCH, the session.policy_changed event, the authorizer's question, and a mode that holds from the next step"
status: complete
track: core
depends_on: [004-session-log.md, 005-harness-loop.md, 006-identity.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 015-api.md, 017-external-runners-handoff-fork.md]
affects: [session/, harness/, internal/server/, api/]
effort: small
created: 2026-10-05
updated: 2026-10-05
author: changkun
---

# Changing a session's approval mode

## Overview

A session decides each tool call under an approval mode, `plan`,
`confirm` or `progressive` ([[012-permissions-and-approvals]]), and it
runs the mode its agent names for its whole life. A person who started
in `confirm` and trusts the work by its third turn has no way to let it
run, and one who sees it heading somewhere they did not mean has no way
to make it only look. This spec adds the mode to `PATCH
/v1/sessions/{id}`, beside the model: one event records the change, the
authorizer decides it, and the harness applies it from the next step.

## Current state

The mode is fixed at create. The server merges the agent's
`spec.approvals` with the authorizer's `limits` and records the merged
policy as the Session's `policy`, which every runner applies
([[012-permissions-and-approvals]]). The merge takes the agent's mode,
`confirm` when it names none: the authorizer's limits carry lists and
thresholds and no mode (`harness.Policy.Merge`, `authorizer.Limits`).
`PATCH /v1/sessions/{id}` takes the model and its effort alone, and any
other member is `invalid_request` ([[015-api]]).

### Is there a bound in the agent's document

No. `spec.approvals.mode` names the mode the agent's sessions start in;
the manifest has no field that names the modes a session may run in or
a mode it may not go past, and spec 012's sentence "the mode the
stricter" has no second mode to compare with, since the authorizer's
limits name none. What an agent's document does bound, and keeps
bounding across a change of the mode:

| Bound | Holds after a change |
|---|---|
| `always_confirm`, the union of the agent's and the authorizer's lists | yes: a match asks in `confirm` and `progressive`, and blocks in `plan` |
| `always_allow`, the intersection | yes |
| the thresholds of `progressive`, the lower of the two | yes |
| a subagent's own `spec.approvals.mode` ([[013-threads-and-subagents]]) | yes: a thread runs the stricter of the session's mode and the modes of the agents above it |
| the hard boundaries: the machine's sandbox, the agent's permissions, the session scope, the git host's ref rules | yes: no mode crosses them |

So a person may switch to any of the three modes, and whether they may
is the authorizer's decision, which this spec gives what it needs to
make. An optional ceiling in the manifest, a mode an agent's sessions
may not go past, is a manifest change of its own and is not in this
spec.

## Design

### The body

`PATCH /v1/sessions/{id}` takes `policy`, the Session's own member,
with `mode` as its one field, alone or beside `model`:

```json
{"policy": {"mode": "progressive"}}
{"policy": {"mode": "plan"}, "model": {"effort": "high"}}
```

`mode` is `plan`, `confirm` or `progressive`; any other value, a
`policy` without `mode` and any other member of `policy` are
`invalid_request`. A body that names neither `model` nor `policy` is
`invalid_request` as before. The other members of the Session's
`policy`, the lists and the thresholds, are not a person's to change:
they are the authorizer's and the agent's.

### The order

1. `session.read`: a caller who may not read the session hears
   `not_found`.
2. An ended session is `conflict`.
3. `session.update` is asked once, with every field the body changes
   (below). A deny is `forbidden` with the authorizer's reason, and
   nothing changes.
4. A model the body names is resolved as before ([[015-api]]).
5. The events of what changed are appended in one batch, so a change of
   the model and the mode lands together or not at all, and the answer
   is the Session with its `policy`.

### The question

`session.update` carries, beside the fields every session action
carries and the model's fields when the body changes the model:

| Field | Value |
|---|---|
| `approval_mode` | the mode the body names |
| `current_approval_mode` | the mode the session runs now |
| `agent_approval_mode` | the mode the session's agent names, the one the session started in, `confirm` for an agent that names none |

```json
{"action": "session.update", "resource": {"kind": "session", "id": "ses_01J...",
  "fields": {"agent": "agent_01J...", "owner": "https://login.example|alice", "runner": "hosted",
    "session_id": "ses_01J...", "approval_mode": "progressive",
    "current_approval_mode": "confirm", "agent_approval_mode": "confirm"}}}
```

`agent_approval_mode` lets an authorizer tell a change past the agent's
own mode from a return to it, without reading the agent. A change to
the mode the session already runs is asked like any other, since the
question is the caller's right to set the mode, and appends nothing.

### The event

An allowed change appends `session.policy_changed`, written by the
server and not in the model's context:

```json
{"type": "session.policy_changed", "payload": {"by": {"subject": "https://login.example|alice", "kind": "person"},
  "old": {"mode": "confirm"}, "new": {"mode": "progressive"}}}
```

`by` is the person who sent the `PATCH`. The header's `policy.mode`
takes the latest `new`, so a session read after the change says the
mode it runs; the rest of `policy` is unchanged. The event is not
redactable: it is a record of what was decided, as `session.model_changed`
is ([[004-session-log]]).

### From the next step

The harness decides a step's calls after the model answers, under the
mode of the latest `session.policy_changed` in the log it has read, and
under the Session's `policy.mode` when there is none. The log it has
read is the one its last append returned, so a change appended while a
step runs is read at that step's own append and holds from the next
step's calls, in the same turn; a turn that starts after it reads it
from the header. A call decided before the change keeps its verdict:
the verdict is in its `agent.tool_use` and no runner decides a call
twice. Each `agent.tool_use` records the mode it was decided under.

A thread decides under the stricter of the session's mode and the
modes of the agents above it in the thread's graph, so a subagent whose
agent names `confirm` stays in `confirm` when the session moves to
`progressive`, and moves to `plan` with it.

A host that records `sandbox: none` ([[009-machines]]) decides a
session switched to `progressive` as `confirm`, and records `confirm`,
since `progressive` runs what the score rates low without asking, which
spec 012 allows only where an operating-system sandbox holds; `topos
run` already refuses `progressive` at its start on such a host.

### A call already waiting

A call waiting for a confirmation when the mode changes keeps waiting,
whichever way the mode moved. The person was asked, and the answer is
theirs: a more permissive mode does not allow a call the person has
before them, and a stricter one does not deny it. They answer it with
`user.tool_confirmation`, or deny it with a message, as before. The
next step's calls are decided under the new mode. A client that shows
the mode beside a waiting call says that the call still waits.

### Fork, rewind and handoff

A fork starts in its own create's mode, the agent's, and reads the
copied `session.policy_changed` events as its parent's history, as it
reads a copied `session.resumed`: the mode is how much a person watches
the work, and a fork is a session of its own whose forker may not be
the person who changed the parent's ([[017-external-runners-handoff-fork]]).
A rewind keeps the mode, as it keeps the model. A handoff moves the
session's writer and keeps its header, mode included.

### Compatibility and rollout

An authorizer that does not know `approval_mode` sees a question whose
fields it does not read. One that requires a model or an effort on
`session.update` refuses a change of the mode alone, which is the safe
direction; one that reads the model alone would allow a change of the
mode beside a model on the model's rules. A core with this spec
therefore rolls after the installation's authorizer answers the field,
and the authorizer answers old and new cores alike: a question without
`approval_mode` is a model or an effort change as before.

A client that does not send `policy` is unaffected. A client that reads
a session's log meets one new event type, which a reader that keeps
unknown types as spec 004 asks shows as nothing.

### API changes

| Route | Change |
|---|---|
| `PATCH /v1/sessions/{id}` | takes `policy.mode` beside `model`; asks `session.update` with `approval_mode`, `current_approval_mode` and `agent_approval_mode`; appends `session.policy_changed` |

## Not in this spec

A ceiling on the modes in the manifest; a change of the lists or the
thresholds by a person; a mode set at a session's create, which takes
the agent's; the console or a client's control for the mode.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| A change of the mode asks `session.update` with `approval_mode`, `current_approval_mode` and `agent_approval_mode`, appends `session.policy_changed` with the old and the new mode, and answers the Session whose `policy.mode` is the new one, its lists and thresholds unchanged; a change to the mode the session runs appends nothing | `internal/server.TestASessionChangesItsMode` | built |
| A change of the mode and the model in one body asks one question with both fields and appends both events in one batch | `internal/server.TestAModeAndAModelChangeTogether` | built |
| A mode outside the three, a `policy` without `mode` or with another member, and a body naming neither member are `invalid_request`; a caller who may not read hears `not_found`; an ended session is `conflict`; a denied change is `forbidden` with the reason; each leaves the mode and the log as they were | `internal/server.TestAModeChangeIsRefused` | built |
| The header's `policy.mode` follows the latest `session.policy_changed`, and a fork's copied change does not move the fork's | `session.TestThePolicyFollowsItsChanges` | built |
| A change appended while a step runs holds from the next step's calls in the same turn, and a call decided before it keeps its verdict | `harness.TestAModeChangeHoldsFromTheNextStep` | built |
| A call waiting for a confirmation keeps waiting after a change to `progressive`, and a confirmation still runs it | `harness.TestAWaitingCallStaysWaitingAfterAModeChange` | built |
| A thread decides under the stricter of the session's switched mode and its own agent's | `harness.TestAThreadKeepsItsAgentsStricterMode` | built |
| A host with no sandbox decides a session switched to `progressive` as `confirm` | `harness.TestProgressiveWithoutASandboxDecidesAsConfirm` | built |
| Through toposd: a session switched to `plan` blocks the write its next turn's model asks for, over the stub model | `cmd/toposd.TestAModeChangeReachesTheRunner` | built |

## Outcome

Built as designed on 2026-10-05. `PATCH /v1/sessions/{id}` takes
`policy.mode`, alone or beside `model`; one `session.update` question
carries `approval_mode`, `current_approval_mode` and
`agent_approval_mode` beside the model's fields; the events of one body
land in one batch. `session.Mode` reads the latest change a session
wrote itself from a log, which the harness reads at each step's
decisions and `ApplyBatch` writes to the header's `policy.mode`. A
thread holds a ceiling, the stricter of the modes its own agents name,
which the session's change cannot pass, and a host that records
`sandbox: none` decides `progressive` as `confirm`. Every criterion has
a passing test, the end-to-end one through `toposd` over the stub model
among them.

Two points the design left open were settled. A change to the mode the
session already runs is asked like any other change and appends
nothing, as a change of the model to the model it runs does. A session
that records no policy, which only a session created outside the server
has, keeps none in its header after a change; the harness applies the
change over the agent's own policy from the log, so the header does not
invent lists it never had.
