---
title: Cella Provider on the Cella Core
status: drafted
track: runtime
depends_on:
  - specs/010-sandbox-cella.md
  - specs/011-sandbox-credentials.md
affects:
  - sandbox/cella/
  - sandbox/provider.go
  - examples/sandbox/main.go
  - retired_route_test.go
  - docs/sandboxes.md
  - README.md
  - CHANGELOG.md
  - go.mod
effort: medium
created: 2026-09-26
updated: 2026-09-26
author: changkun
dispatched_task_id: null
---

# Cella Provider on the Cella Core

## Problem

`sandbox/cella` speaks the API of the hosted sandbox service at
`cella.latere.ai`: a `cella.latere.ai/v1` manifest with `tier` and `policy`,
detached commands polled by cursor, and tar-only file transfer through
multipart forms. That service is being retired. The hosted replacement is the
open source Cella control plane (`cellad`), served under the platform origin at
`https://api.latere.ai/v1/environments`, with a different grammar: a
`cella.latere.ai/v1beta1` manifest, a synchronous exec route, one-file routes,
an egress boundary the plane enforces, and images drawn from a catalog. Since
v0.6.0 a create also answers `201` with the sandbox `Pending` before its
workload runs, so a caller that acts on a new sandbox has to wait for it.

Spec 010 chose a hand-rolled HTTP client because the only Go module Cella had
was its whole control plane. The core now exports `latere.ai/x/cella/client`,
whose build list is the standard library, the manifest types, and the error
envelope of `latere.ai/x/pkg/httpjson`. That removes the reason for a second
client of the same API.

## Contract

The `sandbox.Provider` interface, `cella.Options`' existing fields, the three
`TokenSource` implementations, and the error contract are unchanged for
callers. The provider is built on the exported client:

| `sandbox.Provider` | Core route (under the base URL) | Notes |
|---|---|---|
| `Create` | `POST /sandboxes?wait=1&timeout=...` | a `v1beta1` manifest; the answer is held until the sandbox runs or fails |
| `Destroy` | `DELETE /sandboxes/{id}` | `not_found` is success |
| `HealthCheck` | `GET /sandboxes/{id}` | nil only when the phase is `Running` |
| `Exec` | `POST /sandboxes/{id}/exec?wait=1` | one JSON answer: exit code, stdout, stderr |
| `StreamExec` | the same route | one chunk, delivered when the command ends |
| `ReadFile` | `GET /sandboxes/{id}/files/content?path=` | `not_found` is `sandbox.ErrNotFound` |
| `WriteFile` | `PUT /sandboxes/{id}/files?path=` | the core creates missing parents |
| `ListFiles` | `GET /sandboxes/{id}/files/list?path=` | immediate entries, sorted by name |

`Options.BaseURL` is the control plane's address including its base path,
`https://api.latere.ai/v1/environments` at the hosted deployment. The exported
client maps each `/v1/...` route onto it.

### Create

The manifest the provider sends:

| `CreateOptions` | Manifest | Default |
|---|---|---|
| `Name` | `metadata.name` | the server names it |
| `Labels` | `metadata.labels`, with `kind=agent` added unless the caller set `kind` | `kind=agent` |
| `Image` | `spec.image` | `base`, the catalog's general-purpose image |
| `Env` | `spec.env` | none |
| `Tier` | `spec.lifecycle` | `ephemeral` |
| (none) | `spec.network.egress` | `allowlist` with `api.latere.ai` |

Every sandbox stops after 15 minutes without activity (`autoStop: 15m`), the
cost backstop spec 010 set. The tier chooses what happens to the workspace:

| `Tier` | `spec.lifecycle` | Read back as |
|---|---|---|
| `""`, `ephemeral` | `autoStop: 15m`, `ttl: 24h` | a sandbox with a time to live is `ephemeral` |
| `persistent` | `autoStop: 15m` | a sandbox with none, or `never`, is `persistent` |
| anything else | refused before any request | |

The returned `Tier` is read from the object the core answered, not remembered.
An installation's admission can supply a time to live the caller did not ask
for; the hosted plane does, per plan, and the sandbox is then reported
`ephemeral` because that is what it is.

The create is held with `client.Wait`: bounded by the context's remaining
time less a margin, so the server answers before the caller gives up and the
answer names the sandbox, and by ten minutes when the context has no deadline.
A sandbox the core leaves `Failed` or `Lost` is deleted, best effort, and the
create returns an error naming its id and reason. A sandbox still `Pending`
when the hold ends is returned as `creating`, which the interface already
allows; `Run`'s readiness wait covers it.

### The egress boundary

A hosted sandbox reaches only the hosts its boundary admits, and hosted
admission narrows an organization's boundary to `allowlist` whatever the
manifest asks. A Topos sandbox runs the agent loop's tools, and the brain of a
run placed in a sandbox calls models through Lux, which the hosted deployment
serves at `https://api.latere.ai/v1/models`. The provider therefore sets the
boundary explicitly: `allowlist` with `api.latere.ai`, which the core accepts
on every plan, rather than `open`, which admission would narrow to an empty
list for an organization and so cut the model gateway.

`Options.AllowedHosts` replaces that list: nil keeps the default, and a non-nil
list is exactly the hosts the sandbox may reach, each an exact name or one
leading `*.` wildcard. A core with no egress gateway connected refuses any
boundary other than `open` with `503 egress_gateway_unavailable`, which
surfaces as `*sandbox.APIError` with that code.

### Exec

The synchronous route runs the command with its standard input at end of
file, as the local provider does, and answers once it ends. `ExecResult.Stdout`
carries the command's standard output followed by its standard error, and
`Stderr` stays empty, so the `bash` tool, which reads `Stdout` alone, still
sees both. The core keeps the first mebibyte of each channel. The phase is
`exited` with the command's exit code, and `124` when the core's own timeout
ended it.

The timeout sent is the context's remaining time, at most the core's one hour,
and one hour when the context has no deadline, so the server ends a command the
caller has stopped waiting for rather than leaving it to its ten minute
default. A context that ends while the command runs is the `killed` phase with
no error, as in the local provider; the decision is read from the context, not
from the error type, because the client passes a cancellation through and
wraps a deadline in `*client.Unreachable`.

`StreamExec` runs the same route and hands back a stream holding the whole
output as one chunk. The core's live form is a WebSocket whose standard input
stays open until the session closes, so a command that reads its input would
wait there until its timeout; one contract for both methods is worth more than
incremental delivery, which no caller of this provider reads.

### Paths

A relative path, in a file operation or in `ExecOptions.Cwd`, is resolved
under the workspace, `/workspace`; an absolute path is sent as given, and the
core refuses one outside the workspace.

### What the core does not carry

| Field | Why it has no equivalent | Behavior |
|---|---|---|
| `CreateOptions.Policy` | the core has no named policy profiles; the boundary is the manifest's own fields | a non-empty value is refused before any request |
| `CreateOptions.SecretMounts` | a core secret is a placeholder the egress gateway substitutes toward the secret's hosts, never a file holding the value | a non-empty list is refused; nil and empty mount nothing |
| `ExecOptions.SecretEnv` | the exec route resolves no secret for one command | a non-empty map is refused |

Refusing is deliberate: each field asks for a restriction or a credential, and
a provider that dropped it would run the caller's work without what the caller
asked for.

### The HTTP client

The exported client's own transport allows ten seconds to the first response
byte. A held create and a synchronous exec answer only when the sandbox has
started or the command has ended, so that deadline would cut both. The provider
always passes an `http.Client` to the exported client, `Options.HTTPClient` or
an instrumented one with no timeout, and deadlines come from the context.

## Acceptance criteria

| # | Criterion | How it is checked |
|---|---|---|
| 1 | `Create` sends the manifest above, holds the create with `?wait=1`, and returns `running` | a fake core under `/v1/environments` that answers `201 Pending` without `wait` and `Running` with it |
| 2 | A `Failed` create returns an error naming the id and reason and deletes the sandbox | fake core |
| 3 | `Exec` sends the command, workdir, environment and timeout, and returns stdout then stderr with the exit code; a cancelled context is `killed` | fake core |
| 4 | `ReadFile`, `WriteFile`, `ListFiles`, `Destroy` and `HealthCheck` reach the one-file and object routes and map `404` and `409` | fake core |
| 5 | `Policy`, `SecretMounts` and `SecretEnv` are refused before any request | fake core records no request |
| 6 | Every request goes through the configured `http.Client` | a counting transport |
| 7 | `503 egress_gateway_unavailable` surfaces as `*sandbox.APIError` with that code | fake core |
| 8 | The example defaults to `https://api.latere.ai/v1/environments` | read in review |
