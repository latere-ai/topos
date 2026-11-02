---
title: "What a person consented to: each call sent with its reach, a confirmation that says how the person answered, a session that asks for every step after repeated refusals, and both ends of a subagent's handoff"
status: testing
track: core
depends_on: [004-session-log.md, 008-tools.md, 012-permissions-and-approvals.md, 013-threads-and-subagents.md, 015-api.md, 019-git.md, 034-checkpoints-and-rewind.md, 037-decision-services.md, 043-publishing-a-folder.md, 052-a-sessions-network.md, 087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report.md, 099-hosted-sessions-decided-by-a-decision-service.md]
affects: [session/, harness/, internal/decisions/, internal/server/, internal/config/, internal/hosted/, internal/toposcli/, authorizer/, prompts/, api/openapi.yaml, docs/]
effort: large
created: 2026-11-02
updated: 2026-11-02
author: changkun
---

# What a person consented to

## Overview

A decision service learns which calls a person approves
([[037-decision-services]]). Its core inference is "this kind of call
was allowed before, so it will be allowed now". Two published findings
show where that inference breaks.

- **An approval does not cover a wider call.** Anthropic's study of its
  Claude Code auto mode classifier
  ([Claude Code auto mode](https://www.anthropic.com/research/claude-code-auto-mode),
  2026-03-25) found that most of its misses on
  real overeager actions were not a failure to see the danger: "What it
  gets wrong is whether a real consent signal in the session actually
  covers this action." A person who said "clean up the PR" did not say
  "force push", and one approval of a similar action is not a pattern.
  What a call reaches (can it be undone, does it leave the working
  directory, the session's network or the organization, does it make
  something public, does it touch the default branch or production) is
  what an approval has to cover, and a decision service can only weigh
  it if the decision point says it.
- **Most answers are habit.** Claude Code users approve 97% of
  permission prompts
  ([Auto mode is now the default in Claude Code](https://claude.com/blog/auto-mode-default-in-claude-code),
  2026-08-07), and in a controlled study testers stopped about
  17% of dangerous commands early in a session and about 5% after 50 or
  more prompts. A service that learns from every click learns the
  habit. It can only tell a considered answer from a click if the
  decision point says how the person answered: how long the question
  was in front of them, whether they opened the call's details, where
  they answered, and whether the client showed the service's suggestion
  first. In a study of how agents shape experts' judgment calls
  ([Anthropic, 2026-10-27](https://www.anthropic.com/research/agents-judgment-calls)),
  analysts went with the option their agent leaned toward 86% of the
  time, though a coin flip had set the lean.

The same study of the auto mode classifier also showed two practices
this spec adopts: after 3 refusals in a row or 20 in a session, the
agent stops and the person is asked; and the check runs at both ends of
a handoff to a subagent, when work is delegated and when its result
comes back.

This spec makes Topos say what it knows:

1. **Each call is sent with its reach, and records it**: the class and
   kind of its target, the boundaries it crosses, whether it can be
   undone, whether it is a first contact, and the limits the session's
   machine enforces, all as words from a closed list and never as a
   path, a host or a branch name. The call's `agent.tool_use` records
   the reach it was decided on, before it waits, so a client can tell
   the person what the call reaches before they answer.
2. **A confirmation says how the person answered**: four optional
   fields a client sets on `user.tool_confirmation`, recorded on the
   event and passed to the decision service with the time the call
   waited, which the server measures from the log.
3. **The service's two new kinds of ask are honored**: an ask because
   the call reaches wider than anything the person allowed is kept for
   the person in `learned` mode, and a learned allow loosens the risk
   score only where the service says the person's approvals cover it.
4. **Repeated refusals hand the session back to the person**: after 3
   refusals in a row or 20 in a session, an attended session asks for
   every call that changes something until the person writes again, and
   an unattended turn stops.
5. **Both ends of a subagent's handoff**: the counts and the fallback
   are the session's, shared by every subagent, each call says how deep
   it was delegated, and a subagent whose calls were refused returns
   its result with a fixed warning to its caller.

The decision service's side of this contract (how it reads the reach,
weighs an answer and caps an allow) is the service's own design. This
spec fixes only what Topos sends and how it reads the answer.

## Current state

| Piece | Where | Today |
|---|---|---|
| The action sent | `decisions.(*Decider).action` in `internal/decisions/decisions.go` | `actor` (`agent`, the agent's id, the session), `kind` `tool`, `name`, `effect` (the tool's, `external` when unset), `input`, `environment` (`host` with `credentialed` true, or `sandbox` on a Cella machine), `attributes.attended` ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]), and `signals` with the `rules/1` score. No `target`, no `reversible`, nothing about the session's network, the organization, a publish's access or the repositories |
| The answer sent | `Decider.Answered`, through `harness.Learner` from `turn.forwardAnswer` in `harness/resume.go` | `approve` and `by` alone |
| What a call records | `session.AgentToolUse` | the risk, the verdict, the reason, the mode, `review_probability`, `draw`, the decision service's `suggestion`, `attended`, `decided_by`, `decided_by_rule` and `decision_org`; nothing about what the call reaches, so a client that asks the person can show the command and nothing about its reach |
| A confirmation | `session.UserToolConfirmation` | `sender`, `tool_use_id` or `approval_id`, `decision`, `note`, `remember`; decoded strictly by `POST /v1/sessions/{id}/events`, so a field the server does not know is `invalid_request` |
| Learned mode | `prediction.hasEvidence` | the service's `allow`, `block` and `split` ask decide; any other ask, or a kind the harness does not know, is left to the rules and the risk score |
| Refusals in a row | nowhere | the harness repeats-call rule ([[015-api]]) ends a turn after 3 identical failing calls, but nothing counts refused calls |
| A subagent's calls | `turn.plan` in `harness/harness.go` | every call of every thread goes through the same `Decider` with the thread's narrowed policy ([[013-threads-and-subagents]]), and is sent to the service like the session's own |
| The handoff | `agentTool` in `harness/threads.go` | `agent` and `send_message` have effect `none`, so they are decided `read_only` and sent as allowed reads; the result is the subagent's final text, with nothing about what it was refused |

## Design

### The reach of a call

Topos adds three optional members to the action it sends, filled only
from what the harness knows without reading the call's input as text.
Every value is a word of the closed lists below; Topos never puts a
path, a host, a ref name, a URL or any other string from a call's input
or from the session's configuration into them.

| Member | Values | Meaning |
|---|---|---|
| `scope.undo` | `none`, `reversible`, `recoverable`, `irreversible` | whether the call can be undone: changes nothing; can be undone in the working directory with no loss; can be undone from a copy or by another call; cannot be undone, or others already saw it |
| `scope.crosses` | an object with any of `machine`, `remote`, `network`, `organization`, `public`, each `true` or `false` | the boundaries the call crosses, as far as Topos knows: the machine outside the working directory, another system, a host outside the session's network, a party the organization does not own, anyone at all. A boundary Topos does not know is left out, not sent as `false` |
| `scope.contact` | `first`, `repeat` | whether the call is the session's first contact with its target |
| `target` | `{kind, id, class}`, `kind` a word such as `path`, `app` or `host`, `class` one of `scratch`, `own`, `shared`, `primary` | what the call changes: `own` is something this session made (its branch, its preview, its checkout), `shared` something other people or the person's own files use, `primary` the default or a protected branch, production, a live release. `id` is sent only where it is an opaque id, never a path or a host |
| `environment.holds` | a list of `egress`, `protected_refs`, `checkpoints` | the limits the machine enforces whatever the call does: no host outside the session's network is reached without its own approval; the git host refuses a push to a protected branch; the working directory is kept in checkpoints it can be restored from |
| `environment.repositories` | `private`, `public`, `mixed` | the visibility, at the git host, of the repositories the session was given, when it has any; a push to them is that visible |

A decision service that knows none of these members ignores them; one
that does reads the call's input with its own fixed tables (a `git push`
with `--force` cannot be undone, a refspec that names `main` is
`primary`) and takes the wider of its reading and what Topos says.

**What Topos sends, by call.**

| Call | `scope` | `target` |
|---|---|---|
| a call whose tool reads or changes nothing (`read`, `glob`, `grep`, `history`, `todo`, a question) | `undo: none` | none |
| `write`, `edit` | `crosses.machine` true when the resolved path is outside the working directory, false otherwise; `crosses.remote` false; `undo: reversible` when the path is inside the working directory and the machine holds `checkpoints` | `kind: path`, no `id`; `class: own` on a Cella machine, whose working directory is the session's own checkout, and `shared` on a host or a person's own computer ([[080-device-machines]]), whose files are the person's |
| `bash` | nothing: the call's reach is in its command, which only the service's own tables read | none |
| `web_fetch` | `undo: none`; `crosses.remote` true; `crosses.network` false for a host inside the session's network ([[052-a-sessions-network]]) and true outside it; `crosses.organization` false for a host the boundary names and true for one it does not, left out without a boundary; `crosses.public` false; `contact: first` when no earlier `web_fetch` of the session reached the host, else `repeat` | `kind: host`, no `id`, no `class` |
| `publish` ([[043-publishing-a-folder]]) | `crosses.remote` true; `crosses.public` true when the call sets `access: link` or releases an app that anyone with its address opens, false when the app is private and stays so; `undo: irreversible` for `access: link`, `recoverable` for a release or a preview; `contact: first` when the call creates the app, else `repeat` | `kind: app`, `id` the app's id when it exists; `class: primary` for a release, `own` for a preview |
| `agent`, and `send_message` to a subagent | `undo: none` | none; see [Both ends of a handoff](#both-ends-of-a-handoff) |
| every other call (MCP tools, `request_connection`, `trigger`, `wake`, `notify`, a client's tool) | nothing | none |

`environment.holds` on every call:

| Hold | When Topos sends it |
|---|---|
| `egress` | the machine is a Cella sandbox whose session network mode is `allowlist` or `none` ([[052-a-sessions-network]]); never on a host or a person's own computer |
| `protected_refs` | the session's repositories are reached through the git host with the ref rules of [[019-git]], which refuse a push to a protected branch |
| `checkpoints` | the machine keeps each turn's working directory in a checkpoint ([[034-checkpoints-and-rewind]]) |

`environment.repositories` is the visibility the git host reported for
the session's delivered repositories when the session's machine opened:
`private` when every one is private, `public` when every one is public,
`mixed` otherwise, and left out for a session with none.

**The organization's boundary.** Whether a host belongs to the
organization is the installation's to say. The authorizer's allow of
`session.create`, `session.fork` and `session.send` may carry
`boundary`, `{"hosts": [...]}`, at most 256 entries, each a host name
or `*.` and a domain, lowercased: the hosts the organization owns. The
session keeps the latest allow's boundary, as it keeps its network.
Without one, Topos leaves `crosses.organization` out, and the service
reads it as unknown. This is the "environment" of the auto mode
classifier (where trust ends), in the place this core already gives an
installation's decisions ([[052-a-sessions-network]]); its "block
rules" and "allow exceptions" are this core's `always_confirm` and
`always_allow` ([[012-permissions-and-approvals]]).

**Depth.** Every call carries `attributes.depth`, the subagent depth of
the thread that made it: 0 for the session's own conversation, 1 for a
subagent it started, and so on ([[013-threads-and-subagents]]).

### What the call records

`agent.tool_use` gains `scope`, the reach the call was decided on,
appended with the call's decision and so before the call waits for a
person. A client words it for the person beside the command ("changes
the default branch", "cannot be undone", "makes the app public"); the
command or input itself is what names the target.

| Member | Values |
|---|---|
| `undo` | `none`, `reversible`, `recoverable`, `irreversible` |
| `crosses` | an object with any of `machine`, `remote`, `network`, `organization`, `public`, each `true` or `false` |
| `target_class` | `scratch`, `own`, `shared`, `primary` |
| `target_kind` | the kind of the target, a word such as `path`, `ref`, `app`, `host`, `repository`, `package` |
| `contact` | `first`, `repeat` |
| `source` | `service` when the decision service's reading is in it, `topos` when it is Topos's own claims alone |

Every member but `source` is optional, and a member left out is not
known; no member holds a name, so `main`, an app's slug or a host never
appears in it.

- **With a decision service that answered.** Its prediction carries the
  scope it read, `scope`, which already takes the wider of its reading
  of the input and what Topos sent. Each member is the service's value,
  or Topos's own claim where the service gives none or a narrower one,
  in the order of the members table above (`false` before `true`).
  `source` is `service`. For a `bash` call, only the service reads the
  command, so this is the only way a `bash` call records its reach.
- **Without one.** In plan mode, without a decision service, when the
  prediction failed or came too late, or when the service's prediction
  carries no `scope`, the call records Topos's own claims from the
  table above and `source` is `topos`. A `bash` call then records no
  reach but `source`, and a client says that the reach of the command
  is not known rather than showing none as safe.

`scope` is an optional addition to schema v1 ([[004-session-log]]); a
reader that does not know it ignores it, and an event written before it
has none.

### How the person answered

**What a client sets.** `user.tool_confirmation` gains four optional
fields. A client sets each one it can measure and leaves out each one it
cannot; it never guesses.

| Field | Type | Set to |
|---|---|---|
| `answer_ms` | integer, 0 to 86400000 | the milliseconds from the moment the question became visible to the person on this client (rendered on screen, in a window that has focus) to the person's answer gesture; when the person left and came back, the time the question was visible, summed |
| `details_opened` | boolean | `true` when the person opened the call's details (the full command or input, the diff, the target) before answering; `false` when the client offered details and the person did not open them; left out when the client offers no details |
| `suggestion_shown` | boolean | `true` when the client showed any part of the decision service's suggestion (its verdict, the approval probability, its reason, or an explanation) before the person answered; `false` when it showed none |
| `surface` | `prompt`, `notification`, `queue`, `bulk` | where the person answered: the conversation, with the call waiting in it; a system notification, without opening the session; a list of waiting questions or reviews; one gesture that answered more than one call |

A value of another type, an `answer_ms` outside its range, or a
`surface` outside its list is `invalid_request` ("This confirmation has
a field the server cannot read."), as every malformed confirmation is.
The fields hold no text. The server records them on the event as sent;
`sender`, `decision`, `note` and `remember` are unchanged.

**Order of releases.** `POST /v1/sessions/{id}/events` decodes a
confirmation strictly, so a server that predates this spec refuses the
new fields. A client sends them only to a server whose
`GET /v1/openapi.yaml` lists `answer_ms` in
`Event.user.tool_confirmation`; a client that gets `invalid_request` for
a confirmation that carried them sends it once more without them.

**What a client does not send.** The application the person answered
from is `sender.client`, which the server writes from the verified
token ([[085-the-client-a-message-came-from-and-a-days-activity]]); a
client never sends it, and it is not sent to the decision service.

**What the server measures.** `waited_ms` is the milliseconds from the
event that asked (the call's `agent.tool_use`, or the
`approval.requested` of a network approval) to the confirmation's own
event, both in the log's time. It is computed when the answer is
forwarded and kept in no event, since the log already holds both
times.

**What the decision service is sent.** `harness.Learner.Answered` takes
the confirmation and the time the call waited in place of `approve` and
`by`:

```go
type Learner interface {
	Answered(ctx context.Context, s session.Session, toolUseID string, a Answer) error
}

type Answer struct {
	Approve         bool
	By              string
	AnswerMS        *int64
	WaitedMS        int64
	DetailsOpened   *bool
	SuggestionShown *bool
	Surface         string
	Remembered      bool
}
```

`POST /v1/answers` carries `answer_ms`, `waited_ms`, `details_opened`,
`suggestion_shown`, `surface` and `remembered` beside `approve` and
`by`, each left out when unknown. `remembered` is `true` when the
confirmation carried `remember`; the pattern itself is never sent,
since it holds text of the call's input. `note` is never sent. A
network approval's confirmation is sent the same way, with its own
times. A person's `user.message` in place of an answer still sends
nothing ([[037-decision-services]]).

A decision from a remembered pattern, `always_allow` or any other rule
is sent as a decision and never as an answer, as today.

### The service's new kinds of ask, and when a learned allow decides

A decision service that knows how far a person's approvals reach can
suggest `ask` where its model alone would allow, and say why in
`ask_kind`:

| `ask_kind` | Meaning | `learned` mode |
|---|---|---|
| `wider` | the call reaches wider than any call of its kind the person allowed | kept for the person: `decided_by: learned`, verdict `ask`, review probability 1, as a `split` ask is |
| `split` | the evidence ratio is 0.5 or more and the person's answers about calls of this kind disagree | kept for the person, as today |
| `short` | the evidence ratio is 0.5 or more and the answers agree, with the approval probability still in the ask band | the rules and the risk score decide, as for `unsure` (decided by the owner on 2026-11-02) |
| `few` | the person allowed calls of this reach, but not with enough considered answers or in enough sessions | the rules and the risk score decide, as for `unsure` |
| any other value | | the rules and the risk score decide, as today |

A service that predates `short` sends `split` for answers that agree as
well; such an ask is kept for the person, as before.

A prediction may also carry `cover`, with `covered` true when the
person's approvals cover the call. `TOPOS_DECISIONS_LOOSEN` says when a
learned `allow` may run a call that the rules and the risk score would
not run without a person:

| Value | A learned `allow` in `progressive` (`learned` and `decide`) |
|---|---|
| `covered` (default) | decides when the prediction carries `cover.covered: true`; otherwise the rules and the risk score decide, as for `unsure`, with the suggestion recorded |
| `never` | the stricter of the allow and the rules' verdict decides; the service can make a call wait or stop it, and never run one the risk score would ask about |

Neither value lifts a call above the rules' ceiling: `always_confirm`, a
tool's hold, the session's network and `block_at` decide first, as
today ([[037-decision-services]]). A service that sends no `cover`
under `covered` never loosens the risk score, which is the safe reading
of an older service.

### Repeated refusals

**A refusal** is a call that did not run because:

| Cause | `decided_by` of the call, or the event |
|---|---|
| the risk score blocked it | `score` with `block_at` |
| the session's network blocked it | `network` |
| the decision service's suggestion blocked it | `learned` |
| the person denied its confirmation | the `user.tool_confirmation` with `deny` |
| the person denied a network approval it raised | the `approval.decided` with `deny` |

A block in `plan` mode is not a refusal: plan mode blocks every change
by design, and the agent is told so. A call that changes something and
runs (an allow, a flag, or an ask the person allowed) ends a run of
refusals. A read-only call neither counts nor ends a run.

**The counts.** The session counts refusals in the order of their
events, across its own conversation and every subagent: `consecutive`,
the refusals since the last call that changed something and ran, and
`total`, every refusal since the session started or since the fallback
last ended.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_DENIALS_CONSECUTIVE` | 3 | refusals in a row that start the fallback; 0 turns the limit off |
| `TOPOS_DENIALS_TOTAL` | 20 | refusals in a session that start the fallback; 0 turns the limit off |

The authorizer's allow of `session.create`, `session.fork` and
`session.send` may carry `denials`, `{"consecutive", "total"}`, which
apply when lower than the installation's; it can make the limits
stricter and never looser.

**The fallback.** When a refusal reaches either limit, the runner
appends `session.denials_reached` after it:

| Field | Meaning |
|---|---|
| `consecutive`, `total` | the counts at that refusal |
| `limit` | `consecutive` or `total`, the one reached first |
| `effect` | `ask` on an attended turn, `stop` on an unattended one |

On an **attended** turn, from the next call until the person's next
`user.message`, every call that changes something asks the person,
whatever `always_allow`, a remembered pattern, the sandbox rule, the
risk score or a decision service would decide: its `decided_by` is
`denials`, its review probability 1. A call the rules block stays
blocked, a read-only call and a call that reaches memory folders alone
([[093-memory-folders-the-file-tools-reach]]) run as before, and plan
mode is unchanged. The decision service is asked as in `confirm` mode:
its suggestion is recorded, the decision is sent, and the person's
answer teaches it. The person's next `user.message` ends the fallback
and resets both counts.

On an **unattended** turn ([[087-unattended-turns-a-watchs-allowance-and-pace-and-the-context-report]]),
no person is there to ask. The turn ends at the refusal that reached the
limit, every thread of it at its next step, and the session goes idle
with the stop reason `denials`. The next turn a person begins runs in
the fallback until that person's next message, as an attended turn
would.

`session.denials_reached` and the stop reason `denials` are additions to
schema v1 ([[004-session-log]]); `denials` joins the closed set of
`decided_by` ([[012-permissions-and-approvals]]). The fold derives the
counts and the fallback from the log, so every runner applies them the
same way after a handoff ([[017-external-runners-handoff-fork]]). A
refusal is never sent to the decision service as an answer: a block by
the rules is not the person's view, and a stopped turn is not a denial.

### Both ends of a handoff

**Inside.** Each call a subagent makes is already decided like any call
of the session, through the same decider and the subagent's narrowed
policy. That stays.

**Going out.** A subagent cannot reset what its caller earned:

- The counts and the fallback are the session's. A refusal in a subagent
  counts, a subagent started or sent a message while the session is in
  the fallback runs in it, and a subagent's calls end a run of refusals
  only as the caller's do.
- `agent`, and `send_message` to a subagent, still run as reads, since
  every call the subagent makes is decided on its own. Their action
  carries what the call hands over, as attributes: `handoff.writes`, true
  when the subagent holds a tool whose effect is `write`;
  `handoff.external`, true when it holds one whose effect is `external`;
  and, for `agent`, `handoff.isolation`, `shared` or `worktree`.
- The task text is never sent. It is the agent's own words, as the
  agent's messages are, and no part of this contract carries those.

**Coming back.** When a subagent's run ends and its final text becomes
the result of the `agent` or `send_message` call, the harness counts the
refusals among the subagent's calls in that run. When there is at least
one, the result begins with the text of
`results/subagent/refused-v1`:

> {n} of this subagent's calls did not run: a rule or the person refused
> them. Check its work before you rely on it.

and the call's `tool.result` meta records `refused`, the count. The text
names no rule, no reason and nothing the decision service said; a
reason a decision service gives is the person's and never reaches an
agent ([[037-decision-services]]).

### Names and where they are listed

| Name | Kind | Listed at build in |
|---|---|---|
| `TOPOS_DECISIONS_LOOSEN`, `TOPOS_DENIALS_CONSECUTIVE`, `TOPOS_DENIALS_TOTAL` | variables | the configuration table of [[002-scaffold-and-configuration]] |
| `boundary`, `denials` | members of an allow | the authorizer's limits ([[006-identity]]) |
| `denials` | a value of `decided_by` | the closed set of [[012-permissions-and-approvals]] |
| `session.denials_reached`, the stop reason `denials`, `scope` on `agent.tool_use`, the four fields of `user.tool_confirmation` | schema v1 additions | the tables of [[004-session-log]] |
| `results/subagent/refused-v1` | a prompt | the prompts tree ([[011-instructions-and-skills]]) |

### What Topos never sends

The agent's messages and reasoning, a tool's output, a subagent's task,
a confirmation's `note`, a remembered pattern, a path, a host, a URL or
a ref name as a scope or target value. The decision service learns from
what a call is and what the person answered, and from nothing the agent
wrote about it.

## Not in this spec

- **The branch a `bash` call pushes from.** Knowing it would mean
  reading `HEAD` on the machine before every `bash` call. A push without
  a refspec is read by the decision service as an unknown target, and
  `protected_refs` keeps the default branch out of reach where the git
  host enforces it.
- **An input probe.** The auto mode classifier warns the agent when a
  tool result looks like an injection. That reads tool outputs, which
  nothing in this contract does; it would be a spec of its own.
- **Clients.** How a client measures `answer_ms`, offers details and
  keeps the suggestion out of sight until the person answers is each
  client's; this spec fixes the fields they send.
- **Scope for MCP tools and client tools.** Their reach is their own
  server's to describe; until a manifest field says it, they send none.
- **A per-agent limit on refusals.** The limits are the installation's
  and the authorizer's.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Each call of the table is sent with the `scope`, `target` and `environment.holds` the table gives, on a Cella machine, a host and a person's own computer | `internal/decisions.TestScopeByCall`, `harness.TestTheReachOfACall` | built |
| No scope or target value holds a path, a host, a URL or a ref name, for calls whose input holds markers in each | `internal/decisions.TestScopeHoldsNoInputText` | built |
| A `web_fetch`'s `crosses.organization` follows the allow's boundary, and is left out without one | `internal/decisions.TestBoundaryDecidesOrganization`, `internal/server.TestTheBoundaryFollowsTheLatestAllow`, `authorizer.TestDecodeBoundaryAndDenials` | built |
| `contact` is `first` for the session's first fetch of a host and the `publish` that creates an app, and `repeat` after | `internal/decisions.TestFirstContact` | built |
| A call's `agent.tool_use` records `scope` before the call waits: the service's reading joined with Topos's claims and `source: service` when a prediction answered with one, Topos's claims alone and `source: topos` otherwise, and for `bash` without a service no member but `source` | `harness.TestToolUseRecordsScope`, `internal/decisions.TestScopeSource`, `session.TestAScopeJoinsTheWiderOfTwo` | built |
| No member of a recorded `scope` holds a name of a ref, an app, a host or a path | `harness.TestRecordedScopeHoldsNoName` | built |
| A confirmation with the four fields is recorded as sent, a malformed one is `invalid_request`, and one without them is accepted as before | `internal/server.TestConfirmationAnswerFields`, `session.TestHowThePersonAnsweredIsChecked` | built |
| The answer sent carries the confirmation's fields, `waited_ms` from the log, and `remembered` without the pattern or the note | `internal/decisions.TestAnswerCarriesHowThePersonAnswered`, `harness.TestResumeForwardsAnswers` | built |
| `api/openapi.yaml` describes `scope` on `Event.agent.tool_use`, the four fields on `Event.user.tool_confirmation`, `session.denials_reached`, the stop reason `denials` and `decided_by: denials` | `TestOpenAPIMatchesHandlers`, `TestOpenAPIIsGenerated`, `internal/server.TestAFieldNoteDescribesItsMember` | built |
| In `learned` mode a `wider` and a `split` ask are kept for the person, and a `few`, a `short` and an `unsure` ask are left to the rules | `internal/decisions.TestWiderIsKeptAndFewIsLeft` | built |
| Under `covered` a learned allow without `cover.covered` leaves the call to the rules, and under `never` a learned allow never runs a call the risk score asks about | `internal/decisions.TestLoosen`, `internal/config.TestLoosenAndDenials` | built |
| Three refusals in a row, or twenty in a session, append `session.denials_reached`; a read neither counts nor resets; plan-mode blocks do not count | `harness.TestRefusalsAreCounted`, `session.TestRefusalsAreCountedFromTheLog` | built |
| In the fallback every call that changes something asks with `decided_by: denials`, a blocked call stays blocked, and the person's next message ends it | `harness.TestTheFallbackAsksForEveryChange`, `internal/decisions.TestTheFallbackIsAskedAsConfirm` | built |
| An unattended turn that reaches a limit ends with the stop reason `denials`, and the next attended turn starts in the fallback | `harness.TestAnUnattendedTurnStopsOnRefusals`, `session.TestAStoppedTurnHandsTheNextTurnTheFallback` | built |
| The authorizer's `denials` lowers the limits and cannot raise them, and 0 in the variables turns a limit off | `internal/server.TestDenialLimits`, `session.TestAnAllowsDenialsOnlyLower`, `internal/config.TestLoosenAndDenials`, `internal/hosted.TestTheHarnessKnowsTheRefusalLimitsAndTheRefRules` | built |
| A subagent's refusals count toward the session's, a subagent started in the fallback runs in it, and a fork after a handoff reads the same counts from the log | `harness.TestSubagentsShareTheCounts` | built |
| A subagent whose run had a refusal returns its result after the fixed warning with `refused` in the meta, and one without returns as before | `harness.TestARefusedSubagentWarnsItsCaller`, `session.TestRefusedInCountsOneThreadsRun` | built |
| `agent` and `send_message` carry `handoff.*` and every call carries `depth`, and no task text reaches the decision service | `internal/decisions.TestHandoffAttributes`, `harness.TestHandoffAndDepthReachTheDecider` | built |
| `environment.repositories` is the visibility a read with no credential found for each delivered repository | `runner.TestARepositorysVisibilityIsWhatAReadWithNoCredentialFinds`, `harness.TestTheReachOfACall`, `internal/hosted.TestASessionClonesCommitsAndPushesThroughItsSandbox` | built |

## Outcome

Built on 2026-11-02 as wave 1 of the consent design, with the owner's
decisions of that day: `short` joins the asks the rules decide,
`TOPOS_DECISIONS_LOOSEN` defaults to `covered`, and the limits are 3
and 20, which an authorizer can only lower. Every criterion above is
proven by its test. The status is `testing` until a release carries it.

Where the build reads the design one way of several, or goes past it:

- **`denials` at a send is not read.** An allow of `session.create` and
  `session.fork` sets the session's limits, kept in the header as
  `denials`. No event carries an authorizer's change of a session's
  policy at a send today, as the lists and thresholds are read at the
  create alone, so a send's `denials` is ignored rather than given a new
  event type.
- **A send's `boundary` rides on `session.network_changed`.** The
  header keeps the latest allow's boundary as `boundary`; a send whose
  allow names another appends `session.network_changed` by the
  authorizer with a `boundary` member, which moves no network by itself.
  This is a schema v1 addition beside the ones the names table lists.
- **`environment.repositories` reads the git host at delivery.** The
  runner reads each delivered repository once with no credential, as
  the checkpoints of [[035-hosted-checkpoints-at-the-git-host]] do:
  asked for a credential is `private`, answered is `public`, anything
  else is not known. `session.machine` records it as `visibility` on
  each repository, another schema v1 addition. A session sends
  `mixed` only when every repository's visibility is known, and nothing
  when one is not. A `file://` repository is never read.
- **`protected_refs` trusts the installation's git host.** It is sent
  when every repository of the session is on `TOPOS_ORIGO_URL`, the git
  host whose ref rules [[019-git]] describes; that spec's test of a
  refused push to a protected branch is still not built.
- **A malformed confirmation's sentence is the detail.** The error table
  gives `invalid_request` one message, "The request is not valid.", so
  "This confirmation has a field the server cannot read." is the
  refusal's developer detail, followed by what could not be read.
- **`first` is the wider contact.** Where the service's scope and
  Topos's claim differ in `contact`, the recorded scope keeps `first`,
  the wider reach, as the decision service's order has it, rather than
  the order the members table lists the two values in.
- **The reach a prediction reads is read leniently.** A boundary of the
  prediction's `crosses` may be a boolean or one of `yes`, `no` and
  `unknown`; `unknown`, and a word outside a scope's lists, is left out
  of what the call records. `target_kind` is one of a closed list of
  words (`path`, `ref`, `app`, `host`, `repository`, `package`,
  `deploy`, `service`), so a service's value outside it is dropped.
- **A memory call is a read for the counts.** A call that reaches
  memory folders alone, and a question, neither counts nor ends a run
  of refusals, as a read-only call does; otherwise a call the fallback
  leaves to run would reset the run it guards.
- **The fallback keeps the rules' asks and blocks.** A call that
  `always_confirm`, its tool, the session's network or `block_at`
  already asks about or blocks keeps that `decided_by`; `denials` marks
  the calls the fallback itself made ask.
- **A rollout's run stops as an unattended turn does.** A run of a
  rollout ([[077-rollouts]]) begins with a message, so its turns count as
  attended, but no person is there to ask and every ask is refused at
  once; a limit reached in it records `effect: stop` and ends the turn
  with `denials`, rather than refusing every later change of the run.
- **An unattended stop waits for the step.** The calls a step decided
  before its refusal reached the limit still run, and the turn ends with
  `denials` once they are answered, before the next request; every
  other thread of the turn ends at its next step.
- **An answer leaves out what the service would refuse.** The server
  holds a confirmation's fields to their ranges, and the decider checks
  them again before it relays an answer: an `answer_ms` outside 0 to
  86400000 and a `surface` outside the list are left out, and a
  negative `waited_ms` is sent as 0, so a confirmation another writer
  appended still teaches the service.
- **A thread tool's input is not sent.** `agent`, `send_message` and
  the advisor are sent to the decision service without their input, so
  no task, message or question of the agent reaches it.
- **An uncovered allow goes to the rules in `decide` too.** Under
  `covered`, a learned allow without `cover.covered` is decided by the
  rules and the risk score in `decide` as in `learned`, recorded with
  their layer and the suggestion beside it.
- **What a `publish` reaches.** A call that changes who can open an app
  and nothing else is `primary`, as a release is. A preview is public
  only where the call sets `access: link`; otherwise its `public` is
  `false` for a private app and left out for any other. A fetch's host
  counts as reached once an earlier fetch of it returned `ok`.
