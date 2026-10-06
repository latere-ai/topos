# A session's network: where its machine may reach, and asking the person

A hosted session's machine is a Cella sandbox, and it reaches the
internet only through Cella's egress gateway. The session's network
says which hosts the sandbox may reach. The installation's authorizer
decides it, Topos enforces it, and a person attending the session can
widen it one host at a time. This page is the contract an authorizer
and a client follow. Everything here is on the wire of `/v1`; the field
names are the API's.

## Where the network comes from

An authorizer's allow of `session.create` may carry a network in its
limits:

```json
{"network": {"mode": "allowlist", "hosts": ["example.com", "*.example.org"], "ask": true}}
```

| Member | Meaning |
|---|---|
| `mode` | `open` reaches every host but the ones Cella denies; `allowlist` reaches the hosts listed; `none` reaches nothing |
| `hosts` | with `allowlist` only: exact names or one leading `*.`, never an address, a port or a single label; at most 512 |
| `ask` | with `allowlist` only: a first contact with a host outside the network asks the person attending the session |

Without one, the session runs on its agent's own mode,
`spec.machine.egressMode` (`allowlist` when the manifest names none),
with `ask` false. Under `allowlist` the sandbox always also reaches the
agent's `spec.machine.egress` hosts, the hosts of the session's
credentials, such as the model gateway, and the git hosts of its
repositories, so a narrow list never cuts a session off from what it
needs to run. To give a session no egress at all, answer `none`.

The session records the network it was created with:

```json
"network": {"mode": "allowlist", "hosts": ["example.com"], "ask": true, "source": "authorizer"}
```

`source` is `authorizer`, or `agent` when the allow named none. A
network that is not one of these shapes is refused as
`authorizer_unavailable`, since Topos cannot apply a boundary it cannot
read.

An allow of `session.send` may carry a network too. When it differs
from the session's, the server appends `session.network_changed` with
`source: authorizer` right before the sent event, and the session's
machine runs the new network before the next call of the turn runs on
it. The hosts a person allowed in the session stay. A fork is asked as
a create is: it takes its own allow's network, and the hosts a person
allowed in the session it forks are not carried.

## Fetching a page

A `web_fetch` is decided by the network before the approval mode, and
each `agent.tool_use` records the reason a client shows:

| The fetch's host | Verdict | `reason` |
|---|---|---|
| inside the network | `allow` in `confirm` mode; scored as a known host in `progressive` | `inside the session's network` |
| outside it, `ask` true, the session attended | `ask` | `outside the session's network` |
| outside it otherwise, or any host under `none` | `block` | the text the model reads, saying the host is outside the session's network |

`always_confirm` still asks for a fetch inside the network. A session
asks only when its creator set `attended`, since nothing times an ask
out.

When the person allows an asked fetch, the runner widens the running
sandbox to the host, appends `session.network_changed`, and then runs
the call:

```json
{"type": "session.network_changed", "payload": {"added": ["docs.example.net"], "source": "person", "tool_use_id": "toolu_07"}}
```

The host stays reachable for the rest of the session. If Cella refuses
the widening, nothing is recorded, the call is closed with an error
result, and the session's log holds a `session.error` with the code
`network_unavailable`.

## A command's refused connection

A command's connections are not in its text, so the gateway is the
boundary: a command that reaches a host outside the network fails, and
after the call's result the runner appends one `approval.requested` per
refused host, at most 3 per call:

```json
{"type": "approval.requested", "payload": {
  "approval_id": "apr_01J9...", "tool_use_id": "toolu_07", "source": "egress",
  "destination": {"host": "registry.example.com", "port": 443},
  "reason": "connection outside the session's network", "verdict": "ask"}}
```

The last request of a call carries `more`, the count of further hosts
the same call was refused that have no request of their own. A host the
person already denied in the session, or one already waiting for an
answer, is not asked again.

With `verdict: ask` the session goes idle with `stop_reason`
`tool_confirmation`, after the call's result. With `verdict: block`,
which a session whose network does not ask, or that nobody attends,
gets, nothing waits: the record lets a client offer the host for next
time. The model reads each request after the call's result.

## Answering a request

Send a `user.tool_confirmation` that names the request's `approval_id`
in place of a `tool_use_id`:

```http
POST /v1/sessions/ses_01.../events
{"type": "user.tool_confirmation", "payload": {"approval_id": "apr_01J9...", "decision": "allow"}}
```

A confirmation names exactly one of `tool_use_id` and `approval_id`.
`remember` is refused with an `approval_id`, since an approval is of a
connection and not of a call. One sent for a request that nothing waits
on, or that something else already answered, is `conflict`.

On `allow` the runner widens the session's network to the host and
appends `session.network_changed` with `source: person` and the
`approval_id`, then `approval.decided`:

```json
{"type": "approval.decided", "payload": {"approval_id": "apr_01J9...", "decision": "allow", "by": {"subject": "...", "kind": "person"}}}
```

The model then reads that the host is reachable and may run the command
again. On `deny`, or when the person sends a message instead of an
answer, `approval.decided` carries `deny` and the host is not asked
again in the session. A request is answered once its runner appends
`approval.decided`; a client that shows open requests reads the log for
`approval.requested` events with `verdict: ask` and no `approval.decided`.

## The initiator's instructions

An allow of `session.create` may also carry `instructions`: standing
instructions from the person who starts the session, at most 8 KiB of
UTF-8, such as what to call them or how they like an answer. The
session records them as `instructions`, and the agent reads them after
its own instructions, marked as the person's, for the whole session. A
send never changes them; a fork takes the ones its own allow carries.
They are text the agent reads, never a permission: the network, the
approval mode and every ask stay decided outside the model.
