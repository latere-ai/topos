# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

## v0.62.0 - 2026-10-31

### Added

- **A person's message sent while a turn runs is read at the agent's next step.** A `user.message` of the session's own conversation appended after a `session.status` `running` and before the status that closes the turn waits. The person's waiting messages are read by the turn's next request, after the results of the calls running when they arrived, each led by `transcript/while-working-v1`: `<name> sent this while you were working:`. A trigger's run, a wake or another session's message never steers a running turn: it is read by the first request of the next turn, which a trigger's or a wake's message starts once the turn ends, unless the session then waits for the person, whose answer starts that turn. See `docs/messages-while-working.md`.
- **The log says when a message was read.** Just before the request that reads them, the runner appends `messages.read` with `event_ids`, the waiting messages that request is the first to read, and the request's `model.request` carries `names_reads: true`. A request folds a read message where its `messages.read` is, not where it was appended. A `model.request` without `names_reads`, which every session recorded before this release has, read every waiting message up to its `fold_seq`, so such a session folds byte for byte as before.
- **A message can be sent to be read now.** `send_now: true` on a person's `user.message` while a turn runs cancels a request in flight at once. While calls run, every running `bash` command moves to the background and keeps running, its call answered `ok` with what it wrote so far, its process id and the log it goes on writing to (`results/bash/moved-now-v1`), and the other calls have `harness.SendNowGrace`, one second, to return. When every call returns, the turn goes on and its next request reads the message; otherwise the turn ends `idle` with `stop_reason` `interrupted` and `detail` `send_now`, and the next turn starts at once and reads the message after `transcript/sent-now-v1`: `<name> stopped your work to send this:`. On an idle session `send_now` changes nothing.
- **A waiting message can be withdrawn.** `user.withdraw` with `event_id` takes back a waiting message its sender sent, before any `messages.read` names it: it is never read, its files are never written to the machine, and it starts no turn. The withdrawal is checked against the log it is appended after, so it and a read of the same message never both stand. It is refused `invalid_request` with no `event_id`, `forbidden` for a message someone else sent, and `conflict` for an event that is no message of the session, a message that started its turn, one already read and one already withdrawn. It is asked of the authorizer as `session.interrupt`, so an authorizer that allows by event type needs no change.
- **A machine can move a running command to the background on request.** `machine.ExecRequest.MoveNow`, once closed, moves a foreground command still running to a job log whatever it listens on, and `tools.Call.MoveNow` carries it to a tool. On the host machine the move is local; on the Cella machine it is the helper's new move frame. The runner uploads the helper of its own release to a sandbox that holds another, so a sandbox needs nothing new; a command a move cannot be made for runs on and is canceled after the grace.
- **The session package reads a message's state.** `session.WaitingMessages` answers every waiting message with its state, `waiting`, `read` or `withdrawn`, and `session.Unread` and `session.PendingWaiting` the ones still waiting.
- **The task suite can send a message while a tool runs.** A task's `while_working: {tool, text, now}` sends the person's message, to be read now with `now`, once the tool's first call of the session's own conversation runs; the scripted model's `expect` gains `text_contains`; and the `instructions/redirected` task checks that an agent told mid-command to change its plan follows the new plan.
- **A bash command over the memory folders opens no sandbox.** In a session with memory folders, a `bash` line whose every program is `ls`, `cat`, `head`, `tail`, `wc`, `grep`, `find`, `sort`, `uniq`, `cut`, `mkdir`, `rm`, `mv`, `cp`, `touch` or a shell builtin, and whose every path is under `/memory/`, runs on the runner over the folders, so `cd /memory/user && ls && cat notes.md` answers in milliseconds and reads the folder, where it used to open a sandbox that does not see it. The line answers as the sandbox's `/bin/sh` would, GNU's output and messages included; its writes are the file tools' writes, each conditional on the version the session read, and one of a file the session never read on the version it is at now; a line that empties a file forgets it, and a read-only folder answers `Read-only file system`. Whether a line runs there is decided before it runs; any other line runs in the sandbox as before. A later command that needs the sandbox while the directory is in a folder starts in the working directory, and its result says so. The model is told which commands reach the folders (`context/attachments-v3`). A line there runs within bounds whatever it holds: 50 000 steps, 10 seconds, 256 KiB for one value and 1 MiB for all, 16 MiB held, 64 MiB of files read and 50 000 names listed; one that reaches a bound stops with `/bin/sh: 2: <the bound>` and exit code 2, keeping what it changed before. A line longer than 64 KiB, nested deeper than 64, with a command of more than 16 expansions, more than 32 pipes or a pattern of more than 8 `*` runs in the sandbox, counted with the reason `size`. A line keeps at most 1 MiB of output, and at most half the runner's processors run such lines at once; a line past them waits under its timeout. `set -x` runs in the sandbox. The runner logs which tool call opened a session's sandbox and why, and counts bash calls by where they ran (`latere.topos.bash.runs`) and the calls that opened a sandbox (`latere.topos.machine.opens`). Requires `latere.ai/x/cella` with its `shell` package. See `docs/memory-folders.md`.

### Changed

- **A message held while the session waits for the person no longer counts as pending input.** `session.HasPendingInput`, and the runner's decision to start another turn, hold for a waiting message only once nothing the session waits for is outstanding: an approval, a question or a client's tool. The person's answer starts the turn, and that turn reads the message after the answer. A session ending on idle does not end while a message that would start a turn still waits.

## v0.61.0 - 2026-10-30

### Added

- **A memory folder can hold the person's past conversations.** A memory folder of an allow may name `sessions: {"metadata": {"<key>": "<value>" | null}}` in place of `url`, `owner` and `path`, with `access` `read_only`. The folder holds the initiator's other sessions of the agents of the session's context, kept by the one metadata entry (a string keeps that value, `null` the sessions without the key): one per fork tree, never the reading session's own tree, no run of a rollout, newest first. Each conversation is `/memory/<name>/<yyyy-mm-dd>-<session id>.md`, holding its title, its dates and what the person and the agent said on the session's own thread, at most 256 KiB, so `glob`, `grep` and `read` find and open earlier conversations. Every list asks `session.list` and every conversation listed or read asks `session.read` as the initiator, each naming `origin: {"kind": "memory", "session_id", "folder"}` and a read the conversation's `metadata`; a denied read leaves the conversation out, and a denied list reads `This session may not read these conversations now.` with the deny's reason as `refusal`. A folder of conversations needs no `TOPOS_FILES_AUDIENCE`: a runner in `toposd` reaches the server directly, and a remote runner over its lease at `POST /internal/v1/leases/{session}/conversations/{list,read}`. A release before this one refuses a create whose allow names such a folder, so an authorizer names it only to this release or later. See `docs/memory-folders.md`.
- **A list keeps the sessions without a metadata key.** `session.MetadataEntry` gains `Absent`, which every store filters by before it pages.
- **A trigger can name its reach.** `spec.reach: {kind, split}` names what the trigger's runs reach as a kind, lower-case letters, digits and underscores, a letter first, at most 32, which the installation's authorizer maps; Topos decides nothing from it. `trigger.create` and `trigger.update` carry `reach: {kind, split}`, `split` left out when absent, and each firing's `session.create` carries `reach: {kind}`. See `docs/trigger-reach.md`.
- **A trigger can run once per due scope.** With `reach.split: scope`, for a trigger on a schedule under policy `new` with no `session.target` and no `session.key`, each firing asks the new action `trigger.runs` as the owner, before any session starts, with the trigger's owner, name, agent and reach, the firing `{id, at, origin}` and `last`, each scope's latest started run. The allow's limits name the scopes due, `{"runs": [{"scope", "since"}]}`, and each starts a session keyed by its scope whose `session.create` carries `reach: {kind, scope, since}`; an allow that names none records the new outcome `skipped_nothing_new`, a run that cost nothing with no session and no model call, counted in `status.counts.skippedNothingNew`. More than 16 runs, a scope outside its pattern, or one named twice refuses the firing `runs_invalid`, and a deny refuses it with its reason. A scope whose session is still active is passed over under `skipIfActive`, a refused scope is a `refused` run while the others start, `maxActive` holds back no scope, and every session of the firing counts toward the weekly allowance. An applied trigger whose reach breaks a rule is `invalid_manifest` at `spec.reach.kind` or `spec.reach.split`. An authorizer that knows no kind allows with no limits, so on the owner policy a split trigger records `skipped_nothing_new` at each firing; an installation rolls an authorizer that decides `trigger.runs` before this release, or every firing of a split trigger is refused.
- **The runs read names each run's scope.** `GET /v1/triggers/runs` answers a run per scope of a split firing, each with `scope` and its own session's cost, and `skipped_nothing_new` among the outcomes. The Postgres store's migration `0023` adds `trigger_runs.scope` and keys a run by its firing and its scope.
- **The authorizer package reads the answer.** `authorizer.WireRuns` is the limits object of an allow of `trigger.runs`, and `authorizer.DecodeRuns` reads it as Topos does.
- **The document says how to keep a session's cost so far current from its stream.** The descriptions of reading a session, listing its events and streaming them say that `budget.spent_cost_usd_micro` is what the log cost through `last_seq`, that only `model.request` and `tool.result` add to it, each with its own `cost_usd_micro`, a redacted result's kept in its tombstone, and that a fork's own spending is `spent_cost_usd_micro` less `carried_cost_usd_micro`. A client reads the session once and adds each later event's cost. Nothing on the wire changed. See `docs/cost-and-progress.md`.
- **The document describes a todo call's list.** `TodoResultMeta` is the meta a `todo` call's `tool.result` records: `todos`, the conversation's whole list after the call, each item `{id, content, status}` with `status` `pending`, `in_progress` or `completed`. A refused call records none, and an emptied list is `[]`. Nothing on the wire changed.

## v0.60.1 - 2026-10-30

### Fixed

- **A session handed to a runner that has stopped waiting goes back at once.** A runner in the runner role that hung up on its claim while `toposd` was handing it a session never received the session, which stayed held for that runner, with nobody driving it, until the claim expired about a minute later. `toposd` now releases the session as soon as it sees the runner gone, and the next claim takes it.

## v0.60.0 - 2026-10-29

### Added

- **A session can keep memory folders across sessions.** An authorizer's allow of `session.create` or `session.fork` may name `memory`: up to 4 folders of a files service, each `{"name", "url", "owner", "path", "access"}` with `access` `read_only` or `read_write`. `read`, `write`, `edit`, `glob` and `grep` reach each one at `/memory/<name>/`, and a second session with the same folder reads what the first wrote. The runner carries every such call with the session's own token for `TOPOS_FILES_AUDIENCE`, so no sandbox holds a credential, and a call whose every path is under `/memory/` opens no sandbox: a session that only talks and remembers never has one. Every write is conditional on the version the session last read (`If-Match`, or `If-None-Match: *` for a new file), and one the folder changed under reads as the file tools' changed text and overwrites nothing. A write of no content, or an edit that leaves the file empty, forgets the file with a conditional `DELETE` that leaves it in the trash, and reads `Forgot <path>.` A `read_only` folder refuses a change before any request; a rename and a command under `/memory/` are refused; a file holds at most 1 MiB. A refusal of the files service reads as `<path>: <message> <words>`, with the words that follow `<reason>: ` in its `details.detail`, after the action Arca names before the reason or not, and the reason as the meta's `refusal`; a service that does not answer reads `Memory could not be reached: <detail>.` A session's folders are answered as `memory: [{"name", "access"}]`, and a fork's are its own allow's. A list that breaks a rule, or any list on a server with no `TOPOS_FILES_AUDIENCE`, refuses the request `authorizer_unavailable`. A rollout's run gets none. See `docs/memory-folders.md`.
- **The attachments block names the memory folders.** A session with memory folders renders `context/attachments-v2`, which names each folder and its access after the session's apps, on every request; a session without them renders `context/attachments-v1` byte for byte as before.
- **A write under `/memory/` says what it saved.** Its `agent.tool_result` meta carries `memory: {"folder", "path", "action"}`, the path in the folder and `saved` or `forgotten`, beside `path`.
- **A memory call is decided as a memory.** A call whose every path is under `/memory/` scores 0.1 with the feature `memory`, and is allowed in `confirm` and `progressive` with the reason `a memory the person can review`; in `plan` a read is allowed and a write blocked. `always_confirm` naming `memory` asks the person about every memory call.

### Changed

- **A subagent's events are `subagent.started`, `subagent.message` and `subagent.ended`, and the field that names a subagent is `subagent`.** There is no compatibility: a reader of `thread.started`, `thread.message` and `thread.ended` and of the `thread` field must read `subagent.started`, `subagent.message`, `subagent.ended` and `subagent` from this release. The field is the id of the subagent whose conversation an event belongs to, absent for the session's own, with the value `thread` had; it is renamed on every event the events list and the stream answer, and on every delta of the stream, a reset included. A replay's step names its subagent as `subagent` too. A session written before reads as one written after: the Postgres store's migration renames the stored types and the `events.thread` column to `events.subagent`, and `toposd` renames a data directory's logs the first time it opens it. A replica of an earlier release cannot write to the migrated database, so every replica moves to this release together. The commit a subagent makes on its worktree branch is `topos subagent <id> turn <n>`, and the operator log's attribute for it is `subagent`.
- **An agent's limits on its subagents are `spec.subagentLimits`, with `maxDepth` and `maxConcurrent` as before.** There is no compatibility: a manifest that names `spec.threads` is refused `invalid_manifest` as any unknown field, so a manifest written for an earlier release must say `subagentLimits` before it is applied again, and a reader of an agent's document reads `spec.subagentLimits`. A problem with the limits is reported at `spec.subagentLimits.maxDepth` or `spec.subagentLimits.maxConcurrent`. What a release before stored is renamed the first time `toposd` opens it, once: every agent version's document and bundle, whose digest becomes the digest of the renamed spec, the one an apply of the same manifest now resolves to, so applying it again unchanged keeps the version; and every session's agent, which the session gets as a renamed spec and bundle beside the ones it had, so a session created before goes on running. On Postgres the rename is one transaction over the agent versions and the sessions, recorded in a new `data_rewrites` table so a later start passes it by.
- **The `agent` tool's description says "a focused subagent"** where it said "a focused helper". New requests send `tools/agent-v2`.

## v0.59.0 - 2026-10-25

### Added

- **A subagent can run the model of the agent that starts it.** An inline subagent's spec may leave `model` out. The subagent then runs, at each of its turns, the model the agent that started it stands on at that turn, with that model's connection, figures and reasoning level: for a session on a routed name such as `tier/quick`, the model the authorizer resolved the name to. Nothing is connected for such a subagent when the session's drive starts, so an agent on a routed name can now have subagents; before, every subagent named a model that was connected by its name alone, and a name only the authorizer resolves failed the drive. When its caller moves to another model, by a failover within a turn, a person's change or the authorizer's, the subagent's next turn runs on that model too, and the model it ran before is not connected again; a change to another model family between two of its turns is translated as a session's change of model is. `thread.started` records the model the subagent started on, and each of its `model.request`s the model it ran. Its requests are priced by that model's figures and count in the session's spend as every request does. A subagent's model given without a name is `invalid_manifest`; an agent, and a subagent that names another agent, still name their model, and the OpenAPI document no longer lists `model` among `AgentSpec`'s required members.

### Changed

- **The subagent tools are `agent` and `send_message`.** The tool that starts a subagent, `spawn`, is now `agent`, with the inputs `subagent` (before, `agent`), `task`, `isolation`, `tools` and `budget`. The tool that continues one, `message`, is now `send_message`, with `to` (before, `thread`), `message` (before, `content`) and `end`. The error codes `too_many_threads` and `thread_not_found` are now `too_many_subagents` and `subagent_not_found`; `unknown_subagent`, `depth_exceeded` and `isolation_unavailable` keep their names. The tools' descriptions and results say subagent, and the line after a subagent's answer is `Subagent <id> is idle; send it more work with send_message.` The history tool calls the agent that started a subagent by that name. The event types `thread.started`, `thread.message` and `thread.ended` and the manifest's `spec.threads` are unchanged.
- **The harness prompt names the new tools.** Its section on subagents, which an agent with subagents is given, names `agent` and `send_message`: new requests record `prompt_version` `harness/3`, and `harness/4` on a person's own computer. For an agent without subagents each renders byte for byte as `harness/1` and `harness/2`.
- **The old names still work in this release.** A model's call of `spawn` with `agent`, or of `message` with `thread` and `content`, runs as the new tool with the new inputs, and its `agent.tool_use` records the call as the model wrote it; a call recorded under an old name, held for a person's approval or left open when a runner stopped, resumes as the new tool. A pattern of an approval list, or one a person's answer remembered, that names `spawn` or `message` matches the new tool's calls. A client tool named `message` stays the client's. The release after this one refuses the old names.
- **An agent with subagents may not name a client tool `agent` or `send_message`**: the manifest is `invalid_manifest`. An agent stored earlier with such a client tool keeps it, and is not offered the harness's tool of that name.
- **The descriptions of `read`, `write`, `edit`, `bash` and `todo` say "you" where they said "this thread".** New requests send `tools/read-v4` (`tools/read-v5` where a parsing service reads PDFs), `tools/write-v3`, `tools/edit-v3`, `tools/bash-v3` and `tools/todo-v2`. `bash` says a command past its timeout is stopped. A write or an edit of a file the agent has not read is refused with `<path> exists and you have not read it; read it before writing to it.` What each tool does is unchanged.

## v0.58.0 - 2026-10-25

### Added

- **A trigger can fire at a list of times.** `spec.times`, a list of 1 to 100 RFC 3339 times with an offset in place of `schedule`, `on` and `at`, fires the trigger once at each, as `at` fires once: each time is a firing of its own, `skipped_late` past `maxAge` or once the next listed time has come, and one refused for a limit that resets, or skipped at its allowance, waits no later than the next listed time. The latest time's firing spends the trigger, which the server deletes as it deletes a spent one-shot; an earlier time's keeps it. A time already past may stay in the list and never fires, so a client can send back the list it read; a list with no time to come, one instant listed twice, or more than 100 times is refused `invalid_manifest`. `status.nextFireAt` is the next listed time, `trigger.create` and `trigger.update` carry `times`, and `GET /v1/triggers/runs` answers such a trigger with `fires_on: "times"` and its times still to come as `fires`. A manual fire runs once and spends no listed time. See `docs/trigger-times.md`.
- **A trigger can name the model its runs start on.** `spec.session.model`, `{"name": "<model>", "reasoning": "<level>"}` with either member or both, the shape a change of a session's model takes, is the model and the reasoning level each session a firing starts begins on, in place of the agent's; a member left out is the agent's. The firing's `session.create` is asked with it as `model`, and the level under `reasoning` and `effort`, as a create that names its model is, and the allow is read as for the agent's own model: a model it names runs in the trigger's model's place, with the trigger's name as `via`, at the level it names when it names one. A firing that continues an open session leaves the model that session runs, one the person changed it to included, and a trigger under `session.target` ignores the field. An apply whose model names neither member, a name with space around it or another level is refused `invalid_manifest`. A trigger without it runs its agent's model as before, and keeps its digest. See `docs/trigger-models.md`.

### Fixed

- **The agent's trigger tool can change, pause and resume a one-shot.** A trigger with `at` named and labeled as a routine could not be changed, paused or resumed through the `trigger` tool, which wrote it through a routine's fields, a schedule or events. Such a trigger, and one with `times`, is now changed as written, its times kept, and a change to a schedule or to events replaces them.
- **A trigger that names `session.target` applies through the API.** `PUT /v1/triggers/{name}` of a trigger whose `spec.session.target` names a session answered `internal`, since the route could not ask `session.send` about that session. It now asks it, and applies the trigger once the authorizer allows the send. A wake, which its session's tool sets, was not affected.

## v0.57.0 - 2026-10-24

### Added

- **One read answers every trigger's runs and when it fires next.** `GET /v1/triggers/runs?since=&until=` answers the triggers the list would, paged and narrowed by `owner` as it is, each with its runs since `since` (a run being a firing that started or continued a turn, with what that turn cost, or one `skipped_budget` or `refused` stopped), the mean cost of its latest five runs, its `max_age`, and the times it fires before `until`: a schedule's in its zone, a one-shot's time or its next try while it waits, none for an event trigger. A wake answers the runs of every wake set in its session, and a trigger the wakes of the sessions it started. It is asked as `trigger.list`, so an authorizer needs nothing new. A client forecasts a person's week or month from it with one request. See `docs/trigger-runs.md`.
- **Each run's cost is kept.** Every firing that is a run is recorded where the firing is recorded, and its turn's cost is written where the logs are already read, at each firing's count of the allowance and at the read for a turn that had not ended, so a month is answered without reading every log. A wake's runs outlive the wake. Runs are kept six weeks. The Postgres store adds the table `trigger_runs` (migration 0020).

### Changed

- **A trigger's `status.createdAt` is its first apply.** It was the time of the latest version, so a trigger paused, resumed or changed answered the day of that apply as when it was made. The version and the digest still move with each change.
- **No trigger may be named `runs`**, the path of the new read, as none may be named `hold`.

### Fixed

- **A one-shot refused until a limit resets waits for it.** A one-shot, a wake included, whose firing an installation's authorizer refused for a limit that frees up again, such as a person's share of credit for proactive work, was spent, so a follow-up never came. When the deny names when its limit resets, as `limits.resets_at`, the firing is still recorded `refused` with its reason, the one-shot is kept, and it is tried once more at that time, under its own time, so its `maxAge` still bounds the wait (a wake's is 24 hours). The authorizer is asked again then, once, not at every minute's pass. A deny that names no reset spends it as before. See `docs/allowances.md`.
- **A wake refused for good leaves its session.** A wake whose message the authorizer refused, with no reset named, was spent with nothing recorded, so the session's `wake` kept a check-back that never came. Its session now records `session.wake_cleared` with the new reason `refused` and the refusal's reason as `refusal`, and the header's `wake` clears.
- **A price or an allowance too large to hold is refused.** A decimal
  amount whose whole part is past what an int64 of millionths holds, such
  as a trigger tool's `perWeekUsd` of `18446744073710`, was wrapped around
  into a much smaller amount; it is now refused as `invalid_input` or, in
  a catalog, as an invalid price.

## v0.56.0 - 2026-10-23

### Added

- **A trigger can name a weekly allowance.** `spec.budget.perWeek`, in USD micros, is the most its runs may cost in any 7 days. The server counts each run a firing began, from its message through the turn's closing status, in the sessions the trigger started or continued, and the runs wakes began in the sessions it started. A firing while that spend is at or past the allowance starts nothing and records the new outcome `skipped_budget`, counted as `status.counts.skippedBudget`; a one-shot, a wake included, keeps its time and is tried each minute until the week allows it or `maxAge` passes, and nothing needs a resume. `trigger.create` and `trigger.update` carry `budget {per_week_usd_micro}`, so an installation's authorizer can refuse a trigger without one or above a cap. See `docs/allowances.md`.
- **A trigger says how its week is going, and so does each run.** `status.spend` `{windowDays, spentUsdMicro, perWeekUsdMicro, perLookUsdMicro, expectedLooks, projectedUsdMicro}` is written at each firing that counts toward the trigger, and the firing's `user.message` carries the same numbers as `spend`, naming the trigger they count toward. The agent reads them in one line before the message, and, when the projection passes the allowance, is told it is ahead of pace: keep the run short, set the next wake later, or offer a change that runs less often.
- **An agent's offer carries its weekly estimate.** The `trigger` tool takes `perWeekUsd`, the agent's estimate in USD of a week of its runs, on a proposal and a change, applied as `spec.budget.perWeek`; a change without it keeps the allowance. Its description gives the model's input and output price from the catalog to estimate from, and says the estimate is the ceiling the server enforces. `list` answers each trigger's allowance and its week's spend.
- **A turn nobody watches is decided without what the person remembered.** On a turn a trigger's message or a wake began, the patterns the person's confirmations remembered are not applied; the lists, the mode, the network, a decision service and a tool's hold decide as on any turn, and a call the mode asks about waits for the person. `agent.tool_use` and `approval.requested` record `attended`, and a decision service reads it in the action's `attributes`. See `docs/unattended-turns.md`.
- **The authorizer reads which call asks.** Every `trigger.create`, `trigger.update` and `trigger.delete` a session's call asks carries `origin.action`, the `trigger` tool's `propose`, `change`, `pause`, `resume` or `delete`, or the `wake` tool's `set` or `cancel`, so an authorizer tells a pause from a change or a resume at the question before the call is held.
- **Every model request reports how full its context is.** A step's or a compaction's `model.request` records `context {window, used, source, parts}`: the model's input window, the tokens used, from the provider's count or the harness's estimate as `source` says, and the instructions, tools, messages, files, summary and free tokens, the estimates scaled to what was used. The Session folds the latest of its own thread's as `context_report`. See `docs/context-report.md`.

## v0.55.0 - 2026-10-23

### Added

- **One switch holds every trigger a person owns.** `GET /v1/triggers/hold` answers `{held, since}` for the caller, and `PUT /v1/triggers/hold` with `{"held": true}` or `{"held": false}` holds or releases every trigger they own, across every context. While held, the minute loop fires none of their triggers and records nothing, the fire route answers `conflict`, and a one-shot whose time passes keeps it; an agent's `trigger` and `wake` calls go on, and their results say every trigger is paused. On release a schedule starts from then and fires nothing it missed, and a one-shot fires within the minute, a late wake saying when it was due. A trigger's own `suspend` stays as it was. Both routes ask the new action `trigger.hold`, naming `owner` and, to set it, `held`; roll the authorizer that answers it first. No trigger may be named `hold`. See `docs/holding-triggers.md`.
- **A trigger's run can stay quiet.** A trigger's new `spec.session.notify` is `always`, the default, or `judged`: a judged trigger's message carries `notify: "judged"`, and the turn it begins records `outcome` `quiet` on its closing status, idle or the end `end_on_idle` makes, unless the agent calls the new `notify` tool, waits for the person, or asks them on the way. A wake's turn is judged whatever its trigger says. `judged` is refused for an agent whose latest version does not name `notify`. The Session gains `outcome`, the latest turn's, and `notice`, the latest notify's title, sequence and time, so a client marks only the runs worth a look. See `docs/quiet-runs.md`.
- **An agent can tell the person a run found something.** The `notify` tool, offered on every runner to the session's own thread when the agent names it, takes one line the person reads, at most 80 characters, once a turn; it changes nothing outside the log, and its result's `meta.notice` holds the title. On a turn nobody waits for, the agent reads that it notifies only for what deserves attention now.
- **A trigger's sessions can wait for their owner's answers.** `spec.session.attended` makes each session a firing starts attended, so its questions wait for the owner; a firing's `session.create` is asked with `attended`.
- **An agent's offer names whether its runs stay quiet.** The `trigger` tool takes `notify`, `always` or `judged`, in a proposal and a change; an offer on events is judged by default when the agent names `notify`. Every trigger's `trigger.create` and `trigger.update` carry `notify`.
- **An agent can come back to a conversation later.** An agent whose manifest names the new `wake` tool sets the session's next wake: at a time it chooses, at least 10 minutes and at most 168 hours ahead and inside the session's life, the note it wrote to itself comes back into the same session as a `user.message` from the wake's trigger, with a `wake` field `{trigger_id, set_at, due_at, count}`. A session has one wake, which a second set replaces and the agent cancels. After 10 wakes in a row with no message, answer or confirmation from the person who started the session, the next set waits for that person, and only they answer it. The Session's new `wake` field shows the pending wake, and the log records `session.wake_set` and `session.wake_cleared`. A session's end, expiry, archive or deletion clears its wake; a person or a client cancels one at `DELETE /v1/triggers/{id}`, recorded with the sender. A session created with `end_on_idle` stays open while a wake is pending and ends at the idle after its last. The hosted runner alone offers the tool, never to a subagent's thread, and `topos run` refuses an agent that names it. See `docs/wakes.md`.
- **A trigger can fire once, and continue one session.** `spec.at`, an RFC 3339 time in place of `schedule` and `on`, fires the trigger once, and the server deletes it with the firing that spends it; a held firing keeps it until its message is sent, and a failed one runs again. `spec.session.target`, under `policy: continue` and beside no `key`, names the open session every firing continues; the applier must be allowed `session.send` on it. A firing whose target has ended or is gone records the new outcome `skipped_ended`, counted in `status.counts.skippedEnded`.
- **A trigger says who paused it and why.** `status.paused` `{why, by, sessionId, toolUseId, at}` is written when an apply suspends a trigger, with the agent's why and its call for the `trigger` tool's pause, kept while the trigger stays suspended, and cleared when it resumes.
- **An offer on events is the SDK's own shape.** Its `event-` name and the event after its ask are written by `latere.ai/x/sdk-go/agents/routine` v0.10.0, whose `ReadEvent` reads it back, so every client reads it as Topos writes it.
- **The authorizer is asked about a session's wake.** A wake's set asks `trigger.create`, or `trigger.update` when it replaces one, and its cancel `trigger.delete`, as the session's initiator with `org_id` of its context and `origin` of kind `wake`; a create or an update of any one-shot trigger carries `at`. A wake is named `wake-` and the session id's ULID in lowercase and labeled `applied-by` with the session's agent.

## v0.54.0 - 2026-10-23

### Added

- **An agent can change, pause, resume and delete the person's triggers of it.** The `trigger` tool takes `change`, `pause`, `resume` and `delete` of a trigger the person who started the session owns, of the session's agent, by its id, which `list` now answers with each trigger's ask. A change names only what changes, its name, ask, schedule or events, and keeps every other field; a routine stays a routine. A change, a resume and a delete wait for that person every time, as a proposal does, and plan mode blocks them; a pause runs at once, since it only stops work, and the agent says what it paused and why. None reaches another agent's trigger, another person's, or a session's wake: each is answered as a trigger it cannot see. Each keeps the trigger's `status.origin`.
- **An agent can offer work that starts when an event arrives.** A proposal names `on` in place of `schedule`. Every email that arrives, or every call of the person's link, is applied as the routine that starts from an email or a link, which every client reads as one. Any other events, such as a repository's pushes, are applied in the same shape with the filter as their `on` and the event after the ask, under the name `event-` and the routine's digits, so a client's list of routines leaves them out. See `docs/offers.md`.
- **The authorizer is asked about each change a session's call makes.** A change, a pause and a resume ask `trigger.update`, and a delete `trigger.delete`, as the session's initiator with `org_id` of its context, about the stored trigger and with `origin`; a held one asks before it is held and again, with `confirmed_by`, after the allow, and a pause asks once, with none.

## v0.53.1 - 2026-10-23

### Fixed

- **An ended session stays ended.** An end that a runner's claim overtook, between the end route's read of the idle session and its append, ended the running session under the runner driving it, and the turn's status then left it idle again. The route now checks that the session is idle on the read its append follows, so such an end is `conflict` and the session runs on. A runner whose claim follows an end, by the route or the reaper, now appends nothing and lets the session go, where it appended `running` after the end and ran a turn. A title asked after a turn records nothing on a session that ended in the moment before the title read its log, where it appended the title after the end.

## v0.53.0 - 2026-10-23

### Changed

- **The blob route answers a kept file's own media type and its size.** `GET /v1/sessions/{id}/blobs/{digest}` labels a file that is no PNG, JPEG, GIF or WebP picture with the `media_type` its record gave it when it was kept, a message's attachment or a file an answer linked, in place of `application/octet-stream`, and a blob no event records with the type its leading bytes give. Every answer states the blob's length in `Content-Length`, a session without a folder of a files store included, and `HEAD` answers the headers without the bytes. Nothing else changes: a picture is still the only blob answered inline, and every other one is still an attachment under `nosniff` and the sandboxing policy, so a client that shows an HTML page or an SVG still draws it in a sandbox of its own. See `docs/reading-files.md`, which also says how a client learns which files a session's working directory holds.

## v0.52.0 - 2026-10-22

### Added

- **An agent can offer the person a routine, made only on their yes.** An agent whose manifest names the new `trigger` tool can list the triggers the person who started the session holds of it, and propose a routine on a schedule: a name, the ask each run makes, one sentence of why, a five-field cron of a minute and an hour on every day or on the days listed, and a time zone. Every proposal waits for that person in every approval mode, whatever an always-allow pattern, the progressive score or a decision service would answer, and plan mode blocks it. Only the person who started the session answers it: another sender's `user.tool_confirmation` of a `trigger` call is refused `forbidden`. On their allow the routine is applied as theirs, in the session's context, in the shape `latere.ai/x/sdk-go/agents/routine` writes, so every client reads it as a routine; a deny's `note` carries the person's words to the agent. The tool is offered by the hosted runner alone, never to a subagent's thread, and `topos run` refuses an agent that names it. See `docs/offers.md`.
- **A trigger says when a conversation made it.** `status.origin` names the session and the call that proposed it and who allowed it, and a later apply at the route keeps it.
- **The authorizer is asked about a trigger a session's call makes.** `trigger.create` and `trigger.update` asked for the tool carry `origin` (`kind`, `session_id`, `tool_use_id`, and `confirmed_by` once the person allowed the call), asked as the session's initiator with `org_id` of its context; the tool's list asks `trigger.list` naming `owner` and `agent`. A proposal the authorizer refuses is answered at once and asks nobody. An authorizer that reads no `origin` decides it as the owner's own apply, so roll the authorizer that holds `confirmed_by` to its owner first.
- **A tool can ask its service before a call is decided.** `tools.Prober`, beside `tools.Checker`, answers a call its service would refuse without recording an `agent.tool_use`, so no person is asked to approve a call bound to fail.

## v0.51.1 - 2026-10-22

### Fixed

- **A rollout's run no longer receives its initiator's accounts or context.** A session created with `rollout` was given what its initiator's own sessions are: their standing instructions, the context and repositories an allow attaches, and their connections, with the connection service's token in its sandbox. Now, whatever its allow names, it holds none of them, a send changes none of its connections, its sandbox mounts no Secret of the connection service, toposd mints it no token for that service (`run_isolated`), and its requests render none of them. Its network, its folder and the repositories its request names are unchanged.

## v0.51.0 - 2026-10-21

### Added

- **The built-in model catalog knows the current models.** It holds 291 models where it held 109: Claude Opus 5.5, Sonnet 5.5 and Haiku 5.5 and Claude Fable 5.1, each by the hosted Lux's name (`anthropic/claude-opus-5.5`) and by Anthropic's id (`claude-opus-5-5`), and every other model a hosted Lux sells today, with their prices, windows and output limits. An agent can name them on a direct Anthropic or OpenAI connection, where a model the catalog does not know is refused `model_unknown`. `tools/catalog` reads the new source with `-served`, a Lux door's model list saved to a file, and with `-previous` the catalog it replaces, so a model OpenRouter stops listing, `claude-sonnet-4` among them, keeps what it is known to take, images and thinking included.
- **Every message records the application it came from.** A sender the server records for a request carries `client`, the OAuth client of the token it verified (`client_id`, or `azp` where the token has none), written from the token and never from the body. A token that names no client writes none, so older events and such tokens read as before.
- **`GET /v1/sessions/activity` counts a day.** For one day in UTC, per sender, it answers how many messages a person sent from each client, how many sessions they started from each, and how many answers to their messages finished, counts only and never a word of what was said. It asks the new action `session.activity` once, with `day` and `sender`; without an authorizer an admin reads every sender and anyone their own. Postgres reads the day through two new indexes, built concurrently so a running installation's writes are not held while they build (migrations 0014 to 0017; a start stopped mid-build is recovered by forcing the version back, as spec 085 says). See `docs/activity.md`.

- **The deployment's start probe waits up to 15 minutes** for the store's migrations before the liveness probe applies, so a long index build is not killed.

### Fixed

- **A model the catalog does not name goes to its provider's door.** A model whose name begins with `anthropic/` or `openai/`, and that neither the catalog nor the agent gives a family, is spoken to on Lux's Anthropic door, or on the OpenAI door's Responses, in its own dialect, as a model the catalog names is. It went through the OpenAI door on Chat Completions, translated. A model of another provider still goes to the OpenAI door.
- **Built with Go 1.27.3,** which fixes GO-2026-6809, a flaw in the standard library's certificate verification that this code can reach.

## v0.50.2 - 2026-10-20

### Fixed

- **toposd exports its telemetry.** With `OTEL_EXPORTER_OTLP_ENDPOINT` set, the server and the runner role start the OpenTelemetry export at start: a span and the request metrics (`http.server.request.duration` and the body sizes) for every request on both listeners but the probes, the spans of every outbound call, the log, and the Go runtime's metrics. Before, the endpoint was read by nothing, so a deployment that set it exported nothing. The log on stderr keeps its text format.

## v0.50.1 - 2026-10-20

### Fixed

- **The API document describes a session's machine, its repositories and an event's payload.** A create's `machine` is `CreateSessionMachine`, `{kind, environment, workdir}`, and its `resources` a list of `CreateSessionRepository`, `{type, url, ref}`, where the document said any JSON value; a trigger envelope's `payload` is an object.
- **The document writes durations in words.** The `Idempotency-Key` header's replay window reads 24 hours, a computer's approval wait 2 minutes and a deletion's wait 17 seconds, where the document wrote 24h0m0s, 2m0s and 17s.

## v0.50.0 - 2026-10-20

### Added

- **The API document describes every body by a schema.** Each request and answer names a schema derived from the type the server encodes, so a generated client and a reader see every field, including those no example shows: a session's `metadata` and `trigger_id`, an `agent.tool_use`'s `client`, a `tool.result`'s `outcome` and `is_error`, and a `session.error`'s `retryable` and `detail`. `Event` lists one variant per event type with its payload's schema, and `SendEvent` one per type a person sends.
- **The document names the parameters every list and the stream read.** `limit` and `cursor` on every list that pages, `from_seq` on a session's events, the `Last-Event-ID` header a stream resumes after, and the `Idempotency-Key` header every POST reads, with its replay, its `idempotency_conflict` and the `conflict` a repeat gets while the first request is still being answered.

### Fixed

- **A session answers its folder once its working directory is kept there.** A session whose only files in its folder are the copy of its working directory, written at a turn's end under `work/`, now answers `folder {owner, path}`, where before it answered none until a message's attachment or a kept file was stored, so a caller reading what a run made found no folder to read it from.

## v0.49.0 - 2026-10-19

### Added

- **A session can be one run of a rollout.** A create's `rollout`, the caller's id for the rollout the session is one run of, is the session's for its whole life and is carried as `rollout` on `session.create`. The session list, the search and the summary leave a rollout's runs out unless their new `rollout` parameter names that rollout, or `any`; a list by agent, metadata, trigger or tree keeps the same default, so a person's lists hold their own conversations. Nobody attends a run, so a call that would wait for a person's confirmation is answered at once as denied, with a result that says so, and the run goes on to its end rather than going idle. A store of an earlier release gains the rollout column with migration 0013.
- **A create can name the model it runs.** A create's `model`, `{name, reasoning}` with either member or both, starts the session on that model and level from its first request in place of its agent's. `session.create` carries them as `model`, `reasoning` and `effort`, and an allow that names another model or another level refuses the create as `model_not_allowed` (403) rather than running a substitute.

## v0.48.0 - 2026-10-19

### Added

- **The task suite can run another harness prompt.** `TOPOS_TASKS_PROMPT`, `harness/<n>`, names the harness prompt version every run reads, and `TOPOS_TASKS_DEVICE=true` renders it as a session on a person's own computer reads it, on the host machine, so a prompt version is measured against the one before it on the same model; `tasks.Options` takes `PromptVersion` and `Device`, and the report names the prompt.

### Changed

- **A session on a person's computer runs `progressive` as `confirm`.** Where the session runs `progressive`, by its agent's manifest or a change of the mode, a call on a device is decided as in `confirm`: a call that reads runs, and any other, a write or a command included, asks unless `always_allow` names it or the person allowed a pattern for it earlier in the session with `remember`; `plan` still blocks a change, and a decision service's answer is a suggestion and no longer runs a call. Each call's `agent.tool_use` records `mode: confirm`. The session's policy keeps the mode it was given.
- **A trigger that names a person's computer is decided when it is applied.** When a trigger's `spec.session.machine` names a device, `trigger.create` and `trigger.update` carry `machine: device` with `environment` and `workdir` beside `name`, `agent` and `on`, the same flat fields `session.create` carries, so the authorizer can decide whether the person applying the trigger may use that computer before the trigger is stored, and not only at each firing. A trigger of the agent's machine carries none of them. An authorizer that does not read them decides as before.

## v0.47.0 - 2026-10-19

### Added

- **A session's title can follow its conversation.** An agent's `spec.title`, `{model}` with `model` optional, turns it on, and its latest version decides for every session it runs. After a turn a person started leaves the session idle, the harness asks a model whether the title still names what the conversation is about, reading the title it gave before, the person's latest six messages and the start of each answer, and keeps it or writes a new one as `session.title_changed` by `{subject: service:harness, kind: service}`, after the `model.request` of `purpose: title` that asked, in one batch that moves no `updated_at`. A title of at most 60 characters on one line is written; `KEEP`, an empty answer and the title the session has change nothing. No title is asked after a trigger's turn, a turn that ended the session or left a message waiting, once the budget is spent, or in a session a person renamed, a fork's copied rename included. Before the request toposd asks the authorizer `session.title` as the session's initiator, with `session_id`, `model`, the name the request asks for, `current_model`, `current_model_via`, `current_model_route` and `title`: an allow may name the model, and a route beside it, to run on; a deny, or an authorizer that does not know the action, keeps the title, so toposd may roll out first. An agent's title `model` that the installation does not run sends nothing, and never falls back to the session's model. A title that fails writes no `session.error` and changes no status; the runner logs it. The request counts toward the budget, is never streamed as deltas, and leaves `idle_seconds` alone. The whole title takes at most 15 seconds, and a message, a confirmation, an answer or an interrupt ends it at once, its request recorded as `canceled`, so the next turn starts straight after. A person's rename and an end of the session while the title is asked win. A response that spent its 64 tokens reasoning is asked once more with room to reason when the agent names its title `model`, and not on the session's own. A change of an agent's latest version applies within 30 seconds. A runner process asks the question over `POST /internal/v1/leases/{session}/title`.
- **The sessions an agent ran before are named once.** A minute after toposd starts its runners, it names the idle sessions of every agent whose latest version sets `spec.title` that hold a person's message and were never named, one at a time, two seconds apart, skipping a session another runner holds, and logs how many it named, kept, skipped and failed. A session whose title request failed, was refused or was cut short is tried again at the next start, and an agent whose latest version cannot be read is logged and left out.
- **A delete of an idle session waits for the runner that names it.** `DELETE /v1/sessions/{id}` of an idle session whose lease a runner holds, as one does while it asks for the session's title, tries again for at most 17 seconds and deletes it once the runner lets go; one held past that, or that started running, is `conflict` as before.
- **A task of the task suite can start in the middle of a conversation.** `task.yaml` takes `log`, a file of JSON lines of session events written into the session before the prompt, and `agent.title`, the agent's `spec.title`; a title `model` of `${MODEL}` is the run's own model; `check.yaml` takes `title: {matches, not_matching}`. The `titles` category holds `titles/moved` and `titles/aside`.
- **An agent can read back what its context let go.** An agent whose manifest names `history` holds a tool that reads its own conversation from the session's log as it was written: a range of events, `{"from", "to"}`; one call and its result, `{"tool_use_id"}`; or the events whose text holds some words, `{"query"}`, case ignored, at most 20 matches of up to 400 characters each. Each entry is headed by the event's number, its time and who wrote it; a cleared result and a summarized stretch read whole, and a redacted event reads as one line saying it was removed. An answer holds at most 32 KiB, and one that stops early ends with the input that reads on. A thread reads its own conversation alone, up to the call, and a subagent holds the tool when its parent does and its own tools do not leave it out. A call changes nothing, never starts the session's machine, and is allowed in every approval mode. For such an agent a cleared result names its call's id, a summary names the events it replaces, and a summary is asked with `compact/compact-v3`, which `context.compacted` records and which lets a long exact text be named rather than copied; an agent that does not name the tool reads every text as before. See [docs/reading-back.md](docs/reading-back.md).

### Changed

- **The person who started a session can bring a file someone shared with them.** A `source` whose `owner` is the space of the person who shared the file, their subject, is read in that space with the session's own token and named so on the send's `sources`, as any owner the starter names already was. Whether the read is allowed is decided outside the core: it needs a files service that names who holds a share on the file in its authorization question, and an authorizer that decides the session's read on the starter's own share; until both run, such a source is `attachment_source_not_found` as before. A sender who did not start the session still names a file of the session's own space alone.

## v0.46.0 - 2026-10-18

### Changed

- **A file of a person's own files a message names is read with a credential of the session's that the server alone holds.** The server reads a `source` at the files service with the session's token for `TOPOS_FILES_AUDIENCE` of the new workload `source`, asked once for the message, and never with the sender's token, which it no longer passes on; the runner protocol never answers that workload, and the identity provider must issue it. The authorizer decides each read as the person who started the session. A fork's message is read with the fork's own such token. The owner `me` is read as the sender's own space, and the message's record still names `me`. A sender who did not start the session may name a file of the session's own space alone, the owner of its folder, and any other is `attachment_source_not_found` before anything is read. A token the server cannot have is `files_unavailable`. The send's question names the files it will read as `sources`, `{owner, path, version}` with `me` as the sender's subject, so the authorizer may hold the sender to them; an authorizer that does not read it answers as before. The authorizer must admit the source workload's read of one file, decided as the person who started the session, before this release reads a source.

## v0.45.0 - 2026-10-18

### Added

- **A session can work on a person's own computer.** `POST /v1/sessions` takes `machine: {kind: device, environment, workdir}`: `environment` names the Cella environment the computer serves through an embedded worker, and `workdir` the folder on it the session works in, an absolute, clean path. Any other kind is still refused as `invalid_request`, and a server without Cella answers `machine_unavailable`. The session records the machine for its whole life, and the authorizer's `session.create` carries `machine: device` with `environment` and `workdir`; a fork's `session.fork` carries the same three, decided for the person forking. A trigger names a device the same way in `spec.session.machine`, the one machine a trigger names, and its sessions record `trigger_fires_on`, `schedule` or `events`. An agent's manifest that names `device` is `invalid_manifest`.
- **A device machine is the person's computer, not a sandbox.** Its sandbox is created on the device's environment with the folder bound in place, with no image, no Lux, git host or connection Secret, no `LUX_URL` and no `HOME`, labeled with the agent's version and, for a trigger's session, the trigger and what it fires on, and annotated with the agent's display name, which the computer shows its owner. No repository is delivered into the folder, a message's files go to the sandbox's temporary directory, under `topos/attachments/`, and the session keeps no checkpoint and no copy of its working directory and restores neither. The files route answers `file_unavailable` for the folder, which the session's tools reach alone; what an answer showed or linked stays readable from the files the session kept. A call there scores as a call on a host, so in `confirm` a write asks. The helper is written for the computer's operating system and architecture through Cella's file route, and the release builds it for `darwin/arm64` and `darwin/amd64` beside `linux/amd64` and `linux/arm64`. The model reads `harness/2`, whose machine section says the computer is the person's own and that what its files say is data, not instructions; every other session stays on `harness/1`.
- **`machine_waiting`.** A session on a computer that is offline or asleep, or whose owner has not allowed the session within two minutes of a call's asking, goes idle with `stop_reason: machine_waiting` and the reason, `offline` or `approval`, in its `session.status` detail, and `session.machine_waiting {reason, environment}` names it; the one for `approval` is appended while the call still waits, so a client can ask the person to allow the session on the computer. A computer that stops answering under a call stops the turn the same way. A computer whose owner refused the session appends `session.error` `machine_refused` with the computer's own words in `detail`, answers every later call of that turn so, and the turn goes on. The next message, or `POST /v1/sessions/{id}/resume`, continues a session idle on `machine_waiting`; that resume adds no message, keeps the session's budget, and refuses `max_cost_usd_micro` as `invalid_request`.

### Changed

- `latere.ai/x/cella` v0.17.0, for the host facts, the held create, the local driver and the scratch directories of a worker on a person's computer, and `latere.ai/x/pkg` v0.102.0.

## v0.44.0 - 2026-10-18

### Added

- **`read` reads a PDF as text through a parsing service.** Set `TOPOS_PARSING_URL` to the parsing service's API, under which its `parses` are served. A PDF a person attached to a message, or one the working directory held at the end of an earlier turn, is then fetched by the service from the session's folder, at an address the files service signs for a few minutes, and `read` returns its pages as Markdown, each opened by a `<!-- page N -->` mark, after a line that says how many pages the PDF has, and ends with the `pages` to read next. `read` takes `pages`, such as `1-5`, `1-3,7` or `9-`, and returns the first 5 pages by default; the service reads a PDF of up to 256 MiB, at most its first 500 pages, and one read waits at most 5 minutes. The parse is kept for the session, so reading other pages of the same PDF, in the same turn or a later one, reaches no model and costs nothing. What the service's model reads cost is priced at the model's figures and carried as `cost_usd_micro` on the step's `tool.result`, which the session's spend counts; a PDF read from its own text costs 0. The `tool.result`'s meta names the parse, `{id, pages, more, shown}`, for a client to show. A PDF the service could not read, a refusal, a failed parse, a timeout, or a PDF the session's folder does not hold, is the binary answer as before with a sentence saying why, and never fails the turn. Each request carries the session's own token for the audience `lectio`, which the identity provider must issue and the authorizer admit. Unset, or for a session without a folder, `read` answers a PDF as a binary file and offers no `pages`.

### Changed

- **A message can bring a file of up to 256 MiB from the person's own files.** A `source` the files API answers with a redirect, as it answers a file past its inline bound (16 MiB by default) and a public file, is now read at the https address the redirect names, without the client's token, which never leaves the files API; this was `attachment_too_large`. A redirect to an address that is not https, and a second redirect, are `files_unavailable`. A file may be up to 256 MiB, and a message's sources up to 512 MiB together, in place of 16 MiB and 32 MiB; past them a source is `attachment_too_large`, as before. The server holds such a file on disk while the message is checked, not in memory, under `spool/` of `TOPOS_DATA_DIR`, which must be writable, and at most 2 GiB of them at once; a message that would pass that is `files_unavailable` and may be sent again. A program that embeds the server names the directory in `Files.Spool`, and its `Files.Source` answers the file as a stream, `*filestore.Source`.
- **A file delivered to a Cella sandbox is streamed.** A message's file past 1 MiB, and a file of the working directory restored from the session's folder, is written into the sandbox as it is read, so the runner's memory does not grow with the file.
- `latere.ai/x/pkg` v0.101.0: an image a tool returns, such as a PNG `read` opens, now reaches a model on the OpenAI Chat Completions and Responses dialects.

### Fixed

- **A fork's message that keeps a file of the session it forks does not read it again at its source.** A file named by its blob kept the source its record named and was read there once more, so a fork whose original file had since moved, or that the sender could no longer read, was refused `attachment_source_not_found`.

## v0.43.0 - 2026-10-18

### Added

- **A message can bring a file from the person's own files.** A file of a message's `attachments` may name `source`, `{"owner": "me", "path": "files/Reports/q3.csv", "version": 3}`, in place of its `data`, on a send and on a fork's message. The server reads the file when the message is sent, at the files API of the session's folder, with the token the client sent the message with, and keeps it in the session's folder as if its bytes had been sent; its record names the `source`. `version` is optional. The files API must accept the token a client sends to the core. A file the files API answers 401, 403 or 404 for is `attachment_source_not_found` (404), one answer for the three, so the answer does not say whether the file exists; a file past 16 MiB, a public file, which the files API answers with a redirect, and sources past 32 MiB together in one message are `attachment_too_large`; any other failure is `files_unavailable`. A session without a folder refuses a source as `invalid_request`. A source counts among the message's `attachments` in the authorizer's question. A program that embeds the server sets `Files.Source`; toposd sets it whenever `TOPOS_FILES_AUDIENCE` is set.
- **The model reads a short text file without starting a machine.** A text file of a person's message, UTF-8 and at most 64 KiB, of a text type such as CSV, Markdown, JSON or YAML or with such a name, is recorded `inline: true`, up to 192 KiB of such files a message. Every model request then carries the file's text after the line that names it, read from where the file is kept and never written into the log, so a question about a short file is answered without the session's sandbox starting. A file sent before this release is not marked, and the model reads it from the machine as before.
- **`read` returns the text of Word, Excel and PowerPoint files.** A `.docx` is read as its paragraphs, an `.xlsx` as each sheet's rows with their cells separated by tabs, and a `.pptx` as each slide's text, and so are their OpenDocument counterparts `.odt`, `.ods` and `.odp`, numbered and paged like any text file. A damaged one, one past 32 MiB, and one whose parts inflate past 64 MiB together are answered as a binary file, as before, and the text read of one document is at most 8 MiB. A PDF is still answered as a binary file. Version 2 of the tool's description says so.

## v0.42.1 - 2026-10-17

### Fixed

- **A session on a model that several providers serve keeps reading its prompt cache.** Through a gateway such as OpenRouter, an open-weights model like Gemma, Qwen or DeepSeek is served by many providers, and each turn of a session could land on another one, whose cache held nothing; over the two weeks before this release, Gemma sessions read 28 percent of their prompt tokens from the cache. Every model request now carries the session's cache key in the `X-Session-Id` header, by which the gateway keeps the session on one provider from its first turn. An endpoint that does not know the header ignores it.
- **A turn to a Qwen model reached on Chat Completions reads the conversation from the prompt cache.** Alibaba, which serves Qwen models, caches only what is marked, so its turns carried no cache breakpoints and paid full input price; the breakpoints are now sent as `cache_control`, as an Anthropic model's are. Other vendors' models get no marks, since their providers cache a repeated prefix on their own.

## v0.42.0 - 2026-10-17

### Added

- **A session a key's fire starts names the key.** When a trigger is fired with a token minted from a key, a personal access token or a service account's key, the `session.create` its firing asks carries `fired_by`, that token's `token_use` and `authorization_details`, so an installation's authorizer can hold the session to the key's grants as it holds a session the key starts itself. The create is still asked as the trigger's owner, so a key that grants only `trigger.fire` keeps firing. The key is kept with the firing, so a firing held until its session stopped waiting for a person, and a failed firing fired again, still name the key that fired them when their session starts. A person's own sign-in and a schedule name no key. Servers on Postgres apply migration `0012_firing_fired_by`, which adds one column to `trigger_firings`.

### Fixed

- **A turn to an Anthropic model reached on Chat Completions reads the conversation from the prompt cache.** A model a Lux installation names under the `anthropic` vendor and serves through a gateway such as OpenRouter is reached through Lux's OpenAI door; its turns carried no cache breakpoints, so every turn paid full input price for the whole conversation. The breakpoints after the system prompt and after the latest user turns are now sent as `cache_control`, and the cache writes the gateway reports are counted as cache writes.

## v0.41.0 - 2026-10-15

### Changed

- **A service kept apart from Google in a conversation is no longer reported as disconnected.** When the connection service refuses a tool call with `409 connection_not_connected` and the reason `kept_apart_from_google`, the call's result tells the model that this conversation cannot use the service and a new conversation can, rather than asking the person to connect it again. The `connection.requested` event still follows, with that reason, so a client can show it without a connect action. The connections block (version 5) tells the model the same for a request from the sandbox that meets the reason.

## v0.40.0 - 2026-10-14

### Added

- **A connection can keep some of an MCP server's tools from the model.** An installation's authorizer may name `hidden` on an `mcp` connection, globs over the server's tool names with the rules and bounds of `read_only`. A tool one matches is never offered to the model, so it no longer tries a call the connection service refuses every time, and a call of its name is answered as a call of a tool the session does not have, without reaching the service. A send that changes it appends `session.connections_changed`, as a change of any other member does.

## v0.39.0 - 2026-10-14

### Added

- **One owner's triggers and agents, and an agent deleted.** `GET /v1/triggers` and `GET /v1/agents` take `owner=<subject>` and list that owner's objects where the installation's authorizer allows it, so a service account an installation trusts can find what a closed account kept. `DELETE /v1/agents/{ref}` deletes an agent with every version once it has no session left, and archives and disables its identity at the identity provider; an identity the provider already deleted with its owner no longer stops the delete.

## v0.38.2 - 2026-10-14

### Added

- **A connection can tell the model more about its API.** An installation may give each connected service a short note, such as how a document's text is read or where an upload goes, and the model is told it beside the service's example call, with the connection's own variable written in. Version 4 of the connections block carries it.

### Fixed

- On a macOS server with `TOPOS_HOST_SESSIONS=on`, a session's command
  that checks its output before it writes to it, `cat`, `dd` and `node`
  among them, no longer fails. A command's output goes to a log
  beside the session's directory, inside the data directory the sandbox
  denies to every command, and Seatbelt refuses an `fstat` of an open
  file the command may not read: `cat` answered `stdout: Operation not
  permitted` and `node` aborted as it started. Each command may now read
  the log of its own output, which it still cannot write by its path,
  and no other command's. A Linux server was not affected, since
  Bubblewrap hides the path and leaves the open file as it was.

## v0.38.1 - 2026-10-14

### Fixed

- **Commands installed in a hosted sandbox can be run.** In a sandbox
  whose image's own home is read-only, `pip install httpie` succeeded
  but `http` was "not found", and `npm install -g` failed because npm
  could not write its global directory. Every command and background
  job now finds the commands `pip` installs for the user, in
  `$HOME/.local/bin`. When the image's npm global directory is
  read-only, npm installs into `$HOME/.npm-global` instead, and its
  commands are found by name too. The commands and packages the image
  put in its own npm directory stay available there, so
  `npx playwright` still answers at once with the network off, and
  `npm link playwright` still works in a project. A sandbox whose npm
  directory can be written keeps using it.

## v0.38.0 - 2026-10-14

### Added

- **A session with a folder keeps its working directory there.** A
  session whose authorizer names a folder of the files store now keeps
  the latest state of its working directory in that folder, as plain
  files under `work/<path>`, written at the end of each turn that ran a
  call and changed files, with the session's own token. Only what
  changed is written, and a file the directory no longer holds is
  deleted. Installed dependencies and caches (`node_modules`, `.venv`,
  `__pycache__`, `target` and the like), every `.git` directory, the
  credential deny-list, and files over 32 MiB are left out; a build's
  output is kept. One turn end writes at most 1,000 files and 64 MiB,
  the copy holds at most 5,000 files and 256 MiB, and what does not fit
  is written by a later turn end. A copy that cannot be written is
  recorded and never fails the turn.
  - A new `work.saved` event records what each turn end wrote, removed
    and left out, and is the record a restore and a fork read: the
    session's token may not list its folder.
  - A machine made new for such a session, a lost or expired sandbox's
    successor or a fork's first sandbox, is restored from the copy
    before its first call, then from the conversation's record for what
    the copy does not hold, and is recorded as `session.machine` reason
    `restored` with `restore {source: "folder", copy: {...}}`. The
    model is told what came back from where, and to reinstall
    dependencies from the project's manifests
    (`transcript/machine-work-restored-v1`).
  - A fork copies its parent's `work/` into its own folder on the
    server, and starts from the parent's latest working directory.
  - A Cella sandbox walks and hashes its working directory with the
    helper, where the files are.
  - For operators: the files are overwritten in place and deleted with
    `permanent=1`. An authorizer that keeps versions of a write in a
    session's folder keeps one for each overwrite under `work/`; answer
    `versions: false` there to keep the latest state alone.

### Changed

- **A redaction deletes the file from the session's folder.** Redacting
  a message or an answer whose file is in the session's folder deleted
  its digest from the session's reads and left the file in the folder.
  The file is now deleted from the folder for good, with the session's
  own token, once no live event names it; a file another answer of the
  turn still links, and one a fork's copied message still names, stays.
  A delete the files service refuses is logged, and the redaction
  stands.
- **`harness.Files` has two more methods.** A program that embeds the
  harness and passes its own files store in `harness.Config.Files` adds
  `Overwrite`, a write in place of what a path holds, and `Delete`, a
  removal for good that answers no error for a path already empty.

## v0.37.0 - 2026-10-13

### Changed

- **The agent is told plainly that it can use a person's connected
  services.** The connections block opened with "this machine holds
  none of them", which a small model read as having no access: asked to
  summarize the person's mail, with the mail service connected, it said
  it could not reach it and asked the person to connect it again. The
  block now says the connected services can be used now, with no sign-in
  or key, and gives each `http` connection the account it acts as, the
  service's own API base URL its variable stands in for, and one
  documented call written whole with `curl`, the header and the
  variable, version prefix included; it says an error other than
  `connection_not_connected` is about the request, not the connection
  (`context/connections-v3`).
- **A connection may name its `account`, `api_url` and `api_example`.**
  An authorizer can name the account a connection acts as, and, for an
  `http` connection, the service's own API base URL and the path of one
  documented call, which the block names. All are optional; an available
  connection names none of them.

## v0.36.1 - 2026-10-13

### Fixed

- **A background job's exit line comes after its files are gone.** The
  host machine wrote a job's `exited with code` line before it removed
  the job's script and input files, so a reader that waited for the line
  could still find them.

- **A connection service on the model gateway's public host no longer
  stops every hosted session from opening its machine.** With
  `TOPOS_CONNECTIONS_URL` on the host the model gateway publishes its
  doors under, while `TOPOS_MODELS_URL` named another address of the
  gateway, the configuration check passed, and Cella then refused every
  sandbox's create, since the sandbox's model key and its connections
  token were scoped to one host: no hosted session could open its
  machine. Serve and the runner now refuse to start on that
  configuration, naming both addresses and their host. A sandbox whose
  connections token would still share a host with another of its
  secrets, or that Cella refuses for it, opens without it: the server
  logs one error naming both hosts, and the session works on without its
  `http` connections, which the agent is then not told of. Its `mcp`
  connections are unaffected.

## v0.36.0 - 2026-10-13

### Changed

- **One `publish` call can put a site online and set who can open it.**
  A call that named `access` beside `path` or `release` was refused
  (`access_alone`) with the advice to make two calls. Some models sent
  the same refused call again instead, and the site was never
  published. Such a call now publishes or releases first, exactly as it
  would without `access`, then changes who can open the app it
  published to. The person approves it once; its `agent.tool_use`
  reason is `publishes and changes who can open the app, which a person
  approves every time`.

  - Its result is the publish's: `meta.publish` keeps the `status`,
    `commit`, `folder` and `release`, with the `visibility` the app has
    after the change, and the text says both.
  - When the publish fails, the result is that failure, and who can
    open the app is not changed.
  - When the change fails after the publish succeeded, the result is an
    error whose `meta.publish` still records the publish, with the
    visibility the app still has and the host's refusal in `error`
    beside the `status`. The text tells the model that the site is
    published, why who can open it was not changed, and not to publish
    again.
  - A call with `access` and neither `path` nor `release` works as
    before.

  The tool's description is `tools/publish-v6`: it tells the model to
  set `access` on the call that publishes a site the person already
  asked to share or keep private. The new result text is
  `results/publish/access-not-changed-v1`. No call is refused
  `access_alone` any more, and the Go constants
  `session.PublishAccessAlone` and `prompts.PublishAccessAlone` are
  removed; results recorded by earlier releases may still carry the
  code.

### Fixed

- **A turn no longer loops on a call that keeps failing the same way.**
  A model that sent a call again unchanged after it failed got the same
  error back and could keep sending it, each time a whole model request
  over a growing context, until the session's budget ran out. A call
  made again with the same tool and the same input, and answered with
  the same error, now carries a note after the error: repeating it
  cannot succeed, so change the call or tell the person what is
  blocking it. After 3 such repeats in one turn, the turn ends once the
  step's calls are answered: the session goes idle with stop reason
  `error` and a `session.error` with the new code `repeated_call`, whose
  message tells the person the agent kept repeating an action that
  failed and asks what to do next. Each subagent counts its own calls,
  and its turn ends on its own. A call that succeeds, or whose input or
  error changes, is never counted, so polling a job's log or running a
  command again after a change works as before.

### Upgrading

- Roll this release after any client that shows a waiting `publish`
  call with `access` as a change of who can open the app and nothing
  else. Such a call may now also name `path` or `release`, and a client
  that asks it as a change of access alone would have the person
  approve less than what runs. A client tells it by `access` beside
  `path` or `release` in the input of its `agent.tool_use`, or by the
  reason above, and its result carries the publish's `status`.
- A client that shows a `session.error` by its code meets a new one,
  `repeated_call`. Show its `message`; the person's next message starts
  a new turn as after any error.

## v0.35.0 - 2026-10-13

### Added

- **An agent can ask the person to connect a service they have not
  connected yet.** The authorizer names such a service in the allow as
  `{"name", "kind", "title", "state": "available"}`. The model sees it
  listed apart from the connected ones and calls `request_connection`
  with its name and why, which appends `connection.requested` for a
  client to show as "connect this service", and ends its turn saying
  what it will do once connected.

- **A conversation's files can live in the installation's files
  service.** The files a person attaches to a message, and the files an
  answer shows or links, were kept in the session's own storage, where
  nobody found them outside the answer that named them. An installation
  that runs an Arca-compatible files service can now keep them there
  instead, in a folder of the conversation's own, where they count and
  are found like any other file of that space. The authorizer decides
  per session: its allow of `session.create` or `session.fork` names
  `folder: {"url", "owner", "path"}`, the files API's root, the space
  and the folder. With a folder:

  - A message's file is written to `attachments/<event id>/<name>` and
    an answer's file to `results/<turn>/<path>`, each once, with the
    session's own token. The record gains `ref`, the file's path in the
    folder, beside `blob`, its digest.
  - A file an answer links is no longer held to 16 MiB, or to 32 MiB
    for one answer's files together; the files service's own bound
    applies, and a file it refuses is skipped `too_large` or
    `unavailable` without failing the turn. A message whose file it
    refuses is refused, `attachment_too_large` or the new
    `files_unavailable`, before it is appended.
  - `GET /v1/sessions/{id}/blobs/{digest}` still answers by digest. A
    file of the folder is streamed from the files service with its
    `Content-Length`, read with toposd's own token, so a conversation
    stays readable after it ends.
  - The session answers `folder: {"owner", "path"}` from the first file
    stored there.
  - A fork copies the folders its copied history and its message name
    into its own folder, on the server.

  A session whose allow names no folder, and every session of an
  installation that configures none, keeps its files as before.

### Upgrading

- Set `TOPOS_FILES_AUDIENCE` to the audience your identity provider
  mints tokens for the files service under, on the server and on every
  runner, before your authorizer names a folder. It needs
  `TOPOS_IDENTITY_URL`, and the identity provider must let toposd's own
  client ask a token for that audience. A server without it refuses an
  allow that names a folder as `authorizer_unavailable`, and a runner
  without it fails the turn of such a session with `files_unavailable`.
- The files of sessions that exist stay where they are. A redaction no
  longer answers a file of a folder by its digest, and the file stays in
  the folder until the installation removes the folder.

## v0.34.0 - 2026-10-13

### Added

- **A session reaches the services a person connected, without their
  credentials.** An installation that keeps its people's connections in
  a connection service of its own names each session's connections in
  its authorizer's allow of the create, the fork and every send, so a
  service connected in the middle of a conversation is there from the
  next message. A connection of kind `http`, such as a code host, is
  reached from the sandbox: `git clone` of the usual URL, fetches and
  pushes go through the connection service, and the service's API is at
  `$<NAME>_API_URL` with `$CONNECTIONS_TOKEN` as the bearer, a
  placeholder Cella's egress gateway swaps for the session's token. A
  connection of kind `mcp` is a remote MCP server whose tools the model
  is offered as `mcp__<name>__<tool>`, read or external by the
  allow's `read_only`, and asked about as any tool is. The model is told
  each connection and how to use it, and a connection the person has to
  connect again fails its call with a result the model relays and a
  `connection.requested` event a client shows as "connect this service".
  Set `TOPOS_CONNECTIONS_URL` and `TOPOS_CONNECTIONS_AUDIENCE`, and let
  the identity provider mint those audiences for the runner. See
  [Connections](docs/connections.md).
- **The sessions list filters by the trigger that started a session.**
  `GET /v1/sessions?trigger=<id>` lists only the sessions that trigger
  started, newest first, a page at a time, beside every other filter
  and under `group=tree`, where a fork of a run, which no trigger
  started, leaves its tree to the run. `GET /v1/sessions/summary` takes
  the same filter and counts those sessions by status. A client can now
  show a routine's runs, and how many there are, without paging through
  every session. The filter narrows only what the caller may list: the
  id of another context's trigger, or of no trigger, lists none of the
  caller's sessions. A value that is not a trigger's id is
  `invalid_request`. The search route does not take it.

### Fixed

- **A conversation keeps publishing to its app after the app is
  renamed.** `publish` remembered the app a conversation published to
  by its slug alone. Once the app's owner renamed it, the old slug
  answered not found, and the next publish created a second app beside
  the first: the conversation forked the site without anyone asking.
  Every publish result now records the app's id as `app_id` beside its
  slug, and every later call finds the app by that id before its slug:
  it publishes to the renamed app, releases it and changes who can open
  it, and its result names the new slug and address. An app a session
  was started on is found by its id from the start when the
  authorizer's allow names it, as `repositories[].app.id` beside the
  app's slug, so an app renamed before the session first reads it is
  still found, and its checkout still starts at the commit the app
  serves. An allow that names no id attaches the app as before, and the
  session finds it by its id once a result of the session read it. The
  slug the conversation knew before still names the app in a call. A
  conversation whose results predate `app_id` finds its app by its
  slug, as before.
- **A file rebuilt into a new machine carries the time it was written
  back.** When a lost machine's files were rebuilt from the log into a
  new one, the archive they arrived in carried no time, so every file
  and folder was dated 1 January 1970, and a tool that refuses such a
  date failed on them, as Python's `zipfile` does on one before 1980.
  Each now carries the time the rebuild wrote it.

### Upgrading

- A client that shows a conversation's app reads `meta.publish.app_id`
  beside `app`, and keeps an app across results by `app_id` where one
  is recorded, since `app` changes when the app's owner renames it.
- Finding an app by its id needs an app host that takes an app's id in
  the slug's place on every app route, answering an id it does not know
  `404`. An older host answers every id `404`, and the tool then reads
  the app by its slug once, as before, at the cost of one more request
  a call; a rename there still loses the app. The two roll in either
  order; a rename is survived once both run.
- An authorizer may name an attached app's id in its allow as
  `repositories[].app.id`: at most 64 ASCII letters, digits, hyphens
  and underscores, starting with a letter or a digit. An allow whose id
  breaks that is refused as an allow toposd cannot read. An earlier
  release ignores the member, so an authorizer may send it before this
  release rolls.
- `runner.Options.Apps`, the reader of the commits an app is served at,
  is given the attached app, `session.ResourceApp`, in place of its
  slug, so a reader finds the app by its id.
- The Postgres store adds the column `sessions.trigger_id`, filled from
  each session's body, and an index on it (migration `0010`). While an
  earlier release runs beside this one, a session it creates enters a
  list by trigger once a batch of its log saves its header, which its
  first turn does.

## v0.33.0 - 2026-10-12

### Changed

- **A published site gets a name and an address of its own.** The first
  `publish` of a session created its app with the session's title as
  the app's name, and the app host made the app's address from the
  name. A title says what the conversation was about, often the
  person's first message, so a site came out named by a sentence, at an
  address made of that sentence's first words, and a site whose name is
  in a script an address holds none of got an address the host
  generated. `publish` now takes two more inputs, and the tool's
  description tells the model to give both the first time it publishes
  a site:

  - `name`, a short name for the site as a person would call it, two to
    four words in the language of the site, such as "Tide Tables" or
    "潮汐表". The call that creates the app sends it as the app's name,
    put on one line and cut to 80 characters. A call that gives none,
    or whitespace alone, creates the app with the session's title, as
    before.
  - `address`, a short address in Latin letters whatever the site's
    language, such as `tide-tables`: at most 40 characters, in words of
    lowercase letters and digits joined by single hyphens. The call
    that creates the app sends it as the create's `slug`. An address of
    any other form is left out, and the host makes the address from the
    name, as it does for a call that gives none.

  An address the host cannot give does not fail the publish. When the
  host refuses the create for its slug, with `invalid_slug`,
  `slug_taken`, `slug_reserved`, `slug_held` or
  `repository_name_taken`, the tool sends the create once more without
  one, the host makes the address, and the same call goes on: the
  person is asked nothing more. Any other refusal of a create is the
  call's error result, as before.

  Both inputs are read by the call that creates the app and by no
  other: a later publish or release, a publish of an app the
  installation attached, and a call with `access` ignore them, and none
  is refused for them. The tool renames no app and moves none to
  another address; a person changes theirs at the app host. Every
  result names the app in `meta.publish` with the `name`, the slug and
  the `url` the host answers, as before, so the address the agent
  passes on is the one that exists, which may differ from the one it
  asked for.

  The tool's description is `tools/publish-v5`; `tools/publish-v4`
  stays as it was.

### Upgrading

- Nothing has to change for an agent to name its sites. An agent
  definition names `publish` in `spec.tools` and pins no version of the
  tool's description, so every agent that holds the tool reads
  `tools/publish-v5` and is offered `name` and `address` from this
  release on. An agent whose own instructions tell it how to name what
  it publishes keeps working either way.
- An app host takes `slug` beside `name` in `POST {root}/apps`, gives
  the app that slug, and refuses one it cannot give with one of the
  five codes above. A host that makes every address itself may ignore
  the member. A host that refuses a body with a member it does not
  know, with another code, fails the first publish of every call that
  asks for an address: roll such a host's reading of `slug` before this
  release. An app that exists keeps its name and its address.
- An installation whose authorizer decides an app's create by what the
  create names is asked about a create with a slug by its app host as
  that host asks it. Where it refuses a session such a create, the
  call's error result carries the refusal and no app is created, so
  check the rule before this release.
- A client that puts a waiting `publish` call to the person in words of
  its own may show `name` and `address`, which the call that creates
  the app carries in its input. One that does not shows what it showed
  before.

## v0.32.0 - 2026-10-12

### Added

- A tool can hold a call for a person. A tool that implements
  `tools.Confirmer` names, from a call's input, a call a person approves
  every time, with the reason its `agent.tool_use` records. Such a call
  waits for a person wherever an `alwaysAllow` pattern, an answer a
  person gave earlier with `remember`, the `progressive` mode's score or
  a decision service would have run it without asking; `plan` blocks it
  as before. For a program that embeds the harness: `harness.Call` has
  `Confirm`, the reason, which `harness.Rules` and `Policy.Settle`
  answer with an ask, and the harness applies the hold after any
  `Decider`.

### Changed

- **The publish tool no longer asks for a public app, and opens an app
  to others once the person approves.** The first `publish` of a
  session created its app with `visibility: public`, so the app was
  open to anyone before its person chose that. The create now names no
  visibility, and who may open a new app is the app host's default. A
  `publish` result records the app's `visibility` in `meta.publish`
  when the host names one, so a client can show who can open the site,
  and the result's text tells the model when the app is private: the
  agent then says that the app is online and private, and offers to
  open it to anyone with the link, instead of handing out an address
  others cannot open.

  `publish` takes `access` for that: `link` lets anyone who has the
  app's address open it without signing in, and `private` only the
  person and the people they share it with. A call with `access`
  changes who can open one app and does nothing else. It reads the app
  and asks the host, `PATCH {root}/apps/{slug}`, for `unlisted` or
  `private`; it never asks for `public`, and sends nothing when the app
  is as asked already, an `unlisted` or a `public` app for `link`. It
  takes its app as a release that names no folder does, and is refused
  before it is asked when it also names `path` or `release`
  (`access_alone`) or has no app to change (`app_required`). Every such
  call waits for the person, whatever the agent's `alwaysAllow` or an
  earlier answer would run without asking, so a session nobody attends
  opens nothing. Its result is `ok` and names the app with the
  visibility the host answers and no `status`. A change the host
  refuses is an error result that tells the model why, and for an
  organization's app that only its owners and admins change who can
  open it; `meta.publish.error` then carries the host's code, its
  sentence and the new member `reason`, the token the host's refusal
  names in `details.reason`.

  The tool's description is `tools/publish-v4`, the ready and released
  results are `results/publish/ready-v2` and `released-v2`, and the
  results of a call with `access` are `results/publish/access-changed-v1`,
  `access-refused-v1`, `access-alone-v1` and `access-app-v1`; the earlier
  versions stay as they were. An app host that keeps no visibility is
  unaffected until a call asks it to change one.

### Upgrading

- Roll this release after any client that frames a published site has
  learned to show a private one another way. A session's new app now
  follows the app host's default, and where that is private, a frame of
  the app ends at the host's sign-in page instead of the site. A client
  reads `visibility` in `meta.publish` to tell. An app host whose
  default is open, and the app host's own release order, need no care:
  the create asks for nothing either way.
- Roll this release after any client that asks a waiting `publish` call
  in words of its own has learned to read the call's input. A call with
  `access` waits like a preview or a release and is neither: a client
  that asks it as putting a folder online would have the person approve
  something else than what runs. The call has `access` in the input of
  its `agent.tool_use` and neither `path` nor `release`, and its result
  has `meta.publish` with no `status`.
- An installation whose authorizer decides a session's requests at the
  app host rolls the rule that lets a session change who can open an
  app before this release, and gives the agents that should do it the
  permission the rule reads. In another order nothing is opened: the
  host refuses the call, and the model tells the person to change it
  from the app's sharing.
- An app host answers `PATCH {root}/apps/{slug}` with
  `{"visibility": "unlisted"}` or `{"visibility": "private"}` with the
  app and its `visibility` after the change. A host that does not is
  answered as a failed change, and nothing else of the tool changes.

## v0.31.0 - 2026-10-12

### Fixed

- A session's sandbox that failed is started again, not replaced. A
  sandbox whose container was killed, for memory for example, read
  `Failed`, and the next turn deleted it and created an empty one: the
  delete removed the workspace, so a session with no repository lost
  every file it had made. The workspace was intact until that delete.
  An open and a call that find the sandbox `Failed` now ask Cella to
  start it, wait until it runs, put the helper back, and go on in the
  same sandbox with the same files. A Cella core after v0.12.0 takes
  that start for a failure that kept its workspace. A core that answers
  `phase_conflict`, an older one or any core for a failure with no
  workspace, is asked twice, and only then is the sandbox replaced at an
  open; a call never deletes one. A sandbox that fails again right after
  a start gets one start from each call that finds it failed, and that
  call fails in words that say so, so nothing spins and nothing is
  deleted.
- A command that was running when its machine was killed says what
  happened. Its result read "The command did not start: machine: the
  command's session ended before its exit (exit code 137)". It now holds
  what the command wrote, that it did not finish, that the machine ran
  out of memory when Cella said so and never otherwise, and where the
  machine stands: running again with the files of the working directory
  kept and every process gone, so the command can be run again; lost; or
  not running yet, with the reason and nothing deleted. A command that
  ends with exit code 137 on a machine that runs on is told what the
  code means, with no cause claimed.
- A machine that replaced a lost one is recorded as one. The log named
  it `session.machine` with reason `handoff` and appended no error, so
  neither the model nor a client was told the files were gone. A machine
  made new in the place of the session's last is now reason `replaced`,
  followed by `session.error` `machine_lost` ("The session's machine was
  lost and replaced with a new one. The files that were in its working
  directory are gone."), and the model reads at that point that the
  files are gone and that the person is to be told.
- A command on a lost machine no longer appends `session.error`
  `network_unavailable` ("The connections the command made could not be
  read."): a machine that is lost, or not running, has no connections to
  read.

### Added

- A working directory is rebuilt from the session's log. A session with
  no repository has no checkpoint, so its files lived in its sandbox
  alone: a sandbox the core could not start again, one whose lifetime
  ended, and a fork all left the person with the conversation and none
  of the files. When a machine's open made it new for a session that
  had one, or for a fork whose fork point kept no checkpoint, the runner
  now replays the log into it before the turn's first tool runs: each
  successful `write`, each `edit` applied in order, each file a
  `files.kept` stored, and each file a `read` returned in full, which is
  how a file a command made and the agent then read comes back. Each
  file is held to the last hash the log recorded for it. A file a
  command changed after the log last held it is put back and counted as
  possibly out of date; a file whose edit no longer applies, and a file
  the log shows only in part, is left out and named; a redacted event
  gives nothing. Files that only a command made, generated data,
  downloads, installed packages and build output, are not in the log
  and do not come back. A rebuild holds at most 2,000 files
  (`tools.MaxRebuiltFiles`) of 64 MiB together
  (`tools.MaxRebuiltBytes`), written into the machine as one archive.
  The machine is recorded as `session.machine` with reason `restored`
  and the new member `restore {source: "log", files, bytes, unverified,
  omitted, commands, paths, unverified_paths, omitted_files}`, with no
  `machine_lost`, and the model reads there which files are back, which
  may be out of date, which are missing, and that nothing a command made
  is back. A fork whose checkpoint the runner can reach is restored from
  it as before. A machine that does not say it is new, such as a
  directory on a person's host, is never written over.
- `session.machine` has the reason `restarted`: the machine the session
  had stopped on a failure and runs again, with the files of its working
  directory kept and every process that ran on it gone. The record
  restates the machine's context, instruction files and skills, and
  carries the new member `cause`, `out_of_memory` when the machine is
  known to have stopped for memory and absent otherwise. It is appended
  beside the call that met the restart, and the model reads there that
  the machine restarted with its files.

### Upgrading

- Starting a failed sandbox needs a Cella core after v0.12.0. Against an
  older core this release behaves as before, a failed sandbox is
  replaced at the next turn, except that the loss is now recorded and
  said.
- A client that draws the log tells the cases apart by `reason`:
  `restarted` keeps the files, `restored` with `restore` put back the
  ones the log holds and names the rest, and `replaced` lost them all
  and comes with `machine_lost`. A client that lists the reasons it
  knows should read `restarted` as a machine that is still the
  session's. A fork whose checkpoint was missing now records
  `checkpoint_missing` with a second sentence when its directory was
  rebuilt from the log instead.
- For a program that embeds the machines: `machine.Recovering` is what a
  machine reports its open and its restarts through, `machine.ExecResult`
  has `Stopped` for a command whose machine stopped under it,
  `cella.ErrLost` is `machine.ErrLost`, and a `cella.Machine` no longer
  deletes a failed sandbox unless Cella refuses to start it.

## v0.30.0 - 2026-10-11

### Added

- An answer's linked files are kept. An agent that made a report or an
  archive with a command and linked it in its answer, as in
  `[Download the report](report.pdf)`, named the file nowhere in the
  session's log: a client had nothing behind the link, a copy of the
  conversation held none of the file, and the file was gone with the
  machine. Once a step of the session's own thread is committed, each
  local file its `agent.message` links, by a Markdown link or an HTML
  `a` element, is read from the open machine, stored as a blob of the
  session, and recorded in the message's `files.kept` beside the images
  it shows. A destination is local when it has no scheme, the `file:`
  scheme, or the prefix `sandbox:` some models write before a path, and
  lies inside the working directory. A linked file's entry has the
  members of an image's: `path` as the message wrote it, `resolved`,
  `blob`, `media_type` by the file name's extension and otherwise by its
  leading bytes, `size`, and `width` and `height` 0. A message keeps its
  first 16 links (`harness.MaxKeptFiles`), each a regular file of any
  type of at most 16 MiB (`harness.MaxKeptFileBytes`), 32 MiB together
  (`harness.MaxKeptFilesBytes`), counted apart from its 8 images. A
  destination both shown as an image and linked is kept once, as the
  image. A link to a directory is skipped with the new reason
  `not_a_file`, a link past the count or the bytes of its message
  `limit`, and a file the credential deny-list names `unavailable`. The
  blob route answers a kept file that is no image as an
  `application/octet-stream` download with `nosniff`, as it answers any
  such blob, so no file an agent wrote opens as a page. Redacting an
  answer removes its linked files with its images.

### Changed

- `GET /v1/sessions/{id}/files` answers a file of at most 32 MiB
  (`server.MaxFileBytes`), up from 10 MiB. The route copies the file
  from the machine as it reads it and holds none of it, so its bound is
  its own and larger than the bytes a linked file is kept at, which the
  session's store holds whole.
- A runner in its own process stores a blob of at most 16 MiB through
  the runner protocol, up from 8 MiB, so it stores a kept file.
- Keeping one message's files is bounded at 72 seconds, up from 30:
  the time its images and linked files take together at 1 MiB a second.
- For a program that embeds the harness: `harness.KeepImages` is
  `harness.KeepFiles`, which takes the media type rule as its last
  argument, and `harness.Config.MediaType` hands a harness that rule. A
  drive of `runner` sets `runner.MediaType`, the rule the files route
  answers by; a harness given none types every linked file
  `application/octet-stream`.

### Fixed

- A model knows today's date on every turn. The date reached a model
  only in the context block a machine records when it attaches. A turn
  that opened no machine, one that only searched the web for example,
  told its model no date, so a model asked for the latest of anything
  answered from the months its training ended in; and a session whose
  machine stood for days read the day it attached as the date. Every
  request now ends its system prompt with a part of its own,
  `context/date-v1`:

  ```
  <context>
  Today's date: 2026-10-13 (UTC)
  </context>
  ```

  The day is the UTC day of the `session.status` `running` that opened
  the request's turn, read from the log and never from a clock, so every
  request of a turn carries the same bytes, a subagent's included. The
  part follows the system prompt's cache breakpoint and carries none: a
  new day changes nothing a provider cached before it. A machine's
  context block no longer names a date (`context/context-v2`). A block
  recorded before this release keeps its `Date` line, and a request over
  one from another day adds a line saying that it is the day the machine
  attached, not today. The harness prompt is unchanged, and
  `prompt_version` stays `harness/1`.

### Upgrading

- The date part adds no event and no member, so a client changes
  nothing for it. A program that replays a recorded session with
  `models.Replay` reads `mismatch` for the requests an earlier release
  recorded, which carried no date part; requests recorded from this
  release on are built again to their hash.
- A client that draws an answer's images from `files.kept` needs no
  change: an entry it matches to no image is skipped. To offer a linked
  file, match a link's destination to an entry's `path` and fetch its
  `blob`; an image's entry has a positive `width`, a linked file's 0.
- No store takes a blob as a stream. A process that keeps a linked file
  holds about twice its size while it stores it, and a fork reads every
  blob its copied events name before it writes them. Four sessions that
  each keep a file of the most bytes in the same moment hold about
  128 MiB in the server that drives them. The bounds are sized for a
  store that keeps blobs in Postgres rows, where an answer's linked
  files add up to 32 MiB to the database.
- Every runner has known `files.kept` since v0.23.0, so this release
  rolls in any order. The store and the authorizer need no change.

## v0.29.0 - 2026-10-10

### Changed

- A turn after the first in a session starts its model request without
  registering a new Lux key. Each turn is a drive under a lease of its
  own, and each drive generated a new key value that the authorizer
  registered with Lux before the turn's first model request. A drive of
  `serve`'s own runners now takes up the key of the session's last drive
  when its Postgres lease shows that every lease of the session since
  was taken by the same process, so no other runner can have replaced
  the key. A drive after another process or a remote runner held the
  session, and a store whose leases do not count (the directory and
  memory stores), generate a new key as before. With the session key
  routes answering in 250 ms, the time from `session.status running` to
  the model request of a warm turn went from 261 ms to 7 ms at the median
  on a local Postgres, and a nine-turn session registered one key where
  it registered nine.

## v0.28.0 - 2026-10-09

### Fixed

- A response that writes a tool call into its text instead of calling
  the tool no longer reaches the person as raw markup. Some models, small
  open models among them, sometimes answer with the call written out,
  such as `<question><header>...</header>...</question>` or
  `<tool_call>{"name": "question", ...}</tool_call>`, and no `tool_use`;
  the turn ended and the person read the tags instead of a question card.
  When a response calls no tool and, outside code blocks and code spans,
  holds an element named after a tool its request offered or a call
  wrapper naming one, the step does not keep it: its `model.request` is
  recorded with the new `outcome` `tool_as_text` and no `agent.message`,
  the stream is reset, and the same request goes once more with a short
  reminder to call the tool, `reminders/tool-as-text-v1`. A tool named in
  prose, markup named after no offered tool such as `<plan>`, and markup
  in code are answers as before.
- A routed turn whose model writes the call as text again after the
  reminder can move to another model, once a turn. The turn asks the
  authorizer the `session.update` of a failover with the new
  `failed_reason: tool_as_text` and the tool in `failed_detail`; an allow
  that names another model moves the turn there, recorded as
  `session.model_changed` with the new `reason` `tool_as_text` ("The
  model could not use its tools, so another one answered."). Otherwise,
  the reminded response is the answer, as an answer was before.
  Nothing loops: a step sends at most two requests on each of two models
  for this.

### Added

- `model.request` carries `reminder` `{prompt, tool}` on a request that
  carried the reminder, so a replay builds it again to its recorded hash.
- `harness.Config.Log` receives one line for each response that wrote a
  tool call as text, `a response wrote a tool call as text`, with the
  session, thread, turn, step, model, tool and the `action` taken:
  `reminded`, `moved` with `to`, or `kept` with `why`. It never holds a
  word of the response. `toposd` writes it to its own log.
- An authorizer that picks a model by what a message asks can read the
  message, where the operator allows it. An allow that names a model may
  name `message_text: true`, which the session keeps on its model beside
  `route` by the same rules, in the header and in both sides of
  `session.model_changed`. With the new `TOPOS_AUTHORIZER_MESSAGE_TEXT`
  set to `true`, each later `session.send` about a person's message on
  that session, from the send route, a trigger firing into an open
  session or a fork sent its message, carries `message_text`: the
  message's text, whole up to 2,000 characters, or its first 1,500 and
  its last 500 around a line holding `…`. The variable is `false` by
  default, and then no question carries a message's words, whatever an
  allow asks. A create's first message never carries them.
- A `user.message` a client sends, and a fork's `message`, may carry
  `askable: true`, saying the client shows an authorizer's ask on the
  message to the person, and `answers`, the ask it is sent in answer to,
  at most 512 bytes. `session.send` carries both under the same names;
  no event keeps them. A server before this release refuses a message
  that carries either as `invalid_request`, so a client sends them only
  to a server that documents them.

### Security

- Built with Go 1.27.2 and golang.org/x/net v0.60.0, which fix GO-2026-6611, GO-2026-6612, GO-2026-6613 and GO-2026-6617.

### Upgrading

- An authorizer that refuses a `failed_reason` it does not know keeps
  every such turn on its model, and the reminded response is the answer;
  the reminder works with no authorizer change. An authorizer that reads
  `failed_model` but ignores `failed_reason` would read the question as
  one about a model that cannot serve, so an installation that wants
  these turns to move rolls an authorizer that reads `tool_as_text`
  first, then toposd and every runner. The runner protocol's failover
  request accepts `reason` `tool_as_text`; a server before it refuses
  the request as `invalid_request`, and the turn keeps its answer.

## v0.27.0 - 2026-10-08

### Added

- A question about a person's message tells the authorizer its shape and
  never its words: `session.send` of a `user.message`, a fork sent its
  message and a create that carries a first message carry
  `message_chars`, the characters of its text; `attachments`, its files
  and images; `links`, the `http://` and `https://` addresses in its
  text; and `tools_last_turn`, whether the session's last turn called a
  tool on any thread. An installation can choose a model by what a
  message asks.
- An allow that names a model may name `route`, the routed name the
  authorizer chose on the way to it. The session keeps it on its model
  beside `via`, in the header and in both sides of
  `session.model_changed`; an allow that names a model without one
  clears it, and a route that moves while the model stays is a change of
  its own. A turn moved off a model that cannot serve keeps its route,
  and a fork starts on its parent's. Every question that carries
  `model_via` carries the route beside it: `model_route` on a send and a
  fork, `current_model_route` on an update.

## v0.26.0 - 2026-10-08

### Changed

- `publish` with `release: true` puts a change live in one call: it
  publishes the folder, or an attached app's checkout, waits for the
  build, and releases the commit it built, so a person approves a change
  once. A build that fails or is canceled releases nothing, and the
  result says why with the end of the build log; a build the host has
  not started when the wait ends releases nothing yet; one still running
  is tagged, and the host releases it once it is ready. A release right
  after a ready preview, with nothing changed, releases that preview.
  The tool's description is `tools/publish-v3`: one release call is the
  way to put a change live, and a preview first is for a person who
  wants to look. After a `behind_live` refusal the model is told to
  merge and release again in one call.
- A release that names neither `app` nor `path` takes the app whose
  preview stands, else the one app the session has published to, and is
  refused `app_required` when that is several apps, or none in a session
  that was attached apps. A `path` given with `release` chooses the app
  as it does for a preview, and is refused `not_in_checkout` outside the
  named app's checkout. A call to the session's own app with no `path`
  publishes the folder it published last, else the working directory.
- A call a tool can tell is bound to be refused is answered before it is
  decided, so in `confirm` and `progressive` no person is asked to
  approve it. A tool offers this through `tools.Checker`; `publish`
  refuses `app_not_attached`, `app_required`, `not_in_checkout`, and a
  drive with no credential for the app host this way. The check sees the
  thread's state before the step, so a call after another of the same
  tool in its step is left to run and is refused there.

### Added

- `meta.publish.folder` names the folder a publish result published,
  relative to the working directory when it is inside it: an attached
  app's checkout, or the folder of the session's own app.

### Upgrading

- A `publish` call refused before it is decided has a `tool.result`
  and no `agent.tool_use`, as a call whose input fails its schema has
  none. A client that pairs each result with its `agent.tool_use` shows
  it as it shows such a call. A release's result may carry its build's
  `failed`, `canceled` or `building` status with no `release`: nothing
  was released.
- Agent instructions that tell the model to publish a preview and then
  release keep working, at two approvals per change; they can say
  instead that one call with `release: true` puts a change live.
- A result recorded before this release names no `folder`: a release of
  the session's own app that names no `path` then releases its standing
  preview as before.

## v0.25.0 - 2026-10-07

### Added

- A session's `metadata` is a label a client files sessions under.
  `GET /v1/sessions` and `GET /v1/sessions/summary` take one
  `metadata.<key>=<value>` filter, which keeps the sessions holding
  that entry exactly, beside every other filter and under `group=tree`.
  `PATCH /v1/sessions/{id}` takes `metadata`, a merge in which a string
  sets a key and `null` deletes it; a body that names `metadata` alone
  is taken on an idle, a running and an ended session, and appends no
  event. A key is a letter or digit then at most 62 letters, digits,
  dots, underscores or hyphens, and a value at most 512 bytes
  (`session.MaxMetadataValue`) without a control character, at a create
  and at a change; a change is checked on the merged result.
- The authorizer reads a session's metadata: `session.create` and
  `session.fork` carry `metadata`, absent when the session has none, and
  a change asks `session.update` with `metadata`, the change as sent,
  and `current_metadata`, the values the named keys hold now.
- Postgres keeps each entry in `session_metadata`; migration 0009 adds
  the table and fills it from the sessions already stored.
- The authorizer's allow of `session.create` attaches what a session
  starts with beyond its request. `repositories`, each `{url, ref, app}`,
  follow the request's resources marked `attached`, at most 8 together;
  one with `app {slug, name, url}` is the source of that app at the
  installation's app host, checked out into `<workdir>/<slug>` on the
  session's branch at the commit the app serves
  (`current_deploy.commit_sha`, else `latest_preview.commit_sha`, else
  the default branch), fetched with its tags. `session.machine` records
  each app's `app`, `base` and, when the host could not be used,
  `base_error`. `context`, titled text of at most 32768 bytes together
  (`session.MaxContext`), is read by the model after the initiator's
  instructions for the session's life, and an attachments block names
  each app's checkout, name, address and slug. A fork carries what its
  parent's request named and what its own allow attaches. A request that
  names a repository's `app` or `attached` is `invalid_request`. An
  allow that breaks a member's rule is `authorizer_unavailable`, and the
  refusal's detail now names the member that did not decode.
- `publish` takes `app`, the slug of the app to publish to or release:
  one the authorizer attached, or one the session created. An attached
  app is published from its checkout: the tool commits it on the
  session's branch and pushes that branch to the attached repository,
  so a merge is plain git there. With no `app`, a folder inside an
  attached app's checkout publishes that app. An `app` the session may
  not publish to is `app_not_attached`, a release with no `app` while
  previews of several apps stand is `app_required`, and a folder outside
  the named app's checkout is `not_in_checkout`, each before any
  request. A release is refused `behind_live`, with no tag, when the
  commit the app serves is not an ancestor of the one it releases, and
  the model is told to merge it first. `meta.publish` carries
  `attached` for an attached app, and the thread keeps a standing
  preview per app.

### Upgrading

- Roll the authorizer first when clients will change a session's
  metadata: an authorizer that decides `session.update` by the fields it
  knows answers a change that names `metadata` alone with a deny, so the
  change is `forbidden` until it accepts `metadata` and
  `current_metadata`. An authorizer that ignores unknown fields reads the
  create and fork questions as before.
- Start filing and listing by a key once every replica runs this
  release: a session a replica of an earlier release creates writes no
  `session_metadata` rows, and a filtered list on Postgres misses it
  until its metadata changes.
- Have the authorizer send `repositories` and `context` only once every
  replica and runner runs this release; an earlier one ignores them and
  the session starts without them. This release does not read `files`.
  An app's checkout starts at the commit the app host serves once the
  host answers `commit_sha` on `current_deploy` and `latest_preview`,
  and at the default branch until then; until then, too, `publish` reads
  every app as having nothing live and releases without the check.

## v0.24.0 - 2026-10-07

### Added

- An answer's images are kept. Once a step of the session's own thread
  is committed, each local image its `agent.message` names, by a
  Markdown image or an HTML `img`, a path with no scheme or a `file:`
  URL inside the working directory, is read from the open machine,
  stored as a blob of the session, and recorded by `files.kept` before
  the next request or the turn's closing status. An image is PNG, JPEG,
  GIF or WebP by its bytes, never SVG, at most 5 MiB, 8192 pixels a side
  (`harness.MaxKeptImageSide`) and 40,000,000 pixels
  (`harness.MaxKeptImagePixels`); a message reads its first 8
  references (`tools.MaxImages`). A reference that is not kept is in
  `skipped` with its reason. A turn that opened no machine opens none to
  keep an image, and skips its references `no_machine`; a step that
  waits on a person keeps its images when its calls are answered.

### Upgrading

- Roll this release only once every runner, the external ones included,
  runs v0.23.0 or later: a runner before v0.23.0 does not know
  `files.kept` and stops a session whose log holds one with
  `schema_too_new`. The authorizer and the store need no change.
- Roll clients that draw an answer's images after it: a client reads
  `files.kept` and falls back to `GET /v1/sessions/{id}/files` where
  none was kept, and a client that does not know the type skips it.

## v0.23.0 - 2026-10-07

### Added

- A person can edit a message and ask again from that point. `POST
  /v1/sessions/{id}/fork` takes `before_seq`, the message a person sent
  that started a turn, and forks just before it: the new session holds
  the events before the message, a model change the person made after
  the last answer included, and `before_seq` 1 copies nothing. `message`,
  the edited message as a send takes it, is sent to the new session in
  the same call, and a runner starts its turn at once; a file of the
  original message is kept by naming its `blob`. `title` names the new
  session, in place of the continuation title. A message sent while a
  turn ran, a trigger's message, and any other event are
  `invalid_fork_point`.
- "Continue in a new conversation from here": a fork body's `tree:
  "new"` starts a tree of the new session's own. Its `root` is its own
  id and its `parent` still names the session it came from, so
  `group=tree` lists it as a conversation of its own, `root=` of the
  source leaves it out, and its edits are versions of it. Absent, a fork
  joins its parent's tree; any other `tree` is `invalid_request`.
  `session.fork` carries `root`, the root of the tree the new session
  joins.
- A fork with a message asks the authorizer `session.read`, then
  `session.fork`, then `session.send` of the new session, before anything
  is written; a deny of either writes nothing. A send refused after the
  fork was allowed is handed to the server's sink option as
  `session.fork` with the refusal's code as its outcome, and toposd logs
  it.
- A session made by a fork carries `root`, the session at the top of its
  tree of forks, beside `parent`. `GET /v1/sessions` takes `root`, a
  tree's sessions, `parent`, a session's forks, and `group=tree`, one
  session per tree, the newest, carrying `tree {root, sessions}`. The
  summary takes none of them. Postgres keeps `parent_id`, `parent_seq`
  and `root_id` in columns; migration 0008 adds them and fills them, and
  `root`, for the forks already stored.
- `docs/editing-a-message.md`: how a client sends an edited message and
  shows its versions.
- The `files.kept` event, read and not yet written: it follows an
  `agent.message` and maps each local image the answer names to a blob
  of the session, `{message, files, skipped}`. This release folds it
  into no prompt, so a log that holds one runs, and it is redactable.
  Redacting an `agent.message` redacts the `files.kept` that names it in
  the same append, one `event.redacted` each, and deletes an image no
  other event names; a redaction sent again for a redacted answer takes
  a `files.kept` that landed since. A redacted `files.kept` keeps the id
  of the answer it named, and no image. No runner of this release
  appends it.

### Changed

- A fork's budget counts its own spend. `budget.carried_cost_usd_micro`
  is the spend a fork copied from its parent, and the check before each
  model request holds `spent_cost_usd_micro` less it to the fork's
  ceiling, so a fork of a session that spent most of its budget runs on
  its own. This holds for a fork that continues an ended session too.
- A request's prompt cache key is the session's tree's root, its `root`
  or its id, so a fork and the session it came from share one key.
- `invalid_fork_point` says "A session is forked only at the end of a
  turn, or before a message of yours that started one."
- `GET /v1/sessions/{id}/blobs/{digest}` answers by the blob's leading
  bytes: PNG, JPEG, GIF or WebP as that image type, `inline`, and any
  other blob, SVG included, as an `application/octet-stream`
  `attachment`. Every blob carries `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`,
  `Cache-Control: private, max-age=31536000, immutable` and its digest
  as `ETag`. An attachment's bytes are unchanged.

### Upgrading

- Roll toposd and every runner together; the authorizer needs no change.
  Migration 0008 runs at start, in one transaction. A replica of the
  earlier release that rewrites a fork's header during the roll drops
  `root` and `carried_cost_usd_micro`: a read fills `root` from its
  column, and that fork's budget counts its whole log, as before. A fork
  such a replica creates during the roll gets its `root` from the next
  release's migration.
- Roll every runner, the external ones included, onto this release
  before any runner onto the release that keeps images. A runner that
  does not know `files.kept` stops a session whose log holds one with
  `schema_too_new`, and leases move sessions between runners during a
  rolling update.
- Roll clients that edit messages or read trees after the server: an
  earlier server refuses `before_seq`, `message` and `title` in a fork
  body as unknown members and ignores the list's `root`, `parent` and
  `group`.

## v0.22.0 - 2026-10-07

### Added

- A session's title changes: `PATCH /v1/sessions/{id}` takes `title`,
  alone or beside `model` and `policy`. The title is trimmed, and one
  that is empty, longer than 200 characters (`session.MaxTitleLength`),
  or holds a control character or a line break is `invalid_request`.
  The authorizer is asked `session.update` with `title` beside the
  fields of the rest of the change, and an allowed change appends
  `session.title_changed` `{by, old, new}`, which the header's `title`
  follows. A rename does not move `updated_at`, so a list ordered by it
  keeps the session in its place, and a fork keeps the title its create
  gave it over the renames it copies.

### Changed

- An idle session can be archived, not only an ended one, so a client
  whose sessions stay open between turns can file one away. A running
  session is still `conflict`. An archived idle session takes a message
  as before, and stays archived while its turn runs.

## v0.21.0 - 2026-10-06

### Added

- A session has a network: where its sandbox may reach. An authorizer's
  allow of `session.create` may carry `limits.network`, `{mode, hosts,
  ask}`, with `mode` `open`, `allowlist` or `none`; the session records
  it as `network`, or its agent's mode with `ask` false when the allow
  names none. An agent names its own mode as `spec.machine.egressMode`
  beside `egress`, `allowlist` when absent. Under `allowlist` the agent's
  hosts, the hosts of the session's credentials and its repositories' git
  hosts are always joined, so a narrow answer never cuts a session off
  from its model gateway or its repositories. An allow of `session.send`
  that names another network replaces the session's before the next
  turn, recorded as `session.network_changed` straight before the sent
  event, and narrows or widens the running sandbox.
- A `web_fetch` of a host inside the network runs without asking, its
  reason `inside the session's network`. One outside it asks, its reason
  `outside the session's network`, when the network asks and the session
  is attended, and is blocked otherwise. A person's allow widens the
  running sandbox to the host before the fetch runs and records it as
  `session.network_changed`; the host stays reachable for the rest of the
  session. A widening Cella refuses closes the call with
  `network_unavailable` and changes nothing.
- A command's connection the egress gateway refused becomes an
  `approval.requested` after the command's result, `{approval_id,
  tool_use_id, source: egress, destination {host, port}, reason, verdict,
  more}`, at most 3 per command. With `verdict: ask` the session waits
  for a `user.tool_confirmation` that names the `approval_id`; an allow
  widens the network and appends `approval.decided`, and the agent may
  run the command again. A deny, or a message in place of an answer, is
  not asked again in the session. `topos confirm` takes an `apr_` id.
- An authorizer's allow of `session.create` may carry
  `limits.instructions`, the initiator's standing instructions, at most
  8 KiB of UTF-8. The session records them as `instructions`, and the
  agent reads them after its own instructions in the prompt's cached
  prefix, the same bytes on every turn; a send never changes them, and a
  fork takes its own allow's.
- `docs/network.md` is the contract an authorizer and a client follow.

### Changed

- On a Cella machine, a `web_fetch` of a host outside the session's
  network is blocked, and the model reads that the host is outside the
  network this session may reach. Before, it asked the person in
  `confirm` mode and then failed at the gateway. This holds for a
  session whose authorizer names no network too: it runs on its agent's
  mode and hosts with `ask` false, so a fetch outside the agent's
  `spec.machine.egress` is blocked where it was asked. A session on the
  host machine decides a fetch as before.

### Upgrading

- Roll the authorizer that answers `network` and `instructions` first: a
  server before this release ignores both members. Then roll toposd and
  every runner together, since a runner that reads a log must know
  `session.network_changed`, `approval.requested` and `approval.decided`
  before any is appended. Until a client answers by `approval_id`, a
  command's refused connection in a session whose network asks leaves
  the session waiting until the person's next message, which denies the
  host for the rest of the session; answer `ask` false until the client
  rolls.

## v0.20.0 - 2026-10-06

### Fixed

- A routed turn whose request the model's provider rejects can move to
  another model. When the gateway answers `upstream_rejected`, the
  provider's own 4xx and not one of the gateway's refusals, the turn asks
  the authorizer the same `session.update` it asks for a model that
  cannot serve, with the new `failed_reason: rejected` beside
  `failed_model`, and the gateway's code and developer detail as
  `failed_detail`, such as `upstream_rejected: upstream status 404: ...`.
  An allow that names another model moves the turn there, as one of its 3
  moves, and the conversation records the change with `model_busy`. An
  allow that names none, or a deny, ends the turn with `model_error` as
  before. Providers withdraw and change free models without notice, and
  until now such a turn ended with an error while the routed name stood
  for models that would have answered. The gateway's own refusals,
  `invalid_request`, `model_not_allowed`, `rate_limited`,
  `budget_exhausted` and the rest, never move a turn. An authorizer that
  reads `failed_model` but not `failed_reason` would read the question as
  one about a model that cannot serve and move the turn on any rejection,
  so the authorizer that reads the field rolls first, then toposd and
  every runner. The runner protocol's failover request carries `reason`
  when there is one; a server before it refuses that request as
  `invalid_request`, and the turn ends with `model_error` as before.
- Every model failure's `session.error` carries the gateway's developer
  detail. A `model_error`, a spend refusal and a `compaction_failed` read
  only the HTTP status and type, such as `HTTP 400 upstream_rejected`,
  while the provider's own status and words were on the failed
  `model.request` alone. Their `detail` now adds the gateway's
  `Lux-Error-Detail` in parentheses, at most 1024 bytes, as
  `HTTP 400 upstream_rejected (upstream status 404: {...})`; a turn the
  authorizer kept on its model adds why it did not move. The `message`
  is unchanged.

## v0.19.1 - 2026-10-05

### Fixed

- A turn that moves off a model that cannot serve now reaches the model
  the authorizer names. The authorizer widens the session's key to that
  model as it answers, and a gateway with several replicas applies the
  change on each within a moment of its own, so the runner could read
  the door's model list before the change reached it, find the model
  missing, and end the turn with `model_busy` and `model_unknown` in the
  detail. While a door lists other models and not the one the runner
  connects, the runner now reads the list again for up to 3 seconds
  (`hosted.DoorSettle`), which also covers a model a send moves the
  session to.
- A model the authorizer names that still cannot be connected no longer
  ends the turn. It counts as one of the turn's 3 moves, and the next
  `session.update` names it as `failed_model`, beside the model the
  session stands on as `current_model`, with why as `failed_detail`, so
  the authorizer passes over it too. The change the turn records says
  which models were passed over and why. An authorizer that holds
  `failed_model` to `current_model` refuses that question, and the turn
  ends `model_busy` as before, so such an authorizer rolls first to
  accept it. The runner protocol's failover request carries `standing`,
  the model the turn stands on, when it is not `failed`; a request
  without it stands on `failed`.

## v0.19.0 - 2026-10-05

### Added

- `GET /v1/sessions/search?q=` finds the sessions `GET /v1/sessions`
  would list for the caller by what was said in them: the text of the
  messages a person sent and the answers the agent wrote on the
  session's own thread, and a person's answers to the agent's
  questions. Each word of `q` is found as the start of a word, whatever
  its case, and characters of Han, Hiragana and Katakana typed in a row
  as those characters in a row; a message matches when it holds every
  one. Tool output, the model's thinking, a subagent's thread, files and
  redacted messages are never searched, and markdown's marks and a
  link's address are not either. Each session found carries its 3
  newest matches, each with its `seq` in the log and an excerpt of at
  most 160 characters with the matched words marked. Pages hold 20
  sessions at most, newest first by id, and take the list's `agent`,
  `status`, `runner` and `archived` filters. The route asks
  `session.list` as the list does and `session.read` of each session it
  finds, leaving out one the caller may not read; an authorizer that
  answers both needs no change. `docs/searching-sessions.md` is the
  contract a client follows.
- A turn on a routed name moves off a model that cannot serve. When the
  gateway answers `upstream_error`, `provider_unavailable` or
  `upstream_timeout`, the turn asks the authorizer at once for another
  model and continues on the one it names, inside the same turn, at most
  3 times a turn. The log records the failed `model.request`, then
  `session.model_changed` by the service with the new `reason`
  `model_busy` ("The model was busy, so another one answered.") and the
  gateway's answer in `detail`, then the request that answered.
- The question is `session.update`, asked as the session's initiator in
  the session's context, with `model` the routed name, `current_model`
  and `current_model_via`, and two new fields: `failed_model`, the model
  that failed, and `failed_detail`, the gateway's developer detail of the
  failure, at most 1024 bytes, such as the upstream's own status. A
  runner process asks it over the internal listener at
  `POST /internal/v1/leases/{session}/failover`.
- The model client keeps a gateway's developer detail
  (`Lux-Error-Detail`), and a failed `model.request`'s `error` and the
  turn's `session.error` detail show it, so a provider's rate limit is
  told from its outage.

### Changed

- A gateway's answer that the model cannot serve is no longer retried
  for about a minute. A turn that can move asks for another model with no
  retry; one that cannot, on a model named itself or out of moves, retries
  once a second later. Every other failure keeps its retries.
- Such a turn ends with `session.error` `model_busy`, retryable, with the
  sentence "The model is busy right now. Send your message again in a
  moment." in `message`, where it ended with `model_error` and the
  gateway's words.

### Upgrading

- The release carries a Postgres migration, which adds a column and two
  indexes to `events`. A server indexes the messages written before it
  in the background after it starts, and again every ten minutes for
  those a replica of an earlier release writes during the rollout, so a
  search finds an older session a short while after the first replica
  of this release starts. The directory store needs no migration: a
  search reads each session's log.
- Roll the authorizer that reads `failed_model` before this release. An
  authorizer that does not read it keeps the session on its model, and
  such a turn ends at once with `model_busy`.

## v0.18.0 - 2026-10-05

### Added

- An authorizer's allow may set the reasoning level a session runs at,
  as `limits.reasoning`, read where `limits.model` is: at a session's
  create, at a change of its model or level, and at each send. Absent
  keeps the session's level, one of `minimal`, `low`, `medium` and
  `high` sets it for the next turn, and `""` returns the session to its
  agent's own. A send whose allow names another level appends
  `session.model_changed` by the service and keeps the model; a session
  already at the level appends nothing. A fork starts at the level its
  parent stood on. A level outside the four refuses the request as
  `authorizer_unavailable`.
- A refusal of the authorizer's carries the deny's limits beside its
  reason, as `details.limits`, so a client can say when a request
  refused for a bound that resets can be made again (`resets_at`). A
  refusal whose reason is withheld carries no limits either.

### Changed

- The API names a model's reasoning level `reasoning`, where it said
  `effort`: an agent's `spec.model.reasoning`, its advisor's and each
  inline subagent's, a session's `model.reasoning`, the `PATCH
  /v1/sessions/{id}` body's `model.reasoning`, and `old` and `new` of
  `session.model_changed`, in the events list and the stream, events
  stored before this release included. Every answer names `reasoning`
  alone. A manifest and a `PATCH` body may still name `effort`, which is
  read on input through every v0.x release and dropped in v1.0; both
  names with two levels are refused as `invalid_manifest` or
  `invalid_request`.
- What is stored keeps `effort`: an agent version keeps its digest and
  its version, and an agent applied again unchanged keeps its version
  under either name. An agent read answers a spec that is not byte for
  byte the spec its digest covers. A server rolled back to an earlier
  release reads every agent, header and event this one stored, its
  levels included.
- The `session.update` question carries a change's level under both
  `effort` and `reasoning`, so an authorizer that reads either name
  decides it.

### Upgrading

- Move a client that reads a session's, an agent's or an event's level
  to `reasoning` before rolling this release: from it, no answer names
  `effort`. A client that writes `effort` keeps working through v0.x.
- Roll an authorizer that answers `limits.reasoning` before or after
  this release: an earlier server ignores the member, and this one keeps
  every level when the member is absent.

## v0.17.0 - 2026-10-05

### Added

- An agent can put a folder of its sandbox online with the `publish`
  tool. The first call creates the session's own app at the
  installation's app host, named after the session; the folder is
  pushed from the sandbox to the session's branch of the app's
  repository, and the call waits for the preview the host builds and
  answers with its address, or with the failed build's code and the
  end of its log. `publish` with `release: true` releases the newest
  preview that is ready or still building to the app's address with
  the next `v` tag. Each call
  asks the person in `confirm` and `progressive`, the runner reaches
  the host with the session's own token, which never enters the
  sandbox, and every result carries `meta.publish` for a client to show
  the preview and the release. An agent holds the tool only when its
  manifest names it; a client tool may no longer be named `publish`,
  and `topos run` refuses an agent that names it. See
  `docs/publishing.md`.
- `TOPOS_APPS_URL` and `TOPOS_APPS_AUDIENCE` connect the app host and
  name the audience of the session's token there. The URL needs
  `TOPOS_ORIGO_URL`.
- Every request of a session that runs by a routed name carries it to
  the model as `Model route: <name>`, after the other system parts, so
  an agent can say when another route would serve the person better.

### Changed

- A hosted session's sandbox starts when the model's response begins a
  call of a tool that acts on a machine, while the call's arguments
  stream, instead of as each turn begins. A turn that only talks creates
  and starts no sandbox, so a conversation that never uses a tool holds
  none of the sandboxes Cella allows its owner. The session still
  records its machine at the first tool that uses it. `machine.Starter`
  and `machine.Start` begin the open of a machine opened on demand.
- A session that already has a sandbox no longer starts it as a drive
  begins: it starts at the first call of a tool that acts on it, as a
  new session's does, and the turn's requests carry the machine context
  the session recorded. A turn that only talks leaves a stopped sandbox
  stopped, and a start Cella refuses is the result of the call that
  needed it, carrying Cella's sentence to the model, rather than a
  failed turn. A turn that opens no machine takes no checkpoint, so a
  rewind to it answers `checkpoint_missing`.

## v0.16.0 - 2026-10-05

### Added

- An agent can search the web with the `web_search` tool: each call
  sends a query to the search service the installation configures with
  `TOPOS_SEARCH_URL` and returns results the agent can cite, a title, a
  URL and a snippet each, which it reads in full with `web_fetch`. An
  agent holds the tool only when its manifest names it in `spec.tools`;
  an agent that names no tools holds the eight built-ins as before, and
  its version and digest do not change. A client tool may no longer be
  named `web_search`: a manifest that declares one is refused as
  `invalid_manifest` at its next apply.
- Topos names no search provider: the contract a service answers is in
  `docs/web-search.md`. On an installation with session keys each search
  carries the session's own model key, so the service can tell which
  session searched; without them it carries `TOPOS_SEARCH_KEY`. A
  service's refusal reaches the agent as its own sentence for the
  person, and its code is the result's `refusal` meta.
- A `tool.result` records `cost_usd_micro` when a service charged for
  the call, and the session's spend, its budget check before each model
  request and the task suite's run cost count it beside model requests.
  A redacted result keeps its cost, so no redaction lowers the spend.

## v0.15.0 - 2026-10-05

### Added

- `GET /v1/sessions/{id}/files?path=<path>` answers one file of a
  session's working directory as it is now, such as a page the agent
  wrote, to anyone who may read the session: the authorizer is asked
  `session.read`, with no new field. The path is absolute, as a write or
  edit result names it, or relative to the working directory. The answer
  is a download of at most 10 MiB with its media type, its length, an
  attachment disposition with its name, `nosniff`, the policy
  `sandbox; default-src 'none'` and `no-store`; `HEAD` answers the
  headers alone. The file is read from the session's machine while it
  runs, and the read never starts or creates one: a sandbox that is
  stopped, never started or gone is `file_unavailable` (409), a larger
  file `file_too_large` (413), and a path outside the working directory,
  a directory or a credential path `invalid_request`. A Cella sandbox is
  read with the session's own Cella token where the installation mints
  one, and with `TOPOS_CELLA_TOKEN_FILE`'s bearer otherwise; a host
  session's directory is read on the server's disk.
- A `write` or `edit` result's `meta` carries `size`, the length in
  bytes of the file it left, beside `path` and `sha256`.

### Changed

- A hosted session's sandbox starts as a turn of an agent whose tools
  act on a machine begins, beside the turn's first model call, instead
  of at the first tool call, so the sandbox's start and the model's
  first answer overlap. The session still records its machine at the
  first tool that uses it. A turn that only talks starts the sandbox
  too and records none; Cella stops it after its idle time, and the
  drive does not wait for it. `machine.Deferred` gains `Start`.
- A server started in the foreground no longer holds the turn until the
  command's timeout. A `bash` command still running after 3 seconds
  whose process group listens on a TCP port it chose is moved to the
  background: the call returns at once with the output so far, the
  port, the job's pid and its log, and the server keeps running until
  the model stops it or the session ends. A listener on a port in the
  system's ephemeral range, as a test's server bound to port 0, does
  not count, and every other long command keeps its timeout. This
  works on Linux, in Cella sandboxes, and on macOS; a sandboxed host
  and Windows keep the timeout alone. `machine.ExecRequest` gains
  `ServerGrace`, and `machine.ExecResult` gains `Moved`, `Ports` and
  `ServerErr`.

## v0.14.0 - 2026-10-05

### Added

- `PATCH /v1/sessions/{id}` takes `{"policy": {"mode": "plan" | "confirm" |
  "progressive"}}`, alone or beside `model`, so a person can change how a
  session asks after it started. The authorizer is asked `session.update`
  with `approval_mode`, `current_approval_mode` and `agent_approval_mode`;
  an allowed change appends the new event `session.policy_changed`
  `{by, old: {mode}, new: {mode}}`, in one batch with a model change, and
  the Session's `policy.mode` reads the new mode. The mode holds from the
  next step of the turn that runs. A call already waiting for a
  confirmation keeps waiting, whichever way the mode moved. A thread
  never runs looser than the modes its own agents name, a host with no
  operating-system sandbox runs a switch to `progressive` as `confirm`,
  and a fork starts in its agent's mode. The agent's
  `spec.approvals.mode` is the mode a session starts in. It does not
  limit the modes a person can switch to: the lists, the thresholds and
  the hard boundaries apply in every mode.

### Changed

- `DELETE /v1/sessions/{id}` asks `session.read` before `session.delete`,
  and refuses a running session as `conflict` before it asks
  `session.delete`. Before, an authorizer that acts on an allowed delete,
  as one that revokes the session's credentials does, acted for a delete
  the store then refused, and the running session was left cut off.
  Interrupt a running session, then delete it once it is idle. A delete
  removes the session's rows at once and its outside blob bodies at
  once or within the reaper's next pass. It leaves the hosted session's
  sandbox, its Secrets and a checkpoint ref at the git host to the
  installation, which the API reference now says.

### Upgrading

- Roll this release after the installation's authorizer answers
  `approval_mode` on `session.update`. An authorizer that requires a model
  or an effort refuses a change of the mode alone, which is safe, and one
  that reads only the model would allow a mode changed beside a model on
  the model's rules alone.

## v0.13.0 - 2026-10-04

### Added

- An agent can put a decision to the person it works for with the
  `question` tool. One call asks one to four questions, each with a
  short header and two to four options; an option has a label, a
  description, an optional preview and may be marked recommended, and
  the person can always answer in their own words. An agent holds the
  tool only when its manifest names it in `spec.tools`; an agent that
  names no tools holds the eight built-ins as before, so its version
  and digest do not change. A client tool may no longer be named
  `question`: a manifest that declares one is refused as
  `invalid_manifest` at its next apply.
- A session created with `"attended": true` on `POST /v1/sessions`, or
  forked with it on `POST /v1/sessions/{id}/fork`, waits for the
  person's answer: it goes idle with the new stop reason `question` and
  holds no runner while it waits. A session created without it, which
  is every session a client created before, answers a question at once
  with the outcome `unanswered`, and the agent decides and says what it
  assumed. `topos run --attended` does the same on your machine, stops
  with exit code 3 on a question, and prints it with its options.
- `POST /v1/sessions/{id}/events` takes `user.answer`,
  `{"tool_use_id", "answers": [{"selected", "text"}]}`, one entry per
  question. The server checks it against the open question:
  `invalid_request` for an answer that does not fit, `conflict`, with
  what closed the question in the detail, for one that names no open
  question. The authorizer is asked `session.send` with `event_type`
  `user.answer`; an authorizer that lists the event types it allows
  must accept it before a client sets `attended`. A person's message
  in place of an answer closes the question, and an interrupt dismisses
  it.
- A question's `tool.result` carries `meta.closed_by` (`answer`,
  `message`, `interrupt` or `unattended`) and `meta.event_id`, and the
  new outcome `unanswered`, which is not an error.
- `user.answer` is redactable, and redacting it also redacts the result
  rendered from it.
- `docs/questions.md` is the contract a client follows to show a
  question and send its answer; `api/openapi.yaml` gains the schemas
  `QuestionInput`, `UserAnswer` and `QuestionResultMeta`.
- The compaction prompt asks the summary to list each question put to
  the person with its answer, so a long session does not ask again. A
  summary records the prompt it was asked with in `context.compacted`'s
  `prompt`.
- A tool's input schema may use `minItems` and `maxItems`.

### Changed

- A `user.tool_confirmation` or a `user.tool_result` is appended only
  after the log it was checked against. Two sent at once for the same
  call no longer both land: one is appended and the other is
  `conflict`.
- A trigger's `continue` firing to a session that waits on a question
  is held, as it is for a confirmation.
- A person's `user.message` sent while a call waits for a confirmation
  denies the call, as the session log's contract states. The call's
  `tool.result` has outcome `denied` and carries the message's text as
  the person's note, the model reads the denial and then the message,
  and the turn goes on. Before, the session went back to idle
  `tool_confirmation` and nothing read the message. The same holds for a
  call a subagent's thread waits on. A `user.tool_confirmation` sent
  after such a message is `conflict`. A trigger's message denies
  nothing.
- When a confirmation, a question and a client tool's result wait at
  once, the session's stop reason names the first of them in that
  order. A client finds the open calls from the log, not from the stop
  reason.

### Fixed

- A tool call that a person allowed was lost when one step left two
  calls waiting and they were answered one at a time. The first call
  never ran, and after the second answer its `tool.result` came back
  with outcome `unknown_effect`, telling the model to inspect a command
  that had not started. Each call now runs in the turn that reads its
  confirmation, while the other still waits. The same loss hit a
  confirmed call beside a subagent's thread that still waited for a
  confirmation of its own.
- Redacting a `tool.result` made its call look unanswered. The next
  turn appended a second `tool.result` for the same call with outcome
  `unknown_effect`. A redacted `tool.result` or `user.tool_result` now
  keeps its `tool_use_id` beside the tombstone mark,
  `{"tombstone":true,"tool_use_id":"..."}`, and still answers its call.
  The id is the model's name for the call and holds nothing of the
  result. A result redacted before this release keeps a tombstone
  without the id and is read as before.

## v0.12.0 - 2026-10-04

### Added

- An installation's authorizer can name the model a session runs. An
  allow may carry `limits.model`, the name of the model to run in place
  of the one that was asked. A session starts on it at its create,
  changes to it at a `PATCH /v1/sessions/{id}` that names a model, and
  changes to it before its next turn at a send. An agent can
  therefore name a choice, such as `tier/quick`, that no gateway lists
  and the authorizer resolves for each person. An allow without
  `limits.model` leaves every route as it was. `authorizer.Limits` and
  `authorizer.WireLimits` gain the member `Model`.
- A session's `model` has `via`: the name that was asked, when the
  authorizer answered another. It is absent when the session runs the
  name that was asked. `session.model_changed` carries `via` in `old`
  and `new`. A client that offers a choice of models shows `via`.
  `model.request` records the model that ran, as before.
- A model change that a send's allow made is recorded as
  `session.model_changed` with `by` set to
  `{"subject": "service:authorizer", "kind": "service"}`, straight
  before the event that was sent. A change that a `PATCH` made is the
  person's, as before.
- The questions tell an authorizer what it routes from. `session.create`
  carries `model`, the agent's name for its model. `session.update`
  carries `current_model`, the model the session runs, and
  `current_model_via`, beside `model`, which is still the name the
  change asks. `session.send` carries `model`, the model the session
  runs, `model_via`, and `idle_seconds`: the whole seconds since the
  session's last model request ended, absent before its first. Topos
  itself knows nothing about how long a provider keeps a prompt cache.
- A fork starts on the model that the forked session ran at the fork
  point, with its `via`, a model that the authorizer named at the
  session's create included. `session.fork` carries it as `model` and
  `model_via`.

### Changed

- `POST /v1/sessions` and `PATCH /v1/sessions/{id}` check that the
  installation runs the model after they ask the authorizer. They
  checked before. The model checked is the one the allow names, or the
  one asked when it names none, so a name that only the authorizer
  resolves is not refused unread. A request that the authorizer denies
  for a model the installation does not run now answers `forbidden`; it
  answered `model_unknown`. An authorizer is now asked about a model
  name that the installation may not run.
- An allow of `session.update` or `session.send` whose `limits` do not
  decode refuses the request as `authorizer_unavailable`. Their limits
  were not read before.
- A fork checks that the installation runs the model the fork starts
  on. It checked the agent's model, whatever the forked session had
  changed to.
- A send reads the session's agent version and the last events of its
  log before it asks the authorizer. The question of `session.send`
  changes as `idle_seconds` changes, so a cached allow of it is seldom
  reused.

- The text for a decision service is shorter and more precise. This
  includes the reason that a person sees for each verdict, the help of
  `--mode`, and two errors. The errors are for an unknown mode and for a
  missing `TOPOS_DECISIONS_TOKEN`. The configuration page describes
  `TOPOS_DECISIONS_URL` and `TOPOS_DECISIONS_TOKEN` in the same words.

## v0.11.1 - 2026-10-02

### Changed

- In the API document, an operation's `summary` is the name of its
  action in at most four words, such as `Apply an agent` or
  `List sessions`, so a reference can list operations by it. What a
  summary said is in the operation's `description`, which every
  operation now has. No route, parameter or answer changed.
- The API document shows a wire example for every operation: the request
  body where a route takes one, an apply's manifest as JSON and as YAML,
  and each success answer, as JSON, as the frames of the session
  stream, or as a binary schema for a blob. Each example is what the
  route answers to the request beside it.
- The API document lists both answers of an apply: `PUT
  /v1/agents/{name}` and `PUT /v1/triggers/{name}` answer 201 when they
  create the object and 200 when they change an existing one or leave it
  as it was, and the document listed 200 alone. It lists no request body
  for `POST /v1/sessions/{id}/archive` and `/unarchive`, whose body is
  empty. The routes answer as they did.

### Fixed

- `POST /v1/sessions/{id}/end` of a running or ended session answers
  `conflict` before it asks the authorizer `session.end`. It asked first,
  and an authorizer that revokes a session's credentials when it allows
  an end, as the platform's does, revoked them for an end the route then
  refused, so the running turn failed. The route now asks `session.read`
  first, so a caller who may not read the session still hears
  `not_found`. `POST /v1/sessions/{id}/archive` likewise refuses a
  session that has not ended before it asks `session.update`.

## v0.11.0 - 2026-10-02

### Added

- A tool call is decided by a `Decider`; without one configured, the
  rules of spec 012 decide as before. Every `agent.tool_use` now records
  `review_probability`, the probability, fixed before the call ran, that
  a person sees it.
- `topos run` can consult a decision service (`TOPOS_DECISIONS_URL`,
  `TOPOS_DECISIONS_TOKEN`; spec 037). In `progressive` mode its
  suggestion decides the calls the rules leave open, with a share of
  automatic verdicts drawn for review; in `confirm` mode the rules decide
  and the service learns. Every decision and every confirmation is sent
  to it; a service that fails or does not answer in two seconds makes the
  call wait for a person.
- `topos run --mode` takes `manual` and `auto`, the names other harnesses
  use, for `confirm` and `progressive`. Sessions, manifests and events
  keep the spec's names.

### Fixed

- A verdict outside allow, flag, ask and block now composes as block. The
  harness's verdicts are the shared vocabulary of `latere.ai/x/pkg/verdict`
  (v0.91.0), and `harness.Stricter` is its `Least`: ranked by its index in
  a list, an unknown verdict was the most permissive, so composing one
  with allow allowed. No caller in this release composed verdicts, so no
  call was affected.

## v0.10.1 - 2026-10-02

### Fixed

- A session that ended with its turn, as one created with `end_on_idle`
  does (a run-once session from the API, or a trigger's session), can be
  continued. `POST /v1/sessions/{id}/fork` forks it at the end of that
  turn instead of answering `invalid_fork_point`, and the new session
  waits for its next message (`idle`, `end_turn`) with the whole
  conversation and that turn's files. A session ended `failed`,
  `canceled` or `expired` while its turn was still running forks at the
  last turn it finished, or is `invalid_fork_point` when it finished
  none.
- A session route given an id that is not in a session's form, such as
  `POST /v1/sessions/ses_doesnotexist/end`, answers 404 `not_found`, as
  an id no session has does, instead of 500 `internal`. Every route that
  names a session answered 500 for such an id on the Postgres and the
  directory stores.

## v0.10.0 - 2026-10-02

- toposd builds with OpenTelemetry Go v1.46.0 and its log modules
  v0.22.0, past GO-2026-6615 (the log batch processor could spin when
  its export buffer was full) and GO-2026-6505 (exporter configuration
  logging could carry endpoint URLs).
- toposd builds against `latere.ai/x/pkg` v0.90.2, whose telemetry
  resource uses semantic conventions v1.43.0, the schema of that SDK.
  With the earlier schema, resource detection fails at start and every
  OTLP export (traces, metrics, logs) is disabled.
- An agent can belong to an organization. An agent applied with a token
  that names an organization in its `org_id` claim is the
  organization's, held under the organization's subject, and its name is
  unique within the organization: every member's apply of the name
  changes the one agent, and every name the API reads is read among the
  agents of the caller's context. A list of agents holds the context's,
  and a list of sessions, and the sessions' summary, the sessions of the
  context's agents, narrowed further by the authorizer. Every question
  names an organization's agent as `{type: "organization", id}`,
  `agent.create` names the organization in its context, and
  `session.list` names the context it lists as `agent_owner`. The
  agent's identity is created for its owner, and an allow naming another
  refuses the apply. Without an authorizer, an organization's agent is
  the admins' alone, and an admin lists their own context's objects.
  Every agent stored before is its person's; a Postgres database
  migrates with each row kept.
- A hosted session that works in a private repository on the git host
  of `TOPOS_ORIGO_URL` keeps each turn's checkpoint at that repository,
  under `refs/topos/checkpoints/<session>/latest`, pushed from its
  sandbox with the session's git credential and the push option
  `origo.event=off`, so a fork of the session restores the files it
  left, committed or not, after its sandbox is gone. A turn's checkpoint
  names that repository as `remote` once it is there. Before each push
  the runner reads the repository with no credential: only a refusal
  for want of one counts as private. A public repository, and one whose
  answer says anything else, never gets a checkpoint, since whoever
  reads a repository reads these refs; its fork starts from its
  repositories. The refs stay until the repository or the ref is
  deleted, deleting the session leaves them, and a repository made
  public later exposes the ones already there.
- A fork whose fork point's files the runner cannot restore starts from
  its repositories and records a `session.error` `checkpoint_missing`
  beside its first machine, naming the checkpoint and why; the call that
  opened the machine runs as it would have. Before, a checkpoint the
  runner did not find was recorded nowhere, and one it found and could
  not check out failed that call.
- A fork's first `bash` call starts in the fork's own working directory.
  It started in the directory its parent's last call ended in, which
  belonged to the parent's machine.

## v0.9.7 - 2026-10-01

- Ships the changes listed under v0.9.6, which was tagged but never
  released: its release pipeline refused the tag because the tagged
  commit never ran verify on a push. No image or binary of v0.9.6 exists.
- `GET /v1/sessions/summary` counts the sessions `GET /v1/sessions`
  would list for the caller, in one read: how many are running, waiting
  for approval (idle on `tool_confirmation`), otherwise idle, and ended,
  and how many distinct agents they belong to. It takes the list's
  `agent`, `runner` and `archived` filters and is asked of the
  authorizer as `session.list`, so it counts exactly what the caller
  may list. The Postgres store counts in one query; a session store
  that implements `session.Summarizer` counts itself, and one that does
  not is counted through its list.

## v0.9.6 - 2026-10-01

Tagged but never released; v0.9.7 ships these changes.

- `POST /v1/sessions/{id}/fork` continues a session, an ended or
  expired one included, as a new session of the same agent version: its
  log starts as a copy of the old one's up to a turn boundary, the last
  one unless `at_seq` names another, so its first turn has the whole
  conversation, and it has a lifetime, a budget and credentials of its
  own, with `parent` naming where it came from. Its title is the old
  session's marked as its continuation, "Notes (continued)" and then
  "Notes (continued 2)", so the two read apart in a list. It is asked of the
  authorizer as `session.fork` with a create's fields and the forked
  session's. The fork point's files are restored into the new session's
  working directory when the runner can reach that turn's checkpoint;
  a hosted session on Cella does not keep its checkpoints past its
  sandbox yet, so its fork starts from its repositories.
- `POST /v1/sessions/{id}/archive` and `/unarchive` file an ended
  session away from the lists and back, asked of the authorizer as
  `session.update` with `archived`. `GET /v1/sessions` leaves archived
  sessions out unless `archived=true` or `archived=any`; an archived
  session stays readable, streamable and forkable by id. The Postgres
  store adds the `archived_at` column.
- A tool call whose arguments are not valid JSON no longer ends the turn
  with `model_error`. A model can break off inside a string argument at
  its output limit, and a provider can report that stop as a tool call;
  the step is now logged as it came, with the call's input recorded as
  `{}` and the raw response kept, and the call is answered
  `invalid_input` with what was wrong and the first 200 characters of
  the arguments, so the model sends the call again on its next step.
- A tool call whose arguments arrive after its block closed, as
  interleaved parallel calls can over Chat Completions, gets its whole
  arguments instead of failing the stream, and a call the stream never
  closed is closed when the message ends with the arguments it
  received, where it ran with an empty input.
- A model request that fails part way through its stream keeps the
  bytes it received as the `response_blob` of its `model.request`, so a
  failed step can be read back as the model sent it.
- A response that stops at its output limit inside a tool call's
  arguments is no longer sent again at the model's full output limit or
  continued: a model that runs away inside an argument only runs further
  with more room. The step keeps the response, runs the calls before the
  cut one, and answers the cut call `invalid_input` with the limit it
  reached and the start of its arguments, asking for shorter arguments,
  and the next step asks the usual cap. A response cut in text or
  thinking is still sent again at the output limit and then continued.
- Over Chat Completions, a response a provider reports as `tool_calls`
  while its native finish reason names the output limit, as OpenRouter
  does, is read as stopped at the output limit, and parallel tool calls
  whose arguments interleave are decoded one call after another
  (`latere.ai/x/pkg` v0.90.1).
- `GET /v1/sessions/{id}/stream?deltas=1` carries a session's live
  output while a response arrives, so a client shows thinking and text
  as the model writes them instead of waiting for the whole
  `agent.message`. Each is a frame of `event: delta` with no `id`, whose
  data is `{thread, turn, step, block, kind, text}`: `kind` is `text`,
  `thinking` or `tool_input`, and `text` is the next run of the content
  block `block`, to append to what the step's earlier deltas carried for
  it, at most 1024 bytes. `{thread, turn, step, reset: true}` says the
  step's request is sent again and what its deltas carried is
  discarded. `thread` is absent for the session's own thread, as on
  events. A frame without an `id` leaves a browser's last event id as it
  was, so a reconnect resumes the log where it was.
- Deltas are best effort. They reach a stream on any replica, whichever
  replica or runner process drives the session, joined per content
  block every 50 ms. They are never appended and never replayed, and a
  stream that falls behind loses deltas rather than slowing the turn.
  The step's `agent.message` stays the record: show a step's deltas
  until its `agent.message` arrives, and ignore a delta of a step whose
  message is already there.
- On Postgres, deltas cross replicas as notifications on the channel
  `topos_deltas`, which each replica's existing listener connection also
  listens on; a replica sends them from its serving pool one statement
  at a time, and opens no connection for them.
- The OpenAPI document describes the stream's frames, its `from_seq`
  and `deltas` parameters, and the `Delta` schema.
- `PATCH /v1/sessions/{id}` changes a session's reasoning effort as
  well as its model: `{"model": {"effort": "high"}}` keeps the model,
  `{"model": {"name": "...", "effort": "low"}}` changes both, and
  `"effort": ""` returns to the agent's own `spec.model.effort`. The
  effort is `minimal`, `low`, `medium` or `high`, holds across a change
  of the model, and takes effect at the next turn, in which a thread
  whose agent names no effort runs at it too. `session.model_changed`
  records it in `old` and `new`, each now `{name, effort}`, and the
  session's `model` carries the effort the next turn runs at.
- The authorizer's `session.update` question carries what the change
  names: `model` when the body names a model, and `effort`, the effort
  the next turn runs at, when it names an effort. An effort change alone
  carries no `model`, so an authorizer that requires one refuses it
  until it decides effort changes.

## v0.9.5 - 2026-09-30

- A trigger's `trigger.create` question carries the `trg_` id the
  trigger is stored under as its resource id, where it carried none, so
  an authorizer that delivers events to triggers knows a new trigger by
  the id `POST /v1/triggers/{ref}/fire` takes from its first apply.

## v0.9.4 - 2026-09-30

- Triggers start and continue an agent's sessions with no person
  present. `PUT /v1/triggers/{name}` applies a Trigger manifest that
  fires on `spec.schedule`, a five-field cron expression or `@hourly`,
  `@daily`, `@weekly` read in `spec.timeZone` (a local time a change
  to daylight saving time skips fires at the next valid minute, and
  one it repeats fires once), or on the events delivered to
  `POST /v1/triggers/{ref}/fire` that `spec.on` selects by product,
  verbs, resources and payload values. An event is one envelope
  `{id, product, verb, resource, actor, subject, time, payload}`; the
  core verifies no provider's signature and polls nothing, so whatever
  produces an installation's events calls the route.
- A trigger's `session.message`, `title` and `key`, and a repository's
  `url` and `ref`, are templates of `{{event.*}}`, `{{trigger.id}}`,
  `{{trigger.name}}`, `{{firing.id}}` and `{{firing.time}}`: path
  lookup and nothing else. Each value is cut at 16 KiB and marked
  `[cut]`, and a message past 64 KiB refuses its firing. An unknown
  root, a `{{` that opens no placeholder, and an event path in a
  schedule trigger are refused at apply as `invalid_manifest` with the
  field's path.
- `session.policy: new`, the default, starts a session per firing and,
  under `skipIfActive`, skips a firing while its key's session is
  active; `continue` sends each firing to the open session its key
  names, and holds it while that session waits on a confirmation or
  on its budget, sending it once the session runs on. `maxActive` (5
  by default, at most 100) bounds the trigger's active sessions, and
  `maxAge` skips a firing more than that late. A redelivered event
  answers its first firing and starts nothing; a failed one answers
  503 and runs again when it is delivered again. `GET
  /v1/triggers/{ref}/firings` lists the firings newest first, and a
  trigger's `status` carries `lastFiredAt`, `lastSessionId`,
  `nextFireAt` and one count per outcome.
- A firing's session is created by the code of `POST /v1/sessions`,
  with the trigger's owner, the person who applied it, as its
  initiator and `trigger:<trg_id>` as the sender of its messages, each
  of which names the firing as `firing_id`. Its `session.create`, and
  a continued message's `session.send`, are asked of the authorizer as
  the owner in the context the owner's token named at apply
  (`org_id`), with `trigger_id` and `firing_id` in the create; the
  fire route asks the new action `trigger.fire` of its caller. A
  trigger's firings act one at a time across replicas, so two
  deliveries of one key never start two sessions.
- `toposd serve` runs the minute loop that fires the schedules due and
  sends the held firings. On Postgres the store's migration 0004 adds
  the `triggers`, `trigger_firings` and `trigger_sessions` tables; the
  directory store keeps triggers under `objects/trigger*`.

## v0.9.3 - 2026-09-30

- An agent knows its session's repositories from its first turn. A
  hosted session's sandbox opens only when a tool first acts on it, and
  its repositories were named to the model only then, so an agent asked
  what it works on answered that it had none. Until the session's first
  machine is recorded, every request's context block now names each
  repository, the ref it starts from, the session's branch and the
  directory it is cloned into; the machine's own context block replaces
  it once the sandbox opens, and the sandbox still opens only when a
  tool needs it.
- A session's model can change between turns. `PATCH /v1/sessions/{id}`
  with `{"model": {"name": "..."}}` sets the model the session's next
  turn runs. The name is checked as a session's create checks its
  agent's model (below), so a switch takes every model a session could
  have been created with, then asked of the authorizer as the new
  action `session.update` with `session_id` and `model`, whose deny is
  `forbidden` with its reason. An allowed switch appends
  `session.model_changed` `{by, old, new}` and the Session carries
  `model`; a turn already running keeps its model, and the next turn
  runs on the new one with its own window, output limit and prices. The
  agent's own model's name switches back to the agent's model as it
  names it.
- A session's create checks that the installation runs its agent's
  model, by the rule the runner connects it with: `model_unknown` (422)
  for a model no source gives an input window and an output limit, and
  `model_unavailable` (503) for a gateway that does not answer, where
  such a session was created and failed its first turn. On an
  installation without `TOPOS_MODELS_KEY`, whose runners read Lux's
  doors with each session's own key, a model that goes through a Lux
  door passes and its figures are read at the turn. `toposd serve`
  reads the Lux root's doors at start whether or not it runs sessions
  itself.
- A person's message can carry images and files. A `user.message`'s
  `content` takes inline images, PNG, JPEG, GIF or WebP as base64
  `data`, at most 8 of at most 5 MiB each, which reach the model as
  image blocks when its figures say it takes images and as a note that
  it cannot see them otherwise, without failing the turn. Its
  `attachments` take files, `{name, media_type, data}`, at most 8 of at
  most 5 MiB each: the server stores each as a blob of the session and
  records `{name, media_type, size, blob, path}` with the path
  `attachments/<event id>/<name>`, in a directory of the message's own
  so that no two messages write one path, and the runner writes it
  there in the working directory when the machine opens, or before the
  next step when it is open, recorded as `attachments.delivered`; the
  model reads the paths after the message's content, and a repository
  excludes the directory from git. An image or a file past its limit is
  `attachment_too_large` (413). An image by URL, a block that is neither
  text nor an image, and an image whose bytes are not the format its
  media type names, which were stored as sent, are now
  `invalid_request`.
- The embedded model catalog knows which models take images, from
  OpenRouter's model list, the source of every figure of what a model
  is, and holds Gemini's models, on the OpenAI Chat dialect Lux's OpenAI
  door takes, so a message's image reaches the Claude, GPT and Gemini
  models that take images on any gateway, a Lux that lists its Models
  without their modalities included.
- An agent applied without `spec.machine` runs its hosted sessions on a
  Cella sandbox of the default image when the server has
  `TOPOS_CELLA_URL` and host sessions are off. The manifest's default
  machine is the host, which is a local run's, so such a session was
  refused at create with `machine_unavailable`. The same holds for an
  agent whose machine is `kind: host` alone, which its resolved spec
  cannot tell apart, and for agents applied before this release. With
  `TOPOS_HOST_SESSIONS=on`, or without Cella, such an agent keeps the
  host, and an agent that names any other machine field keeps its kind.
- A hosted session that ends in a drive deletes the Cella Secrets its
  sandbox mounted, the `-lux` key and the `-origo` git credential, after
  the sandbox, and stops their renewal first so none is applied again.
  They had outlived every session, since a Cella Secret has no lifetime of
  its own. A delete Cella refuses fails the release and is tried again at
  the next one. A session that ends outside a drive still leaves them;
  each carries `topos.latere.ai/session`, so the installation's host can
  sweep them.

## v0.9.2 - 2026-09-29

- A hosted session's `session.create` question names the session's
  repositories, its own or its agent's, as `repositories: [{type, url,
  ref}]`, so an authorizer that holds the git host's registry can grant the
  session read and write on exactly those, without the agent listing them in
  its permissions.
- A toposd or runner shutting down leaves a running hosted session
  running, with nothing appended, for the next runner to claim and
  resume. On a busy machine the stop could reach the turn before the
  session's log was closed to further writes, so the turn was recorded
  as `idle` with stop reason `interrupted` and every rollout interrupted
  the sessions it was running.
- A hosted session whose model neither the embedded catalog nor the agent
  gives a family, such as a provider's model a Lux installation routes,
  goes through the Lux root's OpenAI door, which serves every model Lux
  routes, and takes its context window and output limit from that door's
  model list. It had no door to ask, so it was refused as `model_unknown`
  though Lux served its figures.
- An agent takes an optional display name, `metadata.displayName`: the
  name a person reads, one line of any text up to 200 characters, while
  `metadata.name` stays the identifier routes and references use. It is
  stored with the agent and returned by the apply, every read by name
  or id, each version and the list; an agent without one reads exactly
  as before. A blank one, a longer one, or one holding a control
  character, a line break or a bidirectional control is refused as
  `invalid_manifest` at `metadata.displayName`. The display name, like
  labels and annotations, is not part of the spec: changing it creates
  no version and moves no digest. Such an apply used to answer the new
  metadata and store none of it; now it replaces the latest version's
  metadata, so the next read returns what was applied.
- A `forbidden` answer says why. It keeps its code and its sentence and
  now carries the installation's authorizer's reason for the deny in
  `details.reason`, such as `agents_not_enabled`,
  `agent_budget_unassigned` or `agent_exceeds_initiator`, so a client
  can tell the person what to do. A deny without a reason, or whose
  reason is not a snake_case token, carries none, and `details.detail`
  reads `the authorizer denied <action>` without it. A session create
  of another subject's agent passes its reason only when the caller may
  read that agent, since the reason may be about it; a denied read still
  answers `not_found` with no details. The authorization wire carries
  only the reason, so which permission exceeded the initiator's is not
  yet part of the answer.

## v0.9.1 - 2026-09-29

- An agent's name is unique per owner. It was unique across the whole
  installation: once one person applied `coding-agent`, nobody else
  could, and the refusal told them that someone's agent of that name
  existed. Now each subject has their own names. An apply creates or
  updates the caller's own agent of the name; `GET`, `versions` and
  `archive` by name, a session create's `agent` and a session list's
  `agent` filter read the name among the caller's own agents, as a
  manifest's references do. A name only another subject holds answers
  `not_found`, or `unknown_reference` in a manifest, exactly as a name
  nobody holds. An `agent_` id still names its agent whoever owns it,
  and the authorizer decides as before; an administrator reaches
  another subject's agent by its id, and an administrator's apply by
  name acts on the administrator's own agent. An organization's agent
  is named within the subject that applied it. On Postgres, migration
  0003 replaces the agents table's `UNIQUE (name)` with
  `UNIQUE (owner, name)`; every stored agent already satisfies it, so
  the migration keeps every row. A toposd of v0.9.0 refuses to start on
  the migrated schema; the down migration restores the old constraint
  while no two owners hold one name.

## v0.9.0 - 2026-09-29

- A session's `budget.spent_cost_usd_micro` reports what it has spent:
  every store adds each `model.request`'s cost to the session as the
  event is appended, and the directory store writes the session when a
  request's cost arrives, not only at a status change. It stayed 0 for
  every session, so the console and the API showed nothing spent, though
  the budget ceiling itself was enforced from the log.
- A model request asks at most 8192 output tokens as its `max_tokens`,
  or the model's output limit when that is lower, rather than the whole
  output limit. A gateway that reserves a request's `max_tokens` at the
  output price against the caller's budget before running it, as Lux
  does, held back many times what a step spends, so an agent on a small
  budget was refused with `budget_exhausted` after its first request. A
  response that stops at 8192 tokens is sent again once at the model's
  output limit, in its place, and one that stops at the output limit is
  continued as before. Every `model.request` records the `max_tokens` its
  request asked; a response that stopped at 8192 tokens leaves a
  `model.request` with outcome `escalated`, whose cost counts, and no
  `agent.message`.
- A session's first repository is fetched into the working directory as
  it is instead of cloned into it, so a sandbox whose workspace volume
  starts with `lost+found` gets its repository; `lost+found` is excluded
  from git, and a fetch that fails leaves the directory as it found it.
  A clone refused the non-empty directory, so no Cella session received
  its repository. Every open of a Cella sandbox also names the workspace
  a safe directory in git's global configuration: the volume's root
  belongs to another user than the sandbox's process, and git refused to
  work in it.
- The Cella Secrets a hosted session's sandbox uses, its `-lux` model key
  and its `-origo` git host token, carry the session's and the agent's
  labels, `topos.latere.ai/session` and `topos.latere.ai/agent`, as the
  sandbox does, at their create and at every renewal. An authorizer that
  binds a session's token to the session refused the first Secret, since
  it named no session, so no sandbox opened; `TOPOS_CELLA_LABELS` still
  joins them for a self-hosted installation.
- A Cella sandbox's `HOME` is the machine's own directory,
  `TOPOS_MACHINE_DIR/home`, made on every open, unless the agent's
  machine environment names one. Cella mounts the image's root filesystem
  read-only, so writing git's global configuration into the image's home
  directory failed and a session's first machine never opened; git,
  package managers and build caches now write there.
- An Agent manifest names the repositories its sessions work in as
  `spec.repositories`, a list of `{url, ref}` with `ref` optional, checked
  as a session's are: at most 8, each an `https` URL that names its host
  and holds no credential, and a ref that is a branch, a tag or a commit.
  A session whose create names no repositories works in those of the
  agent version it pins; one that names its own works in those alone. A
  subagent's repositories are not used, and a local `topos run` refuses an
  agent that names any.
- The `session.machine` of a session's first machine lists the
  repositories delivered into it as `repositories: [{url, branch,
  commit}]`, `commit` the HEAD checked out on the session's branch, so a
  reader of the log sees which checkout the agent started from.
- A refused repository URL that holds a credential is no longer quoted
  in the error.
- An installation without an identity provider gives its sandboxes a git
  credential of its own: `TOPOS_ORIGO_TOKEN_FILE` names the file holding
  it, and each Cella sandbox's git sends it to the host of
  `TOPOS_ORIGO_URL` through the `ORIGO_TOKEN` Secret that Cella's egress
  gateway swaps in, so a session clones and pushes its repositories there
  without the credential entering the sandbox. The file is read at each
  sandbox's open. It needs `TOPOS_ORIGO_URL` and is refused beside
  `TOPOS_IDENTITY_URL`, whose sandboxes push with their agents' tokens.
- `TOPOS_CELLA_LABELS` names `key=value` labels that every sandbox and
  every sandbox Secret carries beside the session's and the agent's, for
  a Cella whose authorizer places what a caller creates by labels, such
  as its tenant.

- **Breaking:** the repository restarts as the Topos core. Every package of
  v0.7.0 is removed: the root `topos` package, `graph`, `billing`, `harness`,
  `models`, `runtime`, `sandbox` and `adversarial`. A program pinned to v0.7.0
  or earlier keeps building, because those versions stay in the Go module
  proxy; the v0.7.0 tree is at its tag, and its specs are under
  `docs/history/`. The adversarial review engine continues as its own module,
  `latere.ai/x/adversarial`.
- `session` is the session log of schema v1: the `Session` header, the
  typed events a session appends, and `Fold`, which renders one thread's
  transcript as Lux wire messages and is byte-identical for the same log.
  `session.NewMemoryStore` keeps sessions in memory, and `session/dir`
  keeps each in a directory with an fsync before every acknowledged append
  and a single-writer lock held with `flock`. `session/storetest` is the
  suite every `session.Store` passes.
- `models` is the model boundary: a `Connection` that must name a base URL
  and a model, the `Model` a request streams through, retry classification
  of HTTP answers, stream errors and cut streams, a catalog of model
  windows and prices built into the binary, and exact cost in micro-USD.
  `models/dialect` sends requests over HTTP in the Anthropic Messages,
  OpenAI Responses and Chat Completions wire formats through the
  llmdialect codecs, and reports a stream the server closed before its
  last frame as incomplete, never as a finished answer.
- `machine` is where a session's tools run: the `Machine` interface, the
  credential deny-list (home credential files, `.env` files, key files,
  and secret-named environment variables), and grep and glob that honor
  `.gitignore`. `machine/host` is the person's own computer: file access
  confined to the session's roots through `os.Root`, each command in its
  own process group, killed on timeout, on cancel and when its shell
  exits, and background jobs that end with the session. A command longer
  than 64 KiB, such as a long heredoc, runs from a script file, since
  Linux refuses a single argument past 128 KiB.
- `harness/tools` holds the built-in tools: `read`, `write`, `edit`,
  `bash`, `grep`, `glob`, `web_fetch` and `todo`. `write` and `edit` refuse
  a file that changed since the thread last read it, `bash` keeps its
  directory between calls and runs servers as background jobs, and output
  past 32 KiB goes to a spill file with its head and tail kept. The
  registry validates every call against the tool's JSON Schema before it
  is scored.
- `harness` runs one agent's turns over a session: no step cap, the
  model's own output limit from the catalog, retries with backoff that
  honor `Retry-After`, thinking replayed with its signature, a risk score
  and a verdict (`allow`, `flag`, `ask`, `block`) on every call under the
  `plan`, `confirm` and `progressive` modes, confirmations that survive a
  restart, and old tool results cleared and the conversation summarized
  when a session nears the model's window.
- `runner` drives a session in process: it holds the session's lock,
  attaches the machine with the context block, the project's `AGENTS.md`
  and `CLAUDE.md` files and its skills index, and starts the next turn at
  once when a message arrived during the last one.
- `models/scripted` plays a YAML script as a model, for tests; a
  connection names it with the `scripted:` scheme.
- `topos run` runs a turn of a local session in the working directory,
  and `topos confirm` answers a pending call and continues it. The exit
  code says how the turn ended: 0 done, 1 error, 2 usage, 3 waiting for a
  person, 4 at a limit, 5 interrupted.
- Every turn ends with a checkpoint: a git commit of the working
  directory's tree under `refs/topos/checkpoints/<session>/<turn>`, written
  through a temporary index so HEAD, branches and the index are never
  touched, with credential files and files over 100 MiB left out. Outside
  a repository the objects go to a session repository under the data
  directory. `topos rewind <session> <turn>` restores the working
  directory to a turn's checkpoint, saving the current state first.
- An agent with subagents gets the `spawn` and `message` tools. A spawned
  thread runs a subagent on the same machine with its own model and
  instructions, the tools it is granted narrowed from its parent's, and
  the stricter permission mode; it answers with its final text and keeps
  taking work through `message`. Several spawns in one step run at once,
  threads nest two deep by default, and a thread waiting for a
  confirmation pauses the session until the answer arrives.
- A thread spawned with `isolation: worktree` works in a git worktree of
  its own on the branch `agents/<agent>/<session>.<thread>`; each of its
  turns is committed there, and the result names the branch and commit
  for the parent to merge.
- An agent configured with an advisor gets the `advisor` tool: a stronger
  model that sees the conversation so far, acts on nothing, and answers
  with advice.
- An interrupt cancels the model request in flight and the running tool
  calls at once, instead of waiting for the next step.
- A redacted message no longer stops a session: the next turn first
  summarizes the conversation up to it from a transcript that leaves the
  redacted content out, so the removed value never reaches the model
  again.
- Sessions can live in Postgres: `TOPOS_DB_URL` names the database the
  migrations and change notifications use, and `TOPOS_DB_POOL_URL`, when
  set, serves queries through a transaction-pooling proxy. An append is
  one transaction, watchers follow `LISTEN` with a poll as the fallback,
  and a session's one-writer lease expires and is taken over when its
  holder stops renewing it. `go test -tags postgres` runs the store's
  tests against `DATABASE_URL` or a Postgres container it starts.
- `authorizer` is the vocabulary an installation's authorizer is written
  against: the 33 actions toposd asks, each with the kind it acts on
  (`agent`, `session`, `trigger`, `credential`, `memory_store`), as the
  `authz.Vocabulary` the shared client, endpoint scaffold and conformance
  suite read, and the `limits` an allow of `session.create` may carry,
  with `DecodeLimits` to read them as toposd does.
- toposd verifies bearer tokens from the issuers `TOPOS_OIDC_ISSUERS` lists,
  for an audience of `TOPOS_OIDC_AUDIENCE` (`topos` by default), and
  refuses a token older than a day or without `iat`. An issuer whose key
  set cannot be read at start stops the start, and an `http://` issuer is
  refused unless it is loopback or named in `TOPOS_OIDC_INSECURE_ISSUERS`.
  `TOPOS_PUBLIC_URL` is now required to serve.
- With `TOPOS_AUTHORIZER_URL` unset, toposd decides with the owner policy:
  the subjects of `TOPOS_ADMIN_SUBJECTS` act on every object, the creator
  of an object on it, and everyone else is denied, narrowed further by a
  key's grants. `TOPOS_AUTHORIZER_URL` without `TOPOS_AUTHORIZER_TOKEN` is
  a configuration error.
- `toposd token` signs a token with the local issuer's key,
  `TOPOS_LOCAL_ISSUER_KEY` (a PEM PKCS#8 ECDSA P-256 or RSA key), and
  prints it: `--subject` (default `admin`), `--ttl` (default `1h`, at most
  `24h`) and `--audience`. It opens no store. With the key set, toposd
  accepts those tokens and serves the key set at
  `/.well-known/jwks.json` under its public URL, so a self-hoster needs
  no identity provider.
- `manifest/v1` holds the four kinds of the `topos.latere.ai/v1` API
  group, `Agent`, `Trigger`, `MemoryStore` and `Connection`, and
  `manifest.Resolve` is the one resolver of them. A manifest is refused
  as a whole with every problem and its field path: an unsupported
  version, unknown fields, wrong types, out-of-range values, a secret
  pasted into a free-text field, and an unknown reference. The resolved
  spec has its fixed defaults written out and a digest that stays the same
  on every build; applying an unchanged spec keeps its version.
- `topos run --agent <file>` runs an agent from a manifest: its model,
  instructions (inline or from `instructionsFile`), tools, approval mode,
  lists and thresholds, egress, subagents, thread limits, budget and
  limits. With no `--agent`, `$XDG_CONFIG_HOME/topos/agent.yaml` (or
  `~/.config/topos/agent.yaml`) runs when it exists. The session keeps the
  resolved agent, so `--session` and `topos confirm` continue with the same
  agent after the file changes. `--model` and `--mode` override the
  manifest. A field a local run cannot apply yet, such as hooks, MCP
  servers, memory stores or a Cella machine, is refused rather than
  ignored, and every refusal exits 2 before a session is created.
- `--mode` no longer defaults to `confirm` at the flag: continuing a
  session without it keeps the mode the session was created with, where it
  was silently reset to `confirm` before.
- `test/tasks` is the task suite: each task is a directory with a prompt,
  its starting files and a checker, run in process through the runner and
  the harness. The tasks cover the failures of v0.7.0 (more than 16 steps,
  an answer past 4096 tokens, a subagent reading its parent's file,
  absolute paths, the person's `PATH`), ordinary coding work, and one
  instruction test per built-in tool. Every task is proven against a
  scripted right and wrong solution; `go test -tags tasks ./test/tasks/`
  runs the suite against the model `TOPOS_MODELS_URL`, `TOPOS_MODELS_KEY`
  and `TOPOS_TASKS_MODEL` name and writes a JSON and Markdown report.
- A subagent's own reasoning effort from its manifest reaches its requests,
  where it took the parent's.
- `prompts` holds every text a model reads as a versioned file: the
  harness prompt, the compaction prompt, the tool descriptions, the tool
  results, the transcript's framing and the context block, each rendered
  from `prompts/<area>/<name>-v<N>.md`. A released file never changes; a
  new wording is a new version.
- `machine/cella` runs a session's tools in a Cella sandbox through
  `latere.ai/x/cella/client`: one sandbox per session named `ses-<ulid>`,
  found by name or created and held until it runs, with the session's
  labels, an egress allowlist that includes the named secrets' hosts, and
  a persistent lifecycle (stops after 15 minutes idle, never deleted until
  the session ends). Commands run through `topos-machine`, a static helper
  the machine uploads into the sandbox, which gives each command its own
  process group, a timeout, an input that ends and its final directory,
  and runs grep and glob where the files are. A sandbox that is gone
  answers `machine_lost`, and Cella's refusals `machine_unavailable`. The
  machine needs a Cella Environment that offers the Attach capability,
  because every command runs over the exec socket; Cella's `local` driver
  does not. `test/stubs/cellastub` is the stub Cella its tests run
  against.
- `toposd serve` answers the API under `/v1`: `PUT /v1/agents/{name}`
  applies an Agent manifest and makes a new version only when the spec
  changed, and agents are listed, read by name or id, read at a version
  and archived. `POST /v1/sessions` creates a hosted session of an
  agent's version with an optional first message, its budget, turn
  timeout and age the lowest of the request's, the agent's and the
  authorizer's; sessions are listed by agent, status and runner, read,
  ended and deleted. `POST /v1/sessions/{id}/events` sends a user event
  with the verified subject as its sender, and
  `GET /v1/sessions/{id}/stream` replays the log as Server-Sent Events and
  follows it live from any replica, resuming after `Last-Event-ID`.
  Every route asks the authorizer its action, a denied read answers
  exactly as a missing object does, every error is one JSON envelope with
  a code, lists page with `cursor` and `next_cursor`, every `POST` takes
  an `Idempotency-Key`, and each subject may make 600 requests a minute.
  The OpenAPI document is `api/openapi.yaml` and is served at
  `/v1/openapi.yaml`. No runner claims a hosted session yet, so its turns
  wait for the runners to come.
- toposd keeps agents, their versions and idempotency records in both
  storage modes. Without `TOPOS_DB_URL` they are JSON files under
  `$TOPOS_DATA_DIR/objects/`, each written atomically, sessions live under
  `$TOPOS_DATA_DIR/sessions/`, and one `toposd serve` holds the directory:
  a second is refused with `data directory in use by pid <n>`. On Postgres
  a new migration adds the `agents`, `agent_versions` and
  `idempotency_keys` tables; replicas that race on one agent version or
  one idempotency key store it once. Readiness checks the store.
- A hosted session runs on a Cella machine: an agent whose
  `machine.kind` is `host` is refused `machine_unavailable` by the server,
  and the owner policy lets a subject start sessions of its own agents
  only.
- A reasoning model served over OpenAI Responses keeps its reasoning
  across turns: the harness asks for the encrypted reasoning items, keeps
  each as an opaque block in the session log, and sends it back verbatim
  on the next request (`latere.ai/x/pkg` v0.88.0).
- `write` and `edit` refuse an existing file the thread never read with
  a result that says so, `<path> exists and this thread has not read it;
  read it before writing to it.`, where they said the file had changed
  since it was last read; the tools' descriptions (version 2) name both
  refusals and ask for a `read` first.
- The model catalog also knows each model by the name the hosted Lux
  serves it under, with dots in its version (`anthropic/claude-haiku-4.5`
  beside `anthropic/claude-haiku-4-5`).
- `toposd serve` runs hosted sessions: up to `TOPOS_RUNNER_CAPACITY` (16)
  of them at once, claimed from its own store as soon as a session gets a
  message. Each runs its agent from the session's bundle, on a Cella
  sandbox created at `TOPOS_CELLA_URL` with the bearer in
  `TOPOS_CELLA_TOKEN_FILE`, against the model at `TOPOS_MODELS_URL` with
  `TOPOS_MODELS_KEY`, which serve now requires. The image carries the
  `topos-machine` helper for `linux/amd64` and `linux/arm64`. A turn that
  cannot start closes with a `session.error` naming why
  (`model_credential_missing`, `machine_unavailable`, ...), and a server
  that stops mid-turn leaves the session for the next runner to resume.
  On Postgres a runner writes through its lease, and a batch from a runner
  whose lease another replica has taken over is refused with
  `lease_lost`, so two runners never interleave in one session.
- `toposd runner` runs hosted sessions as a separate deployment: it claims
  from the internal listener at `TOPOS_INTERNAL_URL` with the first bearer
  of `TOPOS_RUNNER_TOKEN`, keeps each claim's lease by renewing it, and
  writes the log only through it. `toposd serve` mounts the runner routes
  on its internal listener when `TOPOS_RUNNER_TOKEN` is set, frees the
  claims of a runner that stopped renewing, and with
  `TOPOS_RUNNER_CAPACITY=0` runs no runner of its own.
- A model gateway's refusal for spend, `budget_exhausted` or
  `spend_exceeded` (Lux sends it as HTTP 429), is no longer retried like a
  rate limit: the turn stops with `budget` and a `session.error` naming the
  refusal, where it was retried six times and ended as `model_error`.
- A tool confirmation or a client tool's result is accepted only for a
  call waiting for that answer, once; a second answer, or one naming a
  call that never asked, is `conflict`. Only events that carry content
  (messages, tool calls and results, summaries) can be redacted, so a
  redaction can no longer erase a verdict or a confirmation, or reset a
  session's spend by removing its model requests.
- On Postgres, toposd holds one `LISTEN` connection per replica on
  `TOPOS_DB_URL`, however many event streams and runners watch sessions,
  where each stream and each runner's interrupt watch opened its own. A
  replica uses its serving pool plus that one connection, and one more at
  start for migrations. A dropped listener reconnects with backoff while
  watchers poll every 2 s, so no event is missed. A subject's 17th
  concurrent `GET /v1/sessions/{id}/stream` on a replica is refused
  `rate_limited` until one of its streams ends.
- `TOPOS_MODELS_URL` may name a Lux gateway's root, such as
  `https://lux.example/v1/models`: toposd and `topos` read its discovery
  document and send each model to its own family's door, Anthropic models
  to `/anthropic` and OpenAI models to `/openai`. A URL that names a door,
  and a provider's API, are used as before. A server that cannot reach a
  URL naming no door refuses to start.
- `session.create` tells the authorizer the permissions of the agent
  version the session pins, so an installation can refuse a session whose
  agent may do more than the person starting it.
- The directory store takes its single-writer lock on Windows with
  `LockFileEx`, as it does with `flock` on Unix.
- `topos` builds for Windows. On a Windows host each command runs in a Job
  Object of its own, which a timeout, a cancel or the session's end
  terminates with every process the command started; a cancel ends the
  command at once, since Windows has no SIGTERM. Commands run under `sh`
  on `PATH`, or the `sh.exe` of Git for Windows beside `git`, and are
  refused with that remedy when there is neither. A Windows host records
  `sandbox: none`, since it has no host sandbox, and a session there that
  asks for `progressive` is refused with `sandbox_unavailable`; `plan`
  and `confirm` run. With `HOME` unset, `USERPROFILE` places the data and
  configuration directories and the credential deny-list. `toposd` with
  `TOPOS_HOST_SESSIONS=on` refuses to start on Windows.
- The credential deny-list of the file tools and search matches without
  regard to case, so `~/.SSH/ID_RSA`, `SERVER.PEM` or `.ENV` on a
  case-insensitive filesystem, macOS's and Windows' default, is refused
  like the entry it spells; before, such a path read the file. On a
  case-sensitive filesystem a name that differs from an entry only in
  case is now refused too.
- A hosted session records its approval policy at create: the agent's
  `spec.approvals` merged with the lists and thresholds the authorizer's
  allow carries, the confirm lists joined, the allow lists narrowed to
  what both allow, each threshold the lower. Every runner decides the
  session's calls by that policy.
- `topos run` in a git checkout another session still owns writes a
  worktree of its own, under the data directory on
  `agents/<agent>/<session>`, instead of the checkout, so two sessions
  never write one working directory. At a session's end its worktree is
  removed, and its branch kept, when it is clean and merged or pushed.
- `toposd serve` ends every idle session past its maximum age as
  `expired` and deletes every ended session past the retention its
  authorizer gave it, every ten minutes.
- A model reached through a Lux door takes its window, output limit and
  prices from the door's own model list where the list gives them,
  before the built-in catalog's and after the agent's own `spec.model`
  figures, so a model the catalog does not know runs when Lux serves it.
  A door that does not answer its model list fails the turn's setup with
  `model_unavailable`.
- Cella refusing a session's sandbox, or a command in it, because the
  session's allowance is spent (`budget_exhausted` or `spend_exceeded`)
  stops the turn with `budget` and a `session.error` naming the refusal,
  as a model gateway's refusal does, where a refused create was
  `machine_unavailable` and a refused command a failed call the model
  retried. `POST /v1/sessions/{id}/resume` continues the session once
  the allowance is raised.
- Every `model.request` records `fold_seq`, the log's last sequence when
  its request was built, and `models.Replay` builds every request of a
  recorded session again, on its own thread, re-encodes it with the
  current build and compares its hash with the recorded one, reporting
  each step `match`, `mismatch`, `codec_mismatch` or `skipped`.
- `TOPOS_BLOB_URL` keeps raw model responses and captured requests
  outside the database or the data directory: in a directory,
  `file:///<path>`, or in an S3 compatible object store,
  `s3://<host>/<bucket>/<prefix>` with `TOPOS_BLOB_ACCESS_KEY` and
  `TOPOS_BLOB_SECRET_KEY`. A deleted session's objects go after its
  records, and the reaper removes objects a failed delete left behind.
- **Breaking:** an Agent manifest no longer takes `spec.identity`, and one
  that names it is refused with that field's path: the agent's owner, a
  person or an organization, decides whose authority it acts with. An
  agent or a session stored by an earlier build of this release whose
  agent carries the field no longer reads; apply the agent again.
- With `TOPOS_IDENTITY_URL`, every agent applied is an identity at the
  installation's identity provider, owned by the organization the
  authorizer's allow names for it (the limits member `owner`), or else by
  its applier, and holds no key. Its hosted
  sessions reach Cella with tokens minted for the agent, at most 15
  minutes long and naming the session, and a session's sandbox pushes to
  `TOPOS_ORIGO_URL` with its own token, which the sandbox holds only as a
  placeholder that Cella's egress gateway swaps in. Archiving the agent
  archives its identity, which is disabled for good once the agent's last
  session ends.
- With `TOPOS_SESSION_KEYS_URL`, each hosted session asks models with a
  Lux key of its own, and its sandbox holds a second one as `LUX_KEY`
  beside `LUX_URL`; toposd generates both and sends the authorizer only
  their hashes. `toposd runner` reaches a session's tokens and keys from
  its server, only while it holds the session's lease.
- A hosted session's Cella sandbox is created when the agent first calls
  a tool that acts on a machine (`read`, `write`, `edit`, `bash`, `grep`,
  `glob` or `web_fetch`), and its `session.machine` is recorded then, so
  a session that only talks has no sandbox and costs none. A session
  whose sandbox exists reopens it by name when a runner claims it. A
  sandbox that cannot be created answers the call that needed it, with
  a `session.error` naming the code, and a later call tries again.
- `POST /v1/sessions` takes `resources`, up to 8 repositories as
  `{"type":"repository","url","ref"}` with `https` URLs. When the
  session's machine first exists the runner clones the first into the
  working directory and the rest beside it, each on the branch
  `agents/<agent>/<session>` from `ref`, with the agent as the author and
  a hook that adds `Topos-Session` and `Topos-Agent` trailers to every
  commit; `git push` publishes the branch. A repository that cannot be
  cloned is reported as `repository_unavailable` and the session goes on
  without it.
- A `v*` tag publishes the image `ghcr.io/<owner>/topos` for `linux/amd64`
  and `linux/arm64`, under the namespace of the account that pushed the
  tag: `toposd` and the `topos-machine` helper builds on the distroless
  base, the same runtime as the developer image, signed and with its bill
  of materials and provenance attested. The image is pushed by digest,
  tagged with the release's tag only once it reports that tag's version,
  and the release notes follow.
- With `TOPOS_MODELS_URL` a Lux root, the runners reach each door Lux's
  discovery document names under that URL, so an installation that
  points it at Lux's in-cluster address keeps every model request inside
  its network even though Lux publishes its doors under its public URL.
  A sandbox's `LUX_URL`, the host its Lux key is swapped in for, and the
  host its egress admits are that published root, since a sandbox
  reaches only public hosts.
- `TOPOS_BASE_PATH` serves the API under a prefix of an origin toposd
  shares with other services, in the place of `/v1`: with `/v1/agents`
  and `TOPOS_PUBLIC_URL=https://api.example.com/v1/agents`, agents are at
  `/v1/agents/agents`, sessions at `/v1/agents/sessions`, and the document
  at `/v1/agents/openapi.yaml`, whose server is the public URL. The base
  path must equal the public URL's path, and a public URL with a path
  needs one, so a mismatch stops the start instead of writing URLs toposd
  does not answer on. A session's stream `Link`, a list's next-page
  `Link` and the document's server are built from the public URL, never
  from the request's host. A path the API does not route, outside the
  base path as well as under it, answers the `not_found` envelope; the
  probes and the build identity stay at the listener's root.

## v0.7.0 - 2026-09-26

- **Breaking:** `sandbox/cella` speaks to a Cella control plane, the open source
  runtime hosted at `https://api.latere.ai/v1/environments`, through its
  exported client `latere.ai/x/cella/client`. The hosted service at
  `cella.latere.ai` that it spoke to before is retired. Set
  `Options.BaseURL` to the control plane's address including that path.
- `Create` holds the request until the sandbox runs, so the first command can
  follow at once. A sandbox that fails to start is deleted and `Create` returns
  an error naming it and the reason.
- A created sandbox reaches only `api.latere.ai`, where the hosted models are
  served, unless the new `Options.AllowedHosts` names the hosts it may reach.
  The image defaults to `base` from the control plane's catalog. An `ephemeral`
  sandbox is deleted 24 hours after it was created; every sandbox still stops
  after 15 minutes idle.
- `Exec` returns the command's standard output followed by its standard error
  in `Stdout`, each cut at a mebibyte, and the command's standard input is at
  end of file. `StreamExec` delivers the same output as one chunk when the
  command ends.
- **Breaking:** the Cella provider refuses `CreateOptions.Policy`, a non-empty
  `CreateOptions.SecretMounts` and `ExecOptions.SecretEnv`, which have no
  counterpart on the control plane, instead of sending them.
- `examples/sandbox` runs on hosted Cella when `TOPOS_CELLA_TOKEN` is set, at
  `https://api.latere.ai/v1/environments` unless `TOPOS_CELLA_URL` names
  another address.
- A Lux usage figure the gateway did not measure counts as zero.

- `ModelLux` with no `BaseURL` reaches `https://api.latere.ai/v1/models`,
  Latere's Lux core under the platform origin. The default was
  `https://lux.latere.ai`, the retired hosted gateway, whose host no longer
  resolves. A `BaseURL` set by the caller is unaffected.

## v0.6.0 - 2026-09-23

- **Breaking:** the PreToolUse hook payload names its validated tool input
  `normalized_input`, and the Go field is `PreToolUsePayload.NormalizedInput`.
  A hook that reads `normalised_input`, or a modified payload that sets it,
  must use the new key; a payload stored before this release carries the old
  one.

## v0.5.0 - 2026-09-18

- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and the
  decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0). Nothing
  changes for a user of Topos.

- The Cella provider's documentation describes the credential that exists.
  Cella issues no bearer of its own: present the short-lived token your issuer
  mints for the audience `sandboxd`. Because that token lives for minutes
  rather than days, a run that outlasts one token needs `TokenFunc`, which is
  asked per request, rather than a bearer bridged once at run start. No code
  changed; the `TokenSource` interface and its three implementations are
  unchanged.

- Agent tool grants now restrict the available tools and their execution,
  including delegated agents. Traces show the concrete tools offered.
- Native critics have no tools by default. Explicit grants enable selected
  tools; persisted empty grants remain empty after a JSON round-trip.
