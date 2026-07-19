---
title: Sandbox Credential Delivery
status: complete
track: runtime
depends_on:
  - specs/010-sandbox-cella.md
affects:
  - sandbox/provider.go
  - sandbox/cella/provider.go
  - sandbox/cella/exec.go
  - sandbox/local/provider.go
  - README.md
effort: small
created: 2026-06-28
updated: 2026-06-28
author: changkun
dispatched_task_id: null
---

# Sandbox Credential Delivery

## Goal

Let a host deliver secrets *into* a sandbox (provider API keys, third-party
tokens, the kind of value one would otherwise paste into a `.env`) without ever
putting the secret value on the wire in plaintext or on a command's argv. This
extends the `sandbox.Provider` interface with backend-neutral credential fields
and maps them onto Cella's vault, while the local provider treats them as a
no-op.

## Background: the three credential surfaces

This spec covers only the third surface below; the first two already exist.

1. **API auth**: the bearer Topos presents to the backend. Owned by
   `TokenSource` / `ContextTokenSource`; the host mints, the provider presents.
   See [Cella Sandbox Provider](010-sandbox-cella.md).
2. **Lift/drop secret deny-list**: `harness/lift.go` and `drop.go` refuse to
   copy laptop secrets (`.env`, `*.pem`, `.ssh/`, `.aws/credentials`) into the
   sandbox or materialise sandbox-born secrets back. Provider-agnostic; works
   through the interface already.
3. **In-sandbox credential delivery**: getting a vault-held secret to the
   workload running inside the sandbox. This is the gap this spec closes.

## The problem with `Env`

`CreateOptions.Env` and `ExecOptions.Env` already exist, but their values travel
as plaintext JSON in the request body, and command env can leak via
`/proc/<pid>/cmdline` on some paths. They are the right channel for non-secret
configuration and the wrong one for secrets. Cella solves this with a vault:
the host stores `(scope, NAME) → value` entries out of band, and the sandbox
references them by name only; the value is resolved server-side.

Cella delivers vault entries two ways:

- **File mounts** at `/run/cella/secrets/<NAME>`, chosen at create time
  (manifest `spec.secrets.mount`). Omitting the field mounts the caller's
  `default_mount` entries; an empty array mounts none; a list mounts exactly
  those.
- **Per-command env** (`env_from_vault` on the command API): a map of env-var
  name → vault entry name, resolved into a tmpfs file for that one command. The
  value never appears on argv.

## Design

### Interface extension (backend-neutral)

Two fields, named for *what they do*, not for Cella:

```go
// CreateOptions
//   SecretMounts names secret entries the backend mounts read-only into the
//   sandbox filesystem at start. A nil slice requests the backend's default
//   set; a non-nil slice (including empty) requests exactly those names (empty
//   = mount none). The local provider ignores this field.
SecretMounts []string

// ExecOptions
//   SecretEnv maps environment-variable names to backend secret-entry names.
//   The backend resolves each value server-side and injects it for this command
//   only, without exposing it on argv. The local provider ignores this field.
SecretEnv map[string]string
```

The nil-vs-empty distinction on `SecretMounts` is deliberate: it is the only way
to express "mount nothing" versus "use the backend default", which Cella
distinguishes.

### Cella mapping

- `Create`: when `SecretMounts != nil`, set `spec.secrets.mount` to it (even when
  empty, so an empty slice serialises as `[]` and means "mount none"). When nil,
  omit `spec.secrets` entirely so the server applies `default_mount`. This
  requires a pointer `secrets` block and a `mount` field **without** `omitempty`,
  since `omitempty` cannot tell nil from `[]string{}`.
- `Exec`: map `SecretEnv` to the command body's `env_from_vault` (omit when
  empty).

### Local mapping

The local provider ignores both fields; it has no vault, and per the decision
recorded here a secret-dependent agent simply runs without those secrets in
local development. No code change is needed (local already reads only
Argv/Env/Cwd/Name); tests assert the no-op (no error) explicitly.

## Out of scope

- Vault CRUD (`/v1/credentials` set/rotate/delete) stays a host/dashboard
  concern; the provider only *references* entries by name, never reads or writes
  values.
- Trust planes and `credential_use_id` correlation are not surfaced; they are
  governed by the sandbox `Policy`, which is already wired.

## Acceptance criteria

- `CreateOptions.SecretMounts` and `ExecOptions.SecretEnv` exist with the
  documented nil/empty/non-empty semantics.
- Cella `Create` sends `spec.secrets.mount` only when `SecretMounts != nil`, and
  serialises an empty slice as `[]`.
- Cella `Exec` sends `env_from_vault` when `SecretEnv` is non-empty.
- Local provider ignores both without error.
- README documents how a host delivers a secret into a Cella sandbox.

## Outcome

Implemented:

- `sandbox/provider.go`: added `CreateOptions.SecretMounts []string` and
  `ExecOptions.SecretEnv map[string]string` with the documented nil/empty
  semantics.
- `sandbox/cella/provider.go`: `Create` adds a pointer `spec.secrets` block
  (`mount` without `omitempty`) set only when `SecretMounts != nil`, so nil
  omits the block (default_mount), `[]` serialises as mount-none, and a list
  mounts exactly those. `sandbox/cella/exec.go`: `Exec` maps `SecretEnv` to the
  command body's `env_from_vault`.
- `sandbox/local/provider.go`: unchanged; it already reads only
  Argv/Env/Cwd/Name, so the new fields are ignored. A test
  (`TestLocalIgnoresVaultCredentials`) pins the no-op.
- Cella tests cover the three `SecretMounts` cases and the `env_from_vault`
  mapping (asserting the secret value never rides plaintext `env`). README
  documents the host-facing usage.

All tests pass under `-race`; total repo coverage stays ~95%.
