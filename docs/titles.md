# Titles: a session's title that follows its conversation

A session that stays open until a person deletes it is one long
conversation, and its subject moves: a person who asked about a release
note goes on to a migration, then to a test that fails. A list of
sessions shows each by its title, so the title is how a person finds the
conversation again. Without help, the title is whatever the create gave,
often a client's cut of the first message, and it changes only when the
person renames the session.

With titles on, the harness keeps the title on what the conversation is
about now. After each turn a person started, it asks a small model
whether the title still names the subject; the model keeps it, or gives
a new one, which becomes the session's title. A person's own rename
always wins. This page is for whoever writes an agent's manifest, runs an
installation, or builds a client that lists sessions.

## Turning it on

Add `title` to the agent's spec:

```yaml
apiVersion: topos.latere.ai/v1
kind: Agent
metadata: {name: assistant}
spec:
  model: {name: anthropic/claude-sonnet-4.5}
  title:
    model: tier/quick
```

| Member | |
|---|---|
| `title` | present, even as `title: {}`, turns titles on |
| `title.model` | the model the title request asks for, such as a small, fast model or a routed name. Left out, the request asks for the model the session runs, by the routed name it was asked by when it has one. An empty `model` is refused |

The setting is read from the agent's latest version, not from the version
a session runs: naming a session changes nothing the agent does, so a
new version that turns titles on names the sessions the agent already
runs, and one that turns them off stops. A runner that cannot read the
agent's latest version reads the version the session runs.

## When a title is asked for

After a turn ends, the session is named when all of these hold:

- the agent's latest version sets `title`;
- the turn left the session idle, not ended;
- the turn's input held a message a person sent: a turn a trigger started
  keeps the name its routine gave the session;
- no message was waiting when the turn ended: the next turn asks instead;
- no person ever changed the session's title, a fork's copied rename
  included: a person who named the conversation keeps the name;
- the session's budget, where it has one, is not spent.

The request runs once the turn's closing status is in the log, so a
client sees the turn end at once and the title a moment later. A message
sent while the title is asked for starts the next turn straight after,
on the same runner.

## What the model reads

One short request, with no tools: the session's current title when the
harness gave it, the person's latest six messages, each up to 2000
characters with the names of its files, and after each the start of the
agent's answer, up to 600 characters. Reasoning, tool calls and results, summaries and removed
events are left out. The model answers `KEEP` or a title of a few words,
in the person's language, as a heading, with no quotes, no closing period
and no emoji, at most 60 characters. It keeps the title unless both of
the person's latest two messages are about something the title does not
cover, so a question on the side that the conversation comes back from
does not rename it. The first time a session is named, there is no title
to keep, and the model names the conversation.

An answer of `KEEP`, an empty one, one of two lines or with a control
character, one longer than 60 characters, and the title the session
already has, change nothing. A model that reasons before it answers and
spends the request's few tokens on that is asked once more with room to
reason.

## The model it runs on

Before the request, the installation's authorizer is asked
`session.title`, as the person who started the session, with the name
the request asks for, the model the session stands on, and the current
title. The authorizer may answer with the model to use; the request then
runs on it when the installation runs it, and otherwise on the name
asked. When the agent names a title `model` and neither runs, no request
is sent: a title never moves to the session's own model, which may cost
far more, behind the agent's back. When the agent names none, the
session's own model is the last choice.

A deny, and an authorizer that does not know `session.title`, leave the
title as it is. An installation that wants fewer titles denies. With no
authorizer configured, the built-in owner policy allows the person who
started the session.

## What a client sees

A new title is a `session.title_changed` event, `{by, old, new}`, whose
`by` is the harness, `{"subject": "service:harness", "kind": "service"}`,
and the session's `title` is the new one. It follows a `model.request`
whose `purpose` is `title` in the same batch. Neither moves the session's
`updated_at`, so a list ordered by its last change does not reorder when
a title moves. No delta of the title request is streamed: a client sees
the title, not the model writing it.

The request's cost is the session's, as any request's is, and counts
toward its budget. A request that fails, is denied or times out, within
20 seconds, writes no error to the session and changes no status; the
runner logs it.

## The sessions an agent ran before

When toposd runs sessions itself, a minute after it starts it names,
once, the idle sessions of every agent whose latest version sets `title`
that were never named: those whose log holds a person's message, no
title request, and no title a person gave, a fork's copied events
included, so the continuation of a named conversation is not named
again. It names one session at a time, two seconds apart, skips a
session another runner holds, and logs how many it named, kept, skipped
and failed. A session it could not name is tried again at the next
start.
