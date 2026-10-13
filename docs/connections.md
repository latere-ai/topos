# Connections: a person's other services, reached without their credentials

A person connects the services they work with, such as a code host, at
an installation that keeps those connections in a connection service of
its own. The service holds each credential, serves each connection at
addresses of its own, and adds the credential on the way out. A session
reaches those addresses with its own short session token, so neither
the runner nor the sandbox ever holds a credential of the other service.
This page is the contract an installation's authorizer and its
connection service follow. The field names are the wire's.

## Naming a session's connections

The authorizer names the connections in the limits of its allow of
`session.create`, `session.fork` and `session.send`:

```json
{
  "connections": [
    {"name": "github", "kind": "http", "title": "GitHub", "audience": "connections",
     "address": "https://connections.example.com/v1/connections/c_1/http",
     "git_address": "https://connections.example.com/v1/connections/c_1/git",
     "git_url": "https://github.com"},
    {"name": "tracker", "kind": "mcp", "title": "Issue tracker", "audience": "connections",
     "address": "https://connections.example.com/v1/connections/c_2/mcp",
     "read_only": ["list_*", "get_*"]}
  ]
}
```

| Member | Meaning |
|---|---|
| `name` | a lowercase DNS label that starts with a letter, at most 32 characters, unique in the list |
| `kind` | `http`: an API and, optionally, a git host reached from the session's sandbox; `mcp`: a remote MCP server the runner reaches |
| `title` | what the model and a person call the connection, one line of at most 100 characters |
| `audience` | the audience of the session token the connection service accepts |
| `address` | the connection's forwarding address for `http`, its MCP address for `mcp`; an `https` URL with no query or fragment |
| `git_address`, `git_url` | `http` alone, both or neither: the connection's git address, and the git host the person's repositories live at |
| `read_only` | `mcp` alone: globs over the server's tool names; a tool one matches reads, and every other reaches outside |

At most 32 connections, connected and available together. A connection
that breaks a rule refuses the request with `authorizer_unavailable`,
naming the member in the developer detail.

### A service the person has not connected

Name a service the installation offers and the person has not connected
with `state` `available`, and with its name, kind and title alone:

```json
{"name": "github", "kind": "http", "title": "GitHub", "state": "available"}
```

An available connection names no `audience`, `address`, `git_address`,
`git_url` or `read_only`, and one that does refuses the request. A
connection without `state` is connected, and needs its `audience` and
`address`. The session reaches nothing of an available connection, so
it needs no connection service on the installation, and a session's
`connections` answer it with its `state`. Once the person connects the
service, answer the next send with the connected entry in its place.

The create's and the fork's list is the session's. Each send's list
replaces it, so a person who connects a service in the middle of a
conversation has it from their next message, and one who disconnects
it loses it there. A send whose list differs appends
`session.connections_changed` with the new list before the message. A
send whose allow names no `connections` leaves the session with none,
so answer every send with the person's current list.

## What the session does with them

The installation sets `TOPOS_CONNECTIONS_URL`, the connection service's
root, and `TOPOS_CONNECTIONS_AUDIENCE`, `connections` unless set
([configuration](configuration.md)). Every sandbox then holds
`CONNECTIONS_TOKEN`, a placeholder that Cella's egress gateway swaps for
the session's token on requests to the service's host, and nothing
else of it. The placeholder is in every sandbox from its create,
because a sandbox takes a placeholder at its create alone.

For each connection:

- **git.** `git clone https://github.com/acme/app` is rewritten to the
  connection's git address and sends the placeholder as a bearer, so
  clones, fetches and pushes go through the connection service. A
  repository a session names on that host is delivered the same way.
- **The API.** Each command carries `<NAME>_API_URL`, here
  `GITHUB_API_URL`, naming the forwarding address. The agent calls
  `$GITHUB_API_URL/user` with `Authorization: Bearer $CONNECTIONS_TOKEN`.
- **The model is told.** A block of the system prompt names each
  connection, how to clone, push and call its API, and that the agent
  pushes its own branch, `agents/<agent>/<session>`.

An `http` connection must name the audience of
`TOPOS_CONNECTIONS_AUDIENCE` and addresses on the host of
`TOPOS_CONNECTIONS_URL`. A server without the variable refuses an allow
with an `http` connection. A session on a host machine uses the
person's own git and environment, and is told nothing of its `http`
connections.

Serve the connection service on a host of its own. A sandbox cannot
hold two placeholders for one host, so the service's host may be
neither the git host's nor the model gateway's, whether the address
`TOPOS_MODELS_URL` names or the one the gateway publishes its doors
under: the server refuses to start on such a configuration. A
sandbox that still cannot hold the service's placeholder, because Cella
refuses it beside another, opens without it, the server logs an error
naming both hosts, and the session works on without its `http`
connections: the agent is told of none of them and asks the person to
connect none. Its `mcp` connections are unaffected.

## Remote MCP servers

For a connection of kind `mcp`, the runner reaches `address` as a Model
Context Protocol server over streamable HTTP, with the session's token
for `audience` and the workload `session` as its bearer, and lists its
tools when the runner starts driving the session, as each message does.
It speaks revision 2026-07-28, and falls back to
the `initialize` handshake of revision 2025-11-25 for a server that
answers `400` with no error of the current revision. Each tool is
offered to the model as `mcp__<name>__<tool>`, and is checked, scored
and asked about as any tool is: a tool a `read_only` glob matches reads,
and every other is an external call, which a session in `confirm`
asks the person about. A server's tools never start the session's
sandbox.

## A connection that is not connected

The connection service answers `409` with the code
`connection_not_connected` for a connection the person has to connect
again. The session goes on in each case:

- **`http`.** The sandbox's tools read that answer as the service gave
  it, and the model is told to relay it to the person, naming the
  service.
- **`mcp`.** The call fails with a result that names the connection and
  the reason, and `connection.requested` follows it:

  ```json
  {"type": "connection.requested", "payload": {"name": "tracker", "title": "Issue tracker", "kind": "mcp", "reason": "revoked", "tool_use_id": "toolu_01"}}
  ```

  A client shows it as "connect this service" and starts the connection
  service's reconnect. A listing that meets the answer at the start of a
  drive offers none of the server's tools and appends the same event
  without `tool_use_id`, and the model is told the service is not
  connected. `scopes`, when present, are the ones the other service
  asked for.

## Asking the person to connect a service

While a session has an available connection, the system prompt lists
each one, a line apiece, apart from the connected ones, and says the
agent may ask the person to connect one when the task needs it. The
agent of the session's own thread is offered the tool
`request_connection`, with `name`, the service's name, and `reason`, a
sentence of at most 200 characters that says what it needs the service
for. A call appends `connection.requested` after its result:

```json
{"type": "connection.requested", "payload": {"name": "github", "title": "GitHub", "kind": "http", "reason": "to clone acme/app and open a pull request", "tool_use_id": "toolu_02"}}
```

A client shows it as "connect this service" and starts the connection
service's connect. The call reaches nothing and opens no machine, and
its result tells the agent to end its turn with a sentence that says
what it will do once the service is connected. The person's next
message reaches the session with the connected entry, so the agent goes
on from there. A call that names a service that is not available
answers an error naming the ones that are, and appends nothing.

A session on a host machine is not told of an available `http`
connection, since connecting it would give the session nothing, and is
not offered the tool for it. A thread the agent started works from its
task and is told of no available service; the agent that holds the
conversation asks.

## What the installation lets its identity provider mint

The runner's host client asks for the session's token for
`TOPOS_CONNECTIONS_AUDIENCE` with the workload `sandbox`, and for each
`mcp` connection's `audience` with the workload `session`. The identity
provider must mint those audiences for it. Without the first no
placeholder is mounted and the sandbox reaches no connection; without
the second the server's tools are not listed, and the session records
`mcp_unavailable`.
