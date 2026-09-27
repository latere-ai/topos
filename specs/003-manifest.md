---
title: "manifest/v1: the Agent, Trigger, MemoryStore and Connection kinds, references, versioning, one resolver"
status: drafted
track: core
depends_on: [001-architecture.md]
affects: [manifest/, manifest/v1/, manifest/testdata/]
effort: large
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# manifest/v1

## Overview

Everything an installation's console can declare about an agent is a
file. `manifest/v1` defines four kinds under the API group
`topos.latere.ai/v1`: **Agent** (its model, instructions, tools,
permissions, identity, subagents, skills, MCP servers, memory stores,
connections, machine defaults, budget and limits), **Trigger** (a
schedule that starts sessions), **MemoryStore** and **Connection** (how
an agent reaches a third-party service, never the secret itself). This
spec owns every field of the four kinds. One resolver decodes,
defaults, validates and resolves references, and the CLI and the server
call the same function, so an agent file resolves to the same bytes in
both. A manifest never holds a secret: credentials are named by
reference, and a manifest that holds one is refused.

## Current state

v0.7.0 declared agents in Go (`AgentSpec`, `Region`) and had no file
form; the retired hosted service stored agent definitions as untagged
Go structs. Nothing is borrowed except the shape of the family's
manifests: a Kubernetes-style object with `apiVersion`, `kind`,
`metadata`, `spec` and `status`, strict decoding, and one resolve, as
Lux's and Cella's manifest specs define theirs.

## Design

### The object

```yaml
apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: reviewer
  labels: {team: platform}
spec:
  description: Reviews a change and fixes what it finds.
  model: {name: claude-sonnet-4-6, effort: high}
  instructions: |
    Review the diff on the current branch. Fix what you can and list the rest.
  tools: [read, edit, bash, grep, glob]
  approvals: {mode: progressive, alwaysAllow: ["bash(go test *)"]}
  subagents:
    - {name: tester, agent: tester}
  memoryStores:
    - {name: review-notes, access: readWrite}
  machine: {kind: cella, image: base, egress: [proxy.golang.org, sum.golang.org]}
  budget: {maxCost: "5.00"}
```

Field names are camelCase, as the family's manifests are; the session
log's snake_case JSON is [[004-session-log]]'s, and the two never mix
in one object. `metadata.name` is a DNS label (lowercase letters,
digits and hyphens, at most 63 characters). Labels and annotations
under `topos.latere.ai/` are the core's own, and a manifest that sets
one is refused. `status` is written by the server or the local resolver
and ignored on input: `id`, `version`, `digest` (the `sha256:` of the
resolved spec), `createdAt`, and for an Agent `identity`, the subject
its key acts as ([[018-credentials-and-secrets]]).

### Agent

| Field | Type | Default | Behavior in |
|---|---|---|---|
| `description` | string | none | shown to people, and to a parent thread in `spawn`'s description ([[013-threads-and-subagents]]) |
| `model.name` | string | required | [[007-models]] |
| `model.family`, `model.dialect`, `model.baseURL` | string | from the catalog; `TOPOS_MODELS_URL` | [[007-models]] |
| `model.credential` | reference | none | a Credential by name or `cred_` id ([[018-credentials-and-secrets]]) |
| `model.effort` | `minimal`, `low`, `medium`, `high` | the model's own | [[007-models]] |
| `model.inputWindow`, `model.maxOutputTokens` | integer | from the catalog | [[007-models]] |
| `model.pricing` | `{input, output, cacheRead, cacheWrite}`, USD per million tokens as decimal strings | from the catalog | [[007-models]] |
| `instructions` | string | empty | [[011-instructions-and-skills]] |
| `instructionsFile` | path, relative to the manifest | none | read by the loader and inlined into `instructions`; the resolved spec has no `instructionsFile` |
| `tools` | list of names or `{name, outputLimit, client, description, inputSchema}` | absent: every built-in; `[]`: none | [[008-tools]]; `client: true` declares a client-executed tool, which needs `description` and `inputSchema` |
| `permissions` | list of `{action, resource}` | none | the agent's grants, which become its key's ([[006-identity]], [[018-credentials-and-secrets]]) |
| `identity` | `agent` or `person` | `person` | [[018-credentials-and-secrets]] |
| `approvals.mode` | `plan`, `confirm`, `progressive` | `confirm` | [[012-permissions-and-approvals]] |
| `approvals.alwaysAllow`, `approvals.alwaysConfirm` | list of patterns | none | [[012-permissions-and-approvals]], used only when no authorizer supplies the lists |
| `approvals.thresholds` | `{flagAt, askAt, blockAt}` in `[0, 1]`, increasing | `0.3`, `0.5`, `0.9` | [[012-permissions-and-approvals]] |
| `hooks` | list of `{event, matcher, command, timeout}` | none | [[012-permissions-and-approvals]] |
| `subagents` | list of `{name, agent}` or `{name, spec}` | none | `agent` is a reference, `spec` an inline Agent spec ([[013-threads-and-subagents]]) |
| `threads.maxDepth` | integer 1 to 4 | `2` | [[013-threads-and-subagents]] |
| `threads.maxConcurrent` | integer 1 to 32 | `8` | [[013-threads-and-subagents]] |
| `advisor` | `{model, instructions}` | none | [[013-threads-and-subagents]] |
| `skills` | list of `{path}` or `{git, ref, path}` | none | [[011-instructions-and-skills]] |
| `mcpServers` | list of `{name, command, args, env, url, connection}` | none | [[021-mcp-servers]]; `command` for stdio, `url` for streamable HTTP, exactly one |
| `memoryStores` | list of `{name, access}`, access `readWrite` or `readOnly` | none | [[020-memory-stores]] |
| `connections` | list of Connection names | none | [[018-credentials-and-secrets]] |
| `machine.kind` | `host` or `cella` | `host` | [[009-machines]] |
| `machine.image`, `machine.environment` | string | Cella's `base`; the installation's default Environment | [[009-machines]] |
| `machine.resources` | `{cpu, memory, disk}` as Kubernetes quantities | Cella's defaults | [[009-machines]] |
| `machine.egress` | list of hosts | none | the hosts commands may reach ([[009-machines]], [[012-permissions-and-approvals]]) |
| `machine.roots`, `machine.readPaths` | list of absolute paths | none | host only ([[009-machines]], [[012-permissions-and-approvals]]) |
| `budget.maxCost` | USD as a decimal string | none | [[007-models]] |
| `limits.turnTimeout`, `limits.maxAge` | Go duration | `2h`, `168h` | [[005-harness-loop]], [[004-session-log]] |
| `context.compactAt` | number 0.5 to 0.95 | `0.8` | [[010-context]] |

### Trigger

| Field | Type | Default | Behavior in |
|---|---|---|---|
| `agent` | reference | required | the agent the trigger's sessions run |
| `schedule` | a five-field cron expression, or `@hourly`, `@daily`, `@weekly` | required | [[022-triggers]] |
| `timeZone` | an IANA zone name | `UTC` | [[022-triggers]] |
| `session.message` | string | required | the first `user.message` of each session |
| `session.title`, `session.machine`, `session.resources`, `session.budget`, `session.limits` | as the session's fields | the agent's | [[004-session-log]] |
| `session.endOnIdle` | boolean | `true` | [[004-session-log]] |
| `skipIfActive` | boolean | `true` | [[022-triggers]] |
| `maxAge` | Go duration | `1h` | [[022-triggers]] |
| `suspend` | boolean | `false` | [[022-triggers]] |

### MemoryStore

| Field | Type | Default | Behavior in |
|---|---|---|---|
| `description` | string, at most 1024 characters, written for the model | required | [[020-memory-stores]], [[011-instructions-and-skills]] |

### Connection

| Field | Type | Default | Behavior in |
|---|---|---|---|
| `service` | string, for example `github`, `slack`, `mcp`, `http` | required | [[018-credentials-and-secrets]] |
| `mode` | `person` (the credential of the person running the agent) or `agent` (one credential the agent owns) | required | [[018-credentials-and-secrets]] |
| `hosts` | list of hosts the credential is injected toward | required | [[018-credentials-and-secrets]] |
| `credential` | reference | required when `mode` is `agent`; refused when `person` | [[018-credentials-and-secrets]] |
| `inject` | `{header, format}`, `format` containing `{value}` | `Authorization`, `Bearer {value}` | [[018-credentials-and-secrets]] |

### References

A reference names another object by `metadata.name` or by id: an agent
as `reviewer`, `agent_<ulid>` (the latest version) or
`agent_<ulid>@<n>`; a credential as a name or `cred_<ulid>`; a memory
store as a name or `mem_<ulid>`. The resolver checks each through a
`Lookup` its caller supplies (the server's store, or the local state
directory for the CLI) and writes the resolved spec with ids and
versions in place of names, so a resolved Agent pins its subagents'
versions.

### No secret in a manifest

No field holds a secret value; credentials exist only as Credential
objects created through the API, write-only
([[018-credentials-and-secrets]]). Decoding is strict, so a field named
`value`, `token` or `password` is refused as unknown. The free-text
fields that could still carry one (`instructions`, `mcpServers[].env`
values and `args`, hook `command`) are checked by the input check of
[[018-credentials-and-secrets]], and a match is refused with
`manifest_holds_secret`, naming the field path and never the value.

### The resolver

```go
func Resolve(ctx context.Context, docs []byte, o Options) ([]Resolved, error)
```

One function, in five stages: decode (YAML or JSON, several documents
in one file), strict field check, defaulting, validation (every
problem collected, each with its field path), and reference
resolution. `Resolved` carries the kind, the resolved spec in canonical
JSON (fields in declaration order, defaults written out), and its
digest. Applying an Agent whose digest differs from its latest version
creates the next version; the same digest creates none. A session pins
the version it was created with, and the resolved manifest is a blob of
the session ([[004-session-log]]).

### Versioning

`topos.latere.ai/v1` changes only by adding optional fields; a
manifest v1 accepts today is accepted by every later v1 build, and
resolves to the same digest when it sets no new field. Another version
is refused with `unsupported_version`. A `Graph` kind joins in
[[033-peers-and-authored-graphs]].

### Error codes

| Code | Meaning |
|---|---|
| `invalid_manifest` | decoding or validation failed; `detail` lists every field path and problem |
| `manifest_holds_secret` | a free-text field matched the input check |
| `unknown_reference` | a reference names no object the caller may see |
| `unsupported_version` | an `apiVersion` other than `topos.latere.ai/v1` |

## Not in this spec

What each field does (the Behavior column); the API that applies
manifests ([[015-api]]) and the `topos apply` command
([[024-client-cli-skill]]); the provisioning of an agent's identity and
key on apply ([[018-credentials-and-secrets]]).

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| Every agent file in `manifest/testdata/` resolves to byte-identical canonical JSON and digest through the CLI's lookup and the server's | `TestCLIAndServerResolveAgree` | not built |
| An unknown field, a wrong type and an out-of-range value are each refused with `invalid_manifest` and their field paths, all in one error | `TestStrictDecodingCollectsEveryProblem` | not built |
| A manifest carrying a token in `instructions` or in an MCP server's `env` is refused with `manifest_holds_secret`, and the error text holds the path and not the value | `TestManifestHoldingASecretIsRefused` | not built |
| Every default of the four kinds' tables is written into the resolved spec | `TestDefaultsAreWrittenOut` | not built |
| Applying the same Agent twice creates one version; changing its instructions creates the next | `TestAgentVersioning` | not built |
| A resolved Agent pins each subagent reference to an id and version | `TestReferencesPinVersions` | not built |
| `threads.maxDepth: 5` and a Connection with `mode: person` and a `credential` are refused | `TestValidationRules` | not built |
| A label under `topos.latere.ai/` is refused | `TestReservedLabelsRefused` | not built |
