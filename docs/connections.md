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
     "git_url": "https://github.com"}
  ]
}
```

| Member | Meaning |
|---|---|
| `name` | a lowercase DNS label that starts with a letter, at most 32 characters, unique in the list |
| `kind` | `http`: an API and, optionally, a git host reached from the session's sandbox |
| `title` | what the model and a person call the connection, one line of at most 100 characters |
| `audience` | the audience of the session token the connection service accepts |
| `address` | the connection's forwarding address, an `https` URL with no query or fragment |
| `git_address`, `git_url` | both or neither: the connection's git address, and the git host the person's repositories live at |

At most 32 connections. A connection that breaks a rule refuses the
request with `authorizer_unavailable`, naming the member in the
developer detail.

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

## A connection that is not connected

The connection service answers `409` with the code
`connection_not_connected` for a connection the person has to connect
again. The sandbox's tools read that answer as the service gave it, and
the model is told to relay it to the person, naming the service. The
session goes on.

## What the installation lets its identity provider mint

The runner's host client asks for the session's token for
`TOPOS_CONNECTIONS_AUDIENCE` with the workload `sandbox`. The identity
provider must mint that audience for it. Without it no placeholder is
mounted, and the sandbox reaches no connection.
