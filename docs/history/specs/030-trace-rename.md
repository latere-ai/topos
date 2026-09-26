---
title: Rename Lineage to Trace
status: complete
track: runtime
depends_on:
  - specs/008-trace.md
affects:
  - topos.go
  - graph/graph.go
  - harness/hooks/events.go
  - adversarial/doc.go
  - adversarial/critic/critic.go
  - README.md
effort: small
created: 2026-08-07
updated: 2026-08-07
author: changkun
dispatched_task_id: null
---

# Rename Lineage to Trace

## Problem

The run graph a `Runner` returns is named `Lineage`. The word is a term of art in
data and ML tooling (data lineage, OpenLineage, MLflow), but not in the vocabulary
of the audience that imports a Go agent runtime. A reader meeting `res.Lineage`
has to be told what it holds before the name means anything, which is the opposite
of what a public API field should do.

## Decision

Rename the concept to `Trace`. Tracing is the established word for a recorded
account of what one request or run actually did, and Go and infrastructure
developers already own it. It carries no collision inside this module, and it
leaves an honest path to exporting the structure as OpenTelemetry spans later.

The alternatives considered and rejected: `Provenance` and `Ancestry` trade one
uncommon word for another; `History` reads as linear and the structure is a DAG;
`CallGraph` denotes the calls that could happen rather than the ones that did;
`Record`, `Ledger`, `RunGraph`, and `Topology` collide with existing names in the
module.

The cost is a breaking change to the public surface, so this ships as a minor
pre-1.0 release.

## Scope

- `Lineage`, `LineageNode`, and `LineageEdge` become `Trace`, `TraceNode`, and
  `TraceEdge`.
- `RunResult.Lineage` becomes `RunResult.Trace`, on both `Run` and `RunGraph`.
- Internal identifiers, doc comments, examples, and README prose follow.
- Spec `008-lineage.md` keeps its number and becomes `008-trace.md`; the two
  references to its path move with it.

Out of scope: no serialization changes, because no JSON or YAML tag ever carried
the word; no behavior change to how the graph is built or ordered; no changes to
downstream repositories that consume the field. Retired specs under
`specs/.archive/` keep their original wording, because they record what was
decided at the time rather than what ships now.

## Verification

The existing suite covers the structure under its new name. The rename is
mechanical, so the gate is that `make all` passes unchanged: build, vet, lint,
race, and the coverage threshold.

## Acceptance criteria

- Outside `specs/.archive/`, no identifier, comment, doc, or spec in the module
  uses the word lineage for the run graph.
- `RunResult.Trace` carries what `.Lineage` carried, with
  identical node ids, edge kinds, and ordering.
- `make all` passes.

## Outcome

Implemented on 2026-08-07. The public surface exposes `Trace`, `TraceNode`, and
`TraceEdge`, reached through `RunResult.Trace`. The
`adversarial` capability reports per-fork traces under the same term.

Consumers outside this module break on upgrade where they read `.Lineage`: the
hosted Topos platform's run and API layers, its frontend locale strings, and
`latere-cli/internal/commands/eval.go`.
