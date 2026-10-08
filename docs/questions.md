# Questions: showing an agent's question and sending the answer

An agent that holds the `question` tool can put a decision to the person
it works for: one call asks several questions at once, each with a few
options, and the person may always answer in their own words. This page
is the contract a client follows to show such a question and send the
answer. Everything here is on the wire of `/v1`; the field names are the
API's, and the bounds are those `api/openapi.yaml` states under
`QuestionInput` and `UserAnswer`.

## Who answers

A session waits for an answer only when its creator says a person
answers it. Set `attended` when you create the session, and again when
you fork one:

```http
POST /v1/sessions
{"agent": "release-notes", "message": "Set up the storage.", "attended": true}
```

```http
POST /v1/sessions/ses_01.../fork
{"attended": true}
```

Set it only when your client shows questions and a person is there to
answer them. A session created without it never waits on a question:
the agent's question is answered at once, the agent decides, and it
says what it assumed in its final message. `attended` is fixed at
create; nothing changes it later. An attended session whose person does
not return holds its question until the session expires at its
`limits.max_age`; nothing times a question out, and the server notifies
nobody, so telling the person that a session waits for them is your
client's.

An agent holds the tool only when its manifest names it:

```yaml
spec:
  tools: [read, write, edit, bash, grep, glob, web_fetch, todo, question]
```

## Finding the open question

When the agent asks, the session goes idle with `stop_reason`
`question`, which `GET /v1/sessions` shows; `GET /v1/sessions/summary`
counts such a session as `idle`. Do not rely on the stop reason to find
the question: when a
call that needs a confirmation waits at the same time, the stop reason is
`tool_confirmation`. Read the log instead.

The open question is the `agent.tool_use` whose `payload.name` is
`question` that nothing closed. A question is closed by the first of
these that follows its `agent.tool_use` in the log:

| Event | Closes it as |
|---|---|
| a `tool.result` with its `tool_use_id` | done; `meta.closed_by` says why |
| a `user.answer` | answered |
| a `user.message` whose `sender.kind` is `person` | a message in place of an answer |
| a `user.interrupt` | dismissed |

At most one question is open at a time. Read its questions from the
`agent.tool_use` event's `payload.input`, never from the streamed
deltas of the response: the input in the log is the one the server
checked.

```json
{"type": "agent.tool_use", "payload": {"tool_use_id": "toolu_01", "name": "question", "verdict": "allow",
 "input": {"questions": [
  {"header": "Storage",
   "question": "Which database should the new service keep its records in?",
   "options": [
     {"label": "Postgres", "recommended": true,
      "description": "The cluster the other services use. One more schema, no new operations work."},
     {"label": "SQLite",
      "description": "A file beside the binary. Nothing to operate, and one writer at a time.",
      "preview": ["data/", "  service.db", "  service.db-wal"]}]},
  {"header": "Regions", "multiple": true,
   "question": "Which regions does the first release serve?",
   "options": [
     {"label": "Europe", "description": "Where the current customers are."},
     {"label": "North America", "description": "Two prospects asked for it."}]}]}}}
```

## Showing it

| Field | Show it as |
|---|---|
| `header` | the question's short label, such as a tab's title |
| `question` | the full question; it is written to be answered without the conversation |
| `options` | in the order given, each with its `label` and its `description` |
| `recommended` | a mark of your own wording on the option; the mark is not part of the label |
| `multiple` | when true, the person may choose several options; otherwise one |
| `preview` | only on a question that does not take several options: each item is one line of plain text, shown in a monospace font with its spaces kept. You may collapse it; the question is answerable without it |

Always let the person answer in their own words, beside the options or
in place of them. Treat every field as text a model wrote: render no
markup, and follow no link on its own.

## A question written as text

Some models, small open models among them, sometimes write a call into
their answer as markup, such as `<question><header>...</header>...`,
instead of calling the tool. Topos does not keep such an answer: the
step's live output is reset, the response is recorded as a
`model.request` with `outcome` `tool_as_text` and no `agent.message`,
and the model is asked once more, with a reminder to call the tool. A
client sees the stream reset and then either the question as above or
an answer.

On a session whose model was picked for a routed name, a model that
writes the call as text again is passed to the authorizer, and the turn
may move to another model once: `session.model_changed` by the service
with `reason` `tool_as_text`, which a client shows as "The model could
not use its tools, so another one answered." Otherwise the second answer
is kept as written. Markup inside a code block or a code span, and prose
that names a tool, are answers like any other.

## Sending the answer

Confirm with the person, then send one `user.answer` with one entry per
question, in the questions' order:

```http
POST /v1/sessions/ses_01.../events
{"type": "user.answer", "payload": {"tool_use_id": "toolu_01",
  "answers": [{"selected": ["SQLite"], "text": "we have nobody to run a second schema"},
              {}]}}
```

| Entry | Means |
|---|---|
| `{"selected": ["SQLite"]}` | the person chose that option |
| `{"text": "..."}` | an answer in the person's own words |
| `{"selected": [...], "text": "..."}` | a choice and the person's note on it |
| `{}` | the question is left to the agent |

An answer whose entries are all empty leaves every question to the
agent: that is how a person declines without stopping the work.

The reply is the appended event, and the session's stream carries it.
An answer is final once it is appended, since a runner picks it up at
once; a correction is a new message.

| Reply | Means | Do |
|---|---|---|
| 200 | the answer is in the log | show the question as answered |
| 400 `invalid_request` | the answer does not fit the question: another number of entries, a label that is no option of its question or one named twice, several labels on a question that takes one, or a text past its bound | fix the answer |
| 409 `conflict` | no question of that id is open: another answer, a message or an interrupt closed it first; `details.detail` says which | read the log again |
| 403 `forbidden` | the installation's authorizer does not let this caller send to the session | the question stays open for someone who may |

Of two answers sent at once, one is appended and the other is
`conflict`.

## A message or an interrupt instead

A `user.message` the person sends after the question closes it: the
agent reads that no option was chosen and then the message, which may
be the answer. A `user.interrupt` dismisses it: the session goes idle
`interrupted`, and nothing runs until the person's next message.

## Showing a closed question

Show an answered question from its `user.answer`. When something else
closed it, read the `tool.result` of the call: its `meta` says why,
without reading the result's text.

```json
{"type": "tool.result", "payload": {"tool_use_id": "toolu_01", "outcome": "ok",
 "meta": {"closed_by": "answer", "event_id": "evt_01..."}}}
```

| `meta.closed_by` | `outcome` | Means |
|---|---|---|
| `answer` | `ok`, or `unanswered` when every entry was empty | the person answered; `event_id` is the `user.answer` |
| `message` | `unanswered` | the person wrote a message instead; `event_id` is the `user.message` |
| `interrupt` | `canceled` | the person dismissed the question; `event_id` is the `user.interrupt` |
| `unattended` | `unanswered` | nobody attends the session, so the agent decided |

`unanswered` is not an error: the result's `is_error` is false.

## Redaction

`user.answer` is redactable. Redacting it also redacts the `tool.result`
rendered from it, since that result holds the same words. The
`agent.tool_use` of a question whose call has no `tool.result` yet
cannot be redacted (`conflict`): dismiss the question with
`user.interrupt` first.

## From the command line

`topos run --attended` creates a session a person attends. A question
stops the run with exit code 3 and prints the open call, its questions
and their options; answer it in your own words with
`topos run --session <id> <answer>`, which sends a message in place of
an answer. Without `--attended`, a question does not stop the run.

## For the installation's authorizer

An answer is asked as `session.send` with `event_type` `user.answer`,
beside `user.message`, `user.tool_confirmation` and `user.tool_result`.
An authorizer that lists the event types it allows must accept
`user.answer` before a client sets `attended`.
