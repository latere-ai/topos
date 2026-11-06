# Topos documentation

One page per question, grouped by who asks it. The design behind every
page is in [specs](../specs/README.md).

## Trying it

| Page | What it answers |
|---|---|
| [Running it locally](local.md) | how to run `toposd` on one machine with everything it needs, and see a session answer |

## Running an installation

| Page | What it answers |
|---|---|
| [Installing Topos](install.md) | the image and its signature, the roles, the database, keys, probes, the manifests, upgrading, and what production needs |
| [Configuration](configuration.md) | every `TOPOS_*` variable, its default, and what refuses a start |
| [Security](security.md) | what Topos guarantees, what it sees and keeps, what it does not defend, and what an operator keeps safe |
| [Writing an authorizer](authorizer.md) | the question `toposd` asks for every action, the vocabulary, the answer and its limits, failing closed, and the owner policy without one |
| [Web search](web-search.md) | turning on `web_search`, and the contract a search service answers |
| [How a call was decided](how-a-call-was-decided.md) | the layer and the rule that decided each tool call, the organization it was decided under, and how repeated refusals hand a session back to its person |
| [Unattended turns](unattended-turns.md) | how a turn nobody watches is decided, and the `attended` the log records |

## Building a client

| Page | What it answers |
|---|---|
| [Building on the Topos API](api.md) | the concepts, the flows a client follows, authentication, the errors to branch on, the limits, and the reference |
| [Questions](questions.md) | how a client shows an agent's question and sends the person's answer |
| [Messages sent while the agent works](messages-while-working.md) | a message read at the agent's next step, sent to be read now, or withdrawn before the agent reads it |
| [Editing a message](editing-a-message.md) | sending an edited message from a point in a conversation, and showing its versions |
| [Titles](titles.md) | a session's title that follows its conversation, turned on by an agent's `spec.title` |
| [Searching sessions](searching-sessions.md) | finding a caller's sessions by what was said in them, and showing the match |
| [Reading a session's files](reading-files.md) | the files a person sent, an answer kept and the working directory holds, with their media types and sizes |
| [Reading back](reading-back.md) | the `history` tool, which reads back what a long session's context cleared or summarized |
| [The context report](context-report.md) | how full a conversation's context is, and what fills it |
| [Cost so far and progress](cost-and-progress.md) | what a session has cost and how far its to-do list is, kept current from its event stream |
| [A session's network](network.md) | where a session's machine may reach, how a person widens it, and the initiator's instructions |
| [Connections](connections.md) | the services a person connected, reached from a session without their credentials |
| [Memory folders](memory-folders.md) | folders a session keeps across sessions, reached by the file tools at `/memory/<name>/` |
| [Publishing](publishing.md) | putting a session's folder online as a web app, and releasing it |
| [A day's activity](activity.md) | on which days people used the agents and from which application, without reading what they said |

## Triggers, wakes and offers

| Page | What it answers |
|---|---|
| [Offers](offers.md) | how a client shows an agent's offer of standing work, or of a change to it, and sends the person's answer |
| [Wakes](wakes.md) | the wake an agent set to come back to a conversation later, the turn it starts, and how to cancel it |
| [Holding every trigger](holding-triggers.md) | the one switch that pauses and resumes every trigger a person owns |
| [Quiet runs and notices](quiet-runs.md) | telling a trigger's run that brought the person something from a quiet one, and showing the agent's notice |
| [A trigger's runs](trigger-runs.md) | every trigger's runs with what each cost, the cost of a typical run, and when each fires next |
| [Allowances](allowances.md) | a weekly allowance on a trigger, the week its runs have spent, and the pace each run is told |
| [Trigger models](trigger-models.md) | the model and the reasoning level a trigger's runs start on, in place of the agent's |
| [Trigger times](trigger-times.md) | a trigger that fires at a list of times, each once |
| [A trigger's reach](trigger-reach.md) | a kind the authorizer maps, and a firing that runs once per due scope or not at all when nothing is new |

## History

[docs/history](history) holds the specs of the v0.7.0 runtime, an
embeddable Go library this core replaced, as a record.
