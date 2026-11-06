# Security

For an operator deciding how to run Topos and a builder deciding what to
send it: what Topos guarantees, what it sees and keeps, what it does not
defend, and what an operator keeps safe. Reporting a vulnerability, and
the test that holds each guarantee, are in
[SECURITY.md](../SECURITY.md).

## What Topos guarantees

[SECURITY.md](../SECURITY.md#what-is-protected) lists each commitment
with its test. In short:

- Every request under `/v1`, the OpenAPI document aside, carries a
  token from an issuer the installation lists, and every action is the
  authorizer's decision, or the owner policy's when there is no
  authorizer. When the authorizer cannot be reached, the action is
  refused and nothing is written.
- A session has one writer at a time, and a runner that lost its lease
  can no longer append.
- The model connection's credential never enters a session's log or
  blobs, and an agent manifest that holds a credential is refused.
- A subagent acts with its session's credentials and permissions, never
  those its own agent declares.
- On a host machine, the file tools stay inside the session's roots and
  refuse files such as `.env`.

## What Topos sees

| What | Where it goes |
|---|---|
| Messages, every tool call and its result, every model request, and the session's status | the session's log, in Postgres with `TOPOS_DB_URL` or in the data directory without it |
| Raw model responses and the requests captured with them | the blob store of `TOPOS_BLOB_URL`, or the database or the data directory without one |
| Files a person attaches, and the images and files an answer shows or links | the session's own storage, or a folder of the files service with `TOPOS_FILES_AUDIENCE` ([Reading files](reading-files.md)) |
| The session's working directory | a checkpoint at every turn; for a hosted session whose first repository is private, at the git host under `refs/topos/checkpoints/<session>/latest`; with a files service, its latest state under `work/` in the session's folder |
| Everything in a session's context | the model provider, through Lux or directly |
| Each action a caller takes, and each create, fork and message | the authorizer, as the question in [docs/authorizer.md](authorizer.md); a message's words only when `TOPOS_AUTHORIZER_MESSAGE_TEXT` is `true` and the authorizer asked for them |
| Each tool call a decision service is asked about | the decision service of `TOPOS_DECISIONS_URL` ([How a call was decided](how-a-call-was-decided.md)) |
| Each `web_search` query | the search service of `TOPOS_SEARCH_URL` ([Web search](web-search.md)) |

[docs/configuration.md](configuration.md) has each variable named here.

## How long it keeps things

- **A session** is kept until someone deletes it, or until its retention
  passes after it ends, where the authorizer's allow gave it one. The
  retention counts from the session's last event. An idle session past
  its `expires_at` is ended as expired, and kept like any ended session.
- **A delete** (`DELETE /v1/sessions/{id}`) removes the session's log
  and blobs for good. A fork keeps its own copy of the log. The session's
  sandbox, its Secrets at Cella and a checkpoint kept at the git host are
  the installation's to remove.
- **A redaction** (`POST /v1/sessions/{id}/events/{event_id}/redact`)
  replaces one event's content with a tombstone. A kept file that no
  other event names is deleted, from the files service's folder too.
- **A checkpoint at the git host** stays until the repository or the ref
  is deleted.
- **A file a message brings from a person's own files** is held in the
  data directory's `spool/` only while the message is checked.

## What it does not defend

[SECURITY.md](../SECURITY.md#what-is-not-defended) is the full list. The
limits an operator plans around:

- A model steered by what it reads acts with all of its session's
  permissions; the approval mode and the machine bound it.
- Code in a Cella sandbox can use the session's credentials toward
  their own hosts while the sandbox exists, through Cella's egress.
- On a host machine without the host sandbox, a command runs as the
  person's account; the roots bind the file tools only.
- Whatever a session reads reaches the model's provider.

## What an operator keeps safe

| Secret | What its holder can do |
|---|---|
| `TOPOS_LOCAL_ISSUER_KEY` | sign a token for any subject, the admins of `TOPOS_ADMIN_SUBJECTS` included |
| `TOPOS_RUNNER_TOKEN` | claim sessions from the internal listener and drive them as their runner |
| `TOPOS_DB_URL`, `TOPOS_DB_POOL_URL`, and the data directory | read and change every agent, session and log |
| `TOPOS_BLOB_ACCESS_KEY`, `TOPOS_BLOB_SECRET_KEY` | read every raw model response and captured request |
| `TOPOS_AUTHORIZER_TOKEN`, `TOPOS_SESSION_KEYS_TOKEN` | call the authorizer's routes as `toposd` |
| `TOPOS_MODELS_KEY`, `TOPOS_SEARCH_KEY`, `TOPOS_DECISIONS_TOKEN` | spend on the model connection, the search service and the decision service as the installation |
| the files of `TOPOS_CELLA_TOKEN_FILE`, `TOPOS_ORIGO_TOKEN_FILE` and `TOPOS_IDENTITY_SECRET_FILE` | create sandboxes at Cella, push to the git host, and get tokens from the identity provider as the installation |

Beside the secrets:

- Keep the internal listener, `TOPOS_INTERNAL_ADDR`, off the public
  network: it serves the routes runners claim sessions from.
- `TOPOS_CELLA_INSECURE` and `TOPOS_OIDC_INSECURE_ISSUERS` send a bearer
  or accept keys over plain `http`; set them only for a network you
  trust.
- `TOPOS_AUTHORIZER_URL` is accepted over plain `http` on any host,
  with no setting to consent to it, and its bearer and every question
  then travel in the clear. Give it an `https` URL unless the network
  between `toposd` and the authorizer is one you trust.
- `TOPOS_ADMIN_SUBJECTS` names who the owner policy lets act on every
  object; keep it to the people who run the installation.
- [docs/install.md](install.md) has the image's signature, the probes and
  what a production installation needs.
