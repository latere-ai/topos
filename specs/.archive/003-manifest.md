---
title: "manifest/v1: the Agent, Trigger, MemoryStore and Connection kinds, references, versioning, one resolver"
status: complete
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

`manifest/v1` holds the four kinds as Go types and `manifest` holds the
resolver, `AgentConfig` and the session bundle; `topos run --agent`
runs a resolved Agent ([[024-client-cli-skill]]). The server resolves
`PUT /v1/agents/{name}` through a `Lookup` over its store, scoped to the
agents the caller may read ([[015-api]]). The topos command's local
state and `topos apply` are not built, so a local run resolves every
reference inside its own file.

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
| `permissions` | list of `{action, resource}` | none | the agent's grants, the reach of its identity ([[006-identity]], [[018-credentials-and-secrets]]) |
| `identity` | `agent` or `person` | `person` | removed with [[018-credentials-and-secrets]]: the agent's owner, a person or an organization, decides, so an organization's agent applied without the field no longer acts as whoever applied it |
| `approvals.mode` | `plan`, `confirm`, `progressive` | `confirm` | [[012-permissions-and-approvals]] |
| `approvals.alwaysAllow`, `approvals.alwaysConfirm` | list of patterns | none | [[012-permissions-and-approvals]], used only when no authorizer supplies the lists |
| `approvals.thresholds` | `{flagAt, askAt, blockAt}` in `[0, 1]`, increasing | `0.3`, `0.5`, `0.9` | [[012-permissions-and-approvals]] |
| `hooks` | list of `{event, matcher, command, timeout}` | none | [[012-permissions-and-approvals]] |
| `subagents` | list of `{name, agent}` or `{name, spec}` | none | `agent` is a reference, `spec` an inline Agent spec ([[013-threads-and-subagents]]) |
| `threads.maxDepth` | integer 1 to 4 | `2` | [[013-threads-and-subagents]] |
| `threads.maxConcurrent` | integer 1 to 32 | `8` | [[013-threads-and-subagents]] |
| `advisor` | `{model, instructions}`, `model` in the form of `spec.model` | none | [[013-threads-and-subagents]] |
| `skills` | list of `{path}` or `{git, ref, path}` | none | [[011-instructions-and-skills]] |
| `mcpServers` | list of `{name, command, args, env, url, connection}` | none | [[021-mcp-servers]]; `command` for stdio, `url` for streamable HTTP, exactly one |
| `memoryStores` | list of `{name, access}`, `name` a reference, `access` `readWrite` or `readOnly` and required | none | [[020-memory-stores]] |
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
| `session.title`, `session.machine`, `session.resources`, `session.budget`, `session.limits` | as the session's fields, camelCase: `machine` `{kind, image, environment}`, each resource `{type, memoryStore, access}` with `type: memoryStore` or `{type, url, ref}` with `type: repository` | the agent's | [[004-session-log]] |
| `session.endOnIdle` | boolean | `true` | [[004-session-log]] |
| `skipIfActive` | boolean | `true` | [[022-triggers]] |
| `maxAge` | Go duration | `1h` | [[022-triggers]] |
| `suspend` | boolean | `false` | [[022-triggers]] |

### MemoryStore

| Field | Type | Default | Behavior in |
|---|---|---|---|
| `description` | string, at most 1024 characters, written for the model | required | [[020-memory-stores]], [[011-instructions-and-skills]] |
| `sharing` | `initiator` or `shared` | `initiator` | [[020-memory-stores]]; `shared` is the authorizer's to allow, by default to an organization admin |
| `audience` | string, the group the authorizer knows | none; required when `sharing` is `shared` | [[020-memory-stores]] |

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
store as a name or `mem_<ulid>`; a Connection by name only, since it has
no id. A name resolves first to a document of the same file, then
through a `Lookup` its caller supplies (the server's store, or the local
state directory for the CLI); an id resolves through the `Lookup`. The
resolver writes the resolved spec with ids and versions in place of
names: a subagent as `agent_<ulid>@<n>`, so a resolved Agent pins its
subagents' versions; a trigger's `agent` as `agent_<ulid>`, so the
trigger runs the agent's latest version, unless the manifest names a
version; a memory store as `mem_<ulid>` and a credential as
`cred_<ulid>`. The documents of one file resolve in dependency order,
each after the documents it references; a cycle among them has no order
in which versions can be pinned and is refused with `invalid_manifest`.
Two documents of one kind and name are refused the same way.

### No secret in a manifest

No field holds a secret value; credentials exist only as Credential
objects created through the API, write-only
([[018-credentials-and-secrets]]). Decoding is strict, so a field named
`value`, `token` or `password` is refused as unknown. The free-text
fields that could still carry one (`instructions`, after
`instructionsFile` is read, of the agent, its inline subagents and its
advisor; `mcpServers[].env` values and `args`; hook `command`; a
trigger's `session.message`) and the credential references, where a
pasted key would otherwise pass as a name, are checked by the input
check of [[018-credentials-and-secrets]], and a match is refused with
`manifest_holds_secret`, naming the field path and never the value. No
problem of any code quotes a string value of the manifest. Until
`session/inputcheck` exists the resolver carries its own copy of the
check, which counts a base64 run as high-entropy only when it mixes
letters and digits, so an identifier such as a long CamelCase test name
in the instructions is not refused.

### The resolver

```go
func Resolve(ctx context.Context, docs []byte, o Options) ([]Resolved, error)
```

One function, in five stages: decode (YAML documents separated by
`---`, or a stream of JSON values when the file starts with `{`), strict
field check, defaulting, validation (every problem collected, each with
its field path), and reference resolution. The stages refuse in order:
a document of another `apiVersion` refuses the file with
`unsupported_version` before anything else is reported; then every
decoding, strict-check, defaulting and validation problem of every
document comes in one `invalid_manifest`, where a field that did not
decode is not reported again as missing; then `manifest_holds_secret`;
then every unknown reference in one `unknown_reference`. `status` is
ignored on input. `instructionsFile` is read through the file system the
caller hands the resolver, rooted at the manifest's directory; a caller
that hands none, such as the API, refuses it.

A default in the tables that is a fixed value is written into the
resolved spec. One that comes from outside the manifest (the catalog,
`TOPOS_MODELS_URL`, Cella's defaults, the installation's Environment,
the agent a trigger runs) is applied where the value is used and is not
written, so a digest depends on the manifest and the build's fixed
defaults alone, and the CLI and the server agree on it.

`Resolved` carries the kind, the resolved spec in canonical JSON (the
byte form of [[004-session-log]]: fields in declaration order, HTML
escaping off, no trailing newline, fixed defaults written out, every
optional field without a value left out), its digest, the resolved
object with its status, and for an Agent the agents its subagents pin,
down to its `maxDepth`. The digest covers the spec alone: a label or an
annotation changes no version. The resolver writes `status` itself from
the `Lookup`'s answer for the object's name: the same digest as the
stored latest version is that version, a different digest is the next
version under the same id, and an object the `Lookup` does not hold is
version 1 under a new id. Applying a resolved object stores the version
the status names, so applying the same Agent twice creates one version
and a changed one creates the next. The documents come back in
dependency order.

`AgentConfig` turns a resolved Agent into the pieces of a harness
configuration that need no I/O: its instructions, the tools it holds,
the policy (mode, lists, thresholds, `machine.egress`), `spec.model`
with its catalog overlay and effort, the subagents as
`harness.Subagent` values nested to `maxDepth`, `maxDepth`,
`maxConcurrent`, `compactAt`, the limits and the budget. The caller
fills each subagent's model and connection. A session pins the version
it was created with, and the resolved manifest is a blob of the session
([[004-session-log]]); beside it the session keeps the bundle, the
resolved agent and the agents it pins as one JSON stream, so another
runner continues the session without the store that resolved it.

### Versioning

`topos.latere.ai/v1` changes only by adding optional fields; a
manifest v1 accepts today is accepted by every later v1 build, and
resolves to the same digest when it sets no new field. One default is
the build's own: an agent that leaves out `tools` holds every built-in
of the build that resolves it, so a build that adds a built-in moves
that agent to a new version rather than widening a pinned one. Another
version is refused with `unsupported_version`. A `Graph` kind joins in
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
| Every agent of `manifest/testdata/` that both the topos command's resolver and `PUT /v1/agents/{name}` resolve comes back as one canonical spec: byte for byte with the same digest when it pins no other agent, and byte for byte up to the ids of the agents it pins, which each store mints; an agent naming a memory store or a connection, which the API holds no route for yet, is `unknown_reference` there | `internal/toposcli.TestCLIAndServerResolveAgree` | built |
| Every file in `manifest/testdata/` resolves to the specs, statuses and digests of its golden file, and a YAML file and the same documents as a JSON stream resolve alike | `manifest.TestTestdataResolves`, `manifest.TestYAMLAndJSONStreamsResolveAlike` | built |
| An unknown field, a wrong type and an out-of-range value are each refused with `invalid_manifest` and their field paths, all in one error | `manifest.TestStrictDecodingCollectsEveryProblem` | built |
| A manifest carrying a token in `instructions` or in an MCP server's `env` is refused with `manifest_holds_secret`, and the error text holds the path and not the value | `manifest.TestManifestHoldingASecretIsRefused`, `manifest.TestTheInputCheck` | built |
| Every fixed default of the four kinds' tables is written into the resolved spec | `manifest.TestDefaultsAreWrittenOut` | built |
| Resolving the same Agent against its stored version keeps that id and version, and changed instructions resolve to the next version under the same id | `manifest.TestAgentVersioning`, `manifest.TestStoredObjectsKeepTheirIDs` | built |
| Applying the same Agent twice through the API creates one version; changing its instructions creates the next under the same id | `internal/server.TestApplyingAnAgentVersionsItsSpec` | built |
| A resolved Agent pins each subagent reference to an id and version, and every unknown reference is reported in one `unknown_reference` | `manifest.TestReferencesPinVersions`, `manifest.TestStoredSubagentsArePinnedTransitively` | built |
| `threads.maxDepth: 5` and a Connection with `mode: person` and a `credential` are refused | `manifest.TestValidationRules` | built |
| A label under `topos.latere.ai/` is refused | `manifest.TestReservedLabelsRefused` | built |
| Another `apiVersion` refuses the file before any other problem, a cycle among one file's documents is refused, and `instructionsFile` is read only through the caller's file system | `manifest.TestTheEnvelopeIsCheckedFirst`, `manifest.TestReferenceCyclesAndLookupFailures`, `manifest.TestInstructionsFile` | built |
| `AgentConfig` carries a resolved Agent's instructions, tools, policy, model overlay, subagents to `maxDepth` and limits, and the bundle reads back to the same configuration and refuses one whose spec does not hash to its digest | `manifest.TestAgentConfigCarriesTheHarnessPieces`, `manifest.TestBundleRoundTrip` | built |

## Outcome

Built on 2026-09-27. The four kinds are Go types in `manifest/v1`, and
`manifest.Resolve` is the one resolver: the topos command calls it with
the manifest's directory and no stored objects, and `PUT
/v1/agents/{name}` with its store's `Lookup` and no files. Every row of
the acceptance table passes.

### What was built

| Piece | Where |
|---|---|
| the kinds, their fields and their JSON form | `manifest/v1` |
| decoding, the strict check, defaulting, validation, references, canonical JSON and the digest, versioning against the `Lookup` | `manifest/decode.go`, `manifest/defaults.go`, `manifest/validate.go`, `manifest/resolve.go` |
| the input check for secrets, the resolver's own copy | `manifest/secret.go` |
| `AgentConfig` and the session bundle | `manifest/config.go`, `manifest/bundle.go` |
| the server's `Lookup`, scoped to the agents the caller may read, and the apply route | `internal/store` (`Lookup`), `internal/server/agents.go` |
| the topos command's resolution of a manifest file | `internal/toposcli` (`resolveFile`) |
| the fixtures and their golden resolutions | `manifest/testdata/` |

### What diverges from the design as written

| What it said | What was built | Why |
|---|---|---|
| a digest depends on the manifest and the build's fixed defaults alone, so the CLI and the server agree on it | they agree byte for byte for an agent that pins no other; an agent that pins one carries the pinned `agent_<ulid>@<n>`, and each store mints its own ids, so the two digests of such an agent differ | a pin names a stored version, and the CLI's local state and the server's store are two stores; agreement over stored references is `topos apply`'s ([[024-client-cli-skill]]) |
| a name resolves through the local state directory for the CLI | the topos command resolves with no `Lookup`, so every reference names a document of the same file, and a manifest naming a credential does not resolve there | the local state and `topos apply` are [[024-client-cli-skill]]'s, not built |
| the MemoryStore has `sharing` (default `initiator`) and `audience` | `v1.MemoryStoreSpec` holds `description` alone, so a manifest that sets either is refused as an unknown field | the fields were added to the deck for [[020-memory-stores]], which is not built |
| `machine.image` defaults to Cella's `base`, and a default from outside the manifest is not written | `base` is written into the resolved spec of a `cella` machine as a fixed default, and a host machine takes none | the resolver treats the image as this build's own default, so the digest does not move with Cella's |
| the server holds the four kinds | it holds Agents; a manifest naming a memory store or a connection is `unknown_reference` on the API | the routes are [[020-memory-stores]]'s and [[018-credentials-and-secrets]]'s |

### What this leaves open

| Open | Why |
|---|---|
| a manifest with `instructionsFile` through the API | the API reads no files and refuses the field; a client inlines the file before it applies, as the `topos apply` row of [[024-client-cli-skill]] requires |
| `status.identity`, the subject an agent's key acts as | the resolver carries a stored agent's identity forward and writes none for a new one; provisioning is [[018-credentials-and-secrets]]'s |
