---
title: Tool Grant Enforcement
status: drafted
track: runtime
depends_on:
  - specs/006-delegation.md
  - specs/008-trace.md
affects:
  - topos.go
  - harness/tools/
  - harness/subagent.go
  - graph/graph.go
  - adversarial/critic/
effort: medium
created: 2026-09-12
updated: 2026-09-12
author: changkun
dispatched_task_id: null
---

# Tool Grant Enforcement

## Problem

Agent tool declarations currently change the trace without restricting the
registry passed to the loop. A model can invoke bash or write tools despite a
read-only declaration, and delegated peers can exceed their attenuated grant.
Issue #12 requires the offered registry, dispatch authority, and trace to agree.

## Contract

`AgentSpec.Tools` selects builtins. A nil declaration retains all six builtins
for compatibility; an explicit empty slice grants none. Exact builtin names are
accepted. Existing family names expand as follows: `read` selects `read_file`,
`grep`, and `glob`; `write` selects `write_file` and `edit_file`; `exec` selects
`bash`. Unknown names grant nothing. Selection deduplicates in builtin order.

Dynamic delegation remains controlled by topology and depth. The `delegate`
tool is added when the existing delegation rules permit it, independently of
the builtin declaration. This preserves the existing read/write lead examples.
`TraceNode.Grants` records the concrete names in the applied registry, including
`delegate` where present. Nodes that fail before starting a loop record no tools.

Before spawning, the runner resolves the parent's and requested peer's builtin
declarations to concrete capabilities. The harness retains its existing
intersection semantics; nil capabilities there mean none. Child execution uses
the resulting capabilities, preserving an empty intersection through every
mesh hop. `Runner.Turn`'s separate nil-registry default remains unchanged.

Graph JSON preserves an explicit empty tools array so persistence cannot turn a
denial into the nil default. Native critics explicitly request no tools when
`Config.Tools` is nil; their prompt already contains the diff. An explicit
critic grant opts into the same builtin selection contract.

## Implementation

The tools package supplies selection and stable concrete names. The runner
builds each registry before recording its grants and starting the loop. The
loop already rejects calls absent from its registry, even if a model invents
them or a permission hook allows them; no new execution path is required.

SDK comments and contributor specs describe declarations and attenuation
precisely. README and critic documentation explain tool availability and
defaults. Denied dispatch retains the existing unknown-tool error result.

## Acceptance criteria and verification

- Pinned steps and dynamic entries expose only selected builtins; forced calls
  to omitted bash/write tools return errors without invoking the sandbox.
- Allowed reads succeed. Nil, empty, aliases, duplicates, and unknown names have
  explicit tests.
- Delegated children use the intersection; nil peer declarations inherit only
  the parent's capabilities, and disjoint grants remain empty through mesh
  recursion.
- Each executed node's trace equals the tools offered to its model. Delegation
  follows the existing topology and depth rules.
- Graph JSON round-trips nil and empty grants distinctly through `ToRuntime`.
- A default native critic cannot execute or write; explicit tools work.
- Regression tests fail before the fix and pass afterward; package suites,
  race checks, and repository gates pass.
