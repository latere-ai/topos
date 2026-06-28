---
title: Cella Sandbox Provider
status: complete
depends_on:
  - specs/runtime/embeddable-sdk.md
  - specs/runtime/delegation.md
affects:
  - sandbox/cella/provider.go
  - sandbox/cella/client.go
  - sandbox/cella/token.go
  - topos.go
  - README.md
effort: medium
created: 2026-06-28
updated: 2026-06-28
author: changkun
dispatched_task_id: null
---

# Cella Sandbox Provider

## Goal

Let a host run Topos agents on real, isolated compute by backing the
`sandbox.Provider` interface with [Latere Cella](https://cella.latere.ai), the
hosted Kubernetes sandbox platform, instead of the local temp-directory
fallback. A host that wants ephemeral cloud sandboxes constructs a Cella
provider and injects it into the runner; everything else (delegation,
per-child sandboxes, lift/drop) keeps working unchanged because it already
depends only on the interface.

This is the first non-local `sandbox.Provider` implementation. Its shape is the
template every future backend follows.

## Constraints inherited from the existing design

The runtime was built anticipating this backend, so most decisions are already
fixed in code and must be honored, not revisited:

- **Single interface boundary.** All upstream code (`topos`, `harness`,
  `runtime/loop`, `harness/tools`) depends only on `sandbox.Provider` and the
  shared types in `sandbox/provider.go`. The new backend lives in a leaf package
  `sandbox/cella` and is the *only* package allowed to know Cella exists.
- **Boundary rule, already tested.** `sandbox/boundary_test.go` asserts that no
  file in `sandbox/` imports `sandbox/cella`. The root `topos` package must not
  import it either: the host constructs the provider and passes it in as the
  interface type.
- **Types mirror Cella on purpose.** `sandbox.State`, `ExecResult.Phase`
  (`exited`/`killed`/`lost`), and the `*APIError` / `ErrNotFound` / `ErrConflict`
  error contract were written to match Cella's API. The `Command.phase` enum in
  Cella's OpenAPI (`running`/`exited`/`killed`/`lost`) maps 1:1 to
  `ExecResult.Phase`; no translation table is needed.
- **Combined output stream.** Cella merges stdout and stderr in arrival order.
  `ExecResult.Stdout` carries the combined stream; `ExecResult.Stderr` stays nil.
  The interface comment already documents this for the Cella backend.
- **Per-request bearer via context.** `sandbox/context.go` already provides
  `WithBearer` / `BearerFromContext`, and its doc comment names the type this
  spec adds: `cella.ContextTokenSource`.

## Why a hand-rolled HTTP client, not the Cella Go module

Cella is module `latere.ai/x/sandbox`, a full control plane (sandboxd,
podman, vault, DOKS deploy) with a large dependency tree. Topos is an
*embeddable* runtime: `go.mod` has a single dependency (`google/uuid`).
Importing the Cella module would drag a Kubernetes stack into every host binary.

The contract between the two systems is the versioned HTTP surface in
`api/openapi.yaml` (the `/v1/*` paths), not Go types. So `sandbox/cella` hand-
rolls a thin `net/http` client against that surface. No code generation, no
shared module, no transitive dependencies beyond the standard library.

## Design

### Package layout

```
sandbox/cella/
  client.go    // low-level HTTP: base URL, do(), error decoding, JSON/tar helpers
  token.go     // TokenSource, StaticTokenSource, TokenFunc, ContextTokenSource
  provider.go  // Provider: implements sandbox.Provider over client + token
  *_test.go    // httptest-backed tests (no live cluster)
```

`cella.New(opts Options) *Provider` takes a base URL, an `http.Client`, and a
`TokenSource`. A compile-time `var _ sandbox.Provider = (*Provider)(nil)` pins
the contract.

### Authentication and token ownership

Cella is bearer-token authenticated. There are two issuers (the legacy auth
service and Cella's own signing key), and `POST /v1/tokens/exchange` mints a
Cella bearer from an upstream actor token. The ownership split:

- **The host owns the exchange.** At run start it bridges the inbound user JWT
  to a user-subject Cella bearer (once), then stores it on the context with
  `sandbox.WithBearer(ctx, bearer)`. This scopes the entire run (entry agent
  plus every delegated peer's create/exec/destroy) to the session user's
  identity.

The provider stores no token: `send` asks the configured `TokenSource` on every
request and sets `Authorization: Bearer <token>`. Three sources cover the
ownership models a caller might have:

- **`StaticTokenSource`**: one fixed token for the process (service account,
  dev). No refresh.
- **`TokenFunc`**: a caller-supplied function the SDK calls per request. This is
  the recommended shape when the caller *owns* the token and rotates it out of
  band: returning the current value makes a refresh flow through automatically,
  including for requests deep inside a long-running `Run`.
- **`ContextTokenSource`**: reads a per-request bearer from `BearerFromContext`
  (set by `sandbox.WithBearer`). Best for multi-tenant hosts (a different user
  per request), but the token is fixed for whatever context is passed, so it does
  not pick up a refresh mid-run.

The provider does **not** call `/v1/tokens/exchange` itself; minting and refresh
are the caller's concern, kept out of the runtime. A long run that may outlive a
token's TTL should use `TokenFunc` so rotation propagates.

### Method mapping

| `sandbox.Provider` | Cella endpoint | Notes |
|---|---|---|
| `Create` | `POST /v1/sandboxes` (SandboxManifest body) | See manifest mapping below. |
| `Destroy` | `DELETE /v1/sandboxes/{id}` | Idempotent server-side; 404 → success. |
| `HealthCheck` | `GET /v1/sandboxes/{id}` | nil iff `state==running`; 404 → `ErrNotFound`. |
| `StreamExec` | `POST .../commands` then `GET .../commands/{cid}/logs?follow=true` (SSE) | Commands are async; `detach` defaults true. |
| `Exec` | built on `StreamExec` | Stream to EOF, then fetch the command record for the exit code. |
| `ExecStream.Result` | `GET .../commands/{cid}` | `phase` + `exit_code` map 1:1 to `ExecResult`. |
| `ReadFile` | `POST .../files/export` `{src_dir: dir(path), paths: [base(path)]}` | Read the single entry from the returned tar. |
| `ListFiles` | `POST .../files/export` `{src_dir: path, paths: ["."]}` | Parse tar headers → `FileInfo`; keep only immediate children (tar recurses). |
| `WriteFile` | `POST .../files/import` (multipart `tarball`, `dest: dir(path)`) | Wrap `data` in a one-entry in-memory tar. |

### Create: SandboxManifest mapping

The flat `CreateSandbox` body is deprecated; the provider sends the
Kubernetes-style `SandboxManifest` envelope (`apiVersion: cella.latere.ai/v1`,
`kind: Sandbox`). `sandbox.CreateOptions` maps as:

| `CreateOptions` | Manifest path |
|---|---|
| `Name` | `metadata.name` (empty → server generates a slug) |
| `Labels` | `metadata.labels`: the provider stamps `kind=agent` into a copy (the backend tags every agent sandbox; per the `CreateOptions.Labels` contract), never mutating the caller's map and not overriding a caller-supplied `kind`. Reserved `sandbox.latere.ai/` prefix is server-rejected. |
| `Image` | `spec.image` (empty → platform base image) |
| `Env` | `spec.env` |
| `Tier` | `spec.tier` (default `ephemeral`) |
| `Policy` | `spec.policy` (empty → caller default; the brain runner sets `brain`) |

The provider always sets `spec.lifecycle.autoStop` to a bounded default (e.g.
`15m`) so an orphaned sandbox stops on its own; see Lifecycle below.

### Exec lifecycle (async → sync)

Cella commands run detached. `Exec` is implemented *on top of* `StreamExec` so
the start → stream → terminal lifecycle lives in one place:

1. `POST .../commands` with `{argv, env, cwd}` → a `command_id`.
2. A background goroutine pulls output with **cursor-based polling**
   (`GET .../commands/{cid}/logs?stream=false&cursor=N`) and writes each chunk
   into an `io.Pipe`. `Recv` reads the pipe, so a slow consumer backpressures
   the poller.
3. The cursor envelope (`{bytes, next_cursor, phase, exit_code}`) carries the
   terminal `phase` and `exit_code` **inline**, so when the phase leaves
   `running` the goroutine records them and closes the pipe; no extra
   `GET .../commands/{cid}` is needed. `lost` carries no exit code.

`Exec` drains the stream to EOF and returns `Result`. A context cancellation is
a terminal `killed` phase with a clean EOF (not a transport error), matching the
local provider.

Cursor polling was chosen over SSE follow because the cursor envelope is fully
specified (including terminal phase/exit code) and trivially testable against an
`httptest` fake, whereas SSE would still require a second request to recover the
exit code. The `bytes` field is raw combined stdout+stderr text (confirmed
against the server's own cursor-mode handler), not base64.

### Error mapping

The interface error contract is already specified; the client's response decoder
implements it: `404 → ErrNotFound`, `409 → ErrConflict`, every other non-2xx →
`*APIError{Status, Code, Message, RequestID}` populated from Cella's error
envelope. Local failures (context cancelled, transport) surface as stdlib errors.

### Injection into the runner

`Run` currently hardcodes `local.New()` (`topos.go`). Add one field to `Options`:

```go
// Sandbox is the execution backend for the run. When nil, the runner uses
// the local temp-directory provider (sandbox/local), so the zero-config path
// needs no external services.
Sandbox sandbox.Provider
```

`Run` uses `r.opts.Sandbox` when set, else `local.New()`. Because the delegate
tool already creates per-child sandboxes through the injected `Provider`
(`delegateTool.Invoke`), delegated peers automatically get their own Cella
sandboxes with no further change. The root `topos` package still imports only
`sandbox` and `sandbox/local`, never `sandbox/cella`; the host wires Cella in:

```go
prov := cella.New(cella.Options{BaseURL: "https://cella.latere.ai", Token: src})
r, _ := topos.NewRunner(topos.Options{Sandbox: prov, Model: ...})
ctx = sandbox.WithBearer(ctx, userBearer)
res, _ := r.Run(ctx, region, task)
```

### Readiness wait (cold start)

Cella's `Create` may return a sandbox still in `creating` (the interface
documents that callers needing `running` must poll `HealthCheck`). `Run` and the
delegate path therefore call a bounded `waitRunning` helper after each `Create`,
polling `HealthCheck` until running (30s budget, 200ms cadence). This is a no-op
for `sandbox/local`, whose `Create` already returns `running` and whose
`HealthCheck` passes on the first call, so the local path is unchanged; an async
backend is given time to warm up before the first `Exec`.

## Lifecycle and cost

Unlike `local`'s temp directories, orphaned Cella sandboxes consume real
resources and bill. Two safeguards:

- **Server-side backstop.** Every sandbox is created `ephemeral` with
  `spec.lifecycle.autoStop`, so a sandbox the host forgets to destroy stops
  itself. `Run`'s `defer Destroy(...)` is best-effort and will not fire if the
  process is killed; the autoStop deadline is the guarantee.
- **Destroy reliability.** `Destroy` treats 404 as success and is the explicit
  teardown path. Transient failures are logged; the autoStop backstop covers the
  case where teardown never lands.

## Testing

The CI gate requires ≥90% coverage and the suite must not depend on a live
cluster. Tests run against an `httptest.Server` that serves the OpenAPI shapes:

- Error mapping: 404/409/5xx → `ErrNotFound`/`ErrConflict`/`*APIError`.
- Exec lifecycle: command start, SSE log streaming to EOF, terminal `phase` and
  `exit_code` retrieval, and the `killed`/`lost` phases.
- Tar round-trip: `WriteFile` produces a tar the fake unpacks; `ReadFile` and
  `ListFiles` parse a tar the fake serves, including immediate-child filtering.
- Token sources: `ContextTokenSource` reads `BearerFromContext`; missing bearer
  is surfaced clearly.
- A `var _ sandbox.Provider = (*cella.Provider)(nil)` compile assertion.

## Implementation plan

Sliced into small, independently testable commits (tests first):

1. `sandbox/cella` skeleton: `client.go` (`do`, error decoder), `token.go`
   (`TokenSource`, `ContextTokenSource`, `StaticTokenSource`).
2. `Create` / `Destroy` / `HealthCheck` with SandboxManifest mapping.
3. `StreamExec` + `Exec`-on-top, phase/exit-code mapping.
4. Tar-based `ReadFile` / `WriteFile` / `ListFiles`; verified against
   `harness/lift.go` and `harness/drop.go`.
5. `Options.Sandbox` injection in `topos.go` + README/usage docs.

## Open questions

- Image catalog: the provider leaves `Image` free-form and supplies a default
  base image (`defaultImage`) when empty, letting the server validate against
  its catalog. A typed enum was rejected: the catalog is a server concern that
  changes independently.
- Workspace root: file ops keep `src_dir`/`dest` at the server default
  (`/workspace`) and pass workspace-relative paths in the tar, so no workdir is
  hardcoded. If Cella ever varies the workdir per sandbox, this assumption (and
  only this assumption) revisits.
- Missing-file `ReadFile`: a path the sandbox lacks is treated as ErrNotFound by
  finding no matching entry in the export tar. The server runs
  `tar -C /workspace -cf - <path>`, which exits non-zero *after* a 200 has begun
  streaming; against a live server this may instead surface as a tar-parse error.
  To be confirmed with a real sandbox; if so, ReadFile maps that to ErrNotFound.
- Killing on cancel: a cancelled `Exec` stops polling and reports `killed`, but
  does not yet `DELETE .../commands/{cid}` to stop the server-side process. The
  `autoStop` backstop bounds the cost; an explicit kill can be added later.

## Outcome

Implemented in package `sandbox/cella`:

- `client.go`: HTTP client (`New`, `send`, `doJSON`), the `{code,message,
  request_id}` error decoder mapping 404→`ErrNotFound`, 409→`ErrConflict`, else
  `*sandbox.APIError`.
- `token.go`: `TokenSource`, `ContextTokenSource` (reads `sandbox.WithBearer`),
  `StaticTokenSource`.
- `provider.go`: `Create` (SandboxManifest body, default image + `autoStop`
  backstop), `Destroy` (idempotent on 404), `HealthCheck`, and the
  `var _ sandbox.Provider` assertion.
- `exec.go`: `StreamExec` via cursor polling into an `io.Pipe`, `Exec` on top.
- `files.go`: tar-based `ReadFile`/`ListFiles`/`WriteFile`.

The injection point is `topos.Options.Sandbox` (`topos.go`); it defaults to
`sandbox/local` when nil. `Run` and the delegate path call `waitRunning` after
each `Create` so an async backend's `creating` sandbox is given time to reach
`running` before first use. The boundary test (`sandbox/boundary_test.go`) and
root-package tests (injected provider is used; readiness wait succeeds and times
out) all pass. Tested against an `httptest` fake (no live cluster); total repo
coverage ~95%.

Caveats that only a live Cella can settle are tracked under Open questions
(missing-file ReadFile, server-side kill on cancel, default image catalog ref).
