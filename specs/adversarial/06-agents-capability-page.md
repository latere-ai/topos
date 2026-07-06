---
title: Adversarial Review Capability Page in the Agents Platform
status: proposed
depends_on:
  - specs/adversarial/03-capability-surface.md
affects:
  - agents/frontend/src/views/
  - agents/frontend/src/router.ts
  - agents/frontend/src/i18n/
  - agents/docs/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Adversarial Review Capability Page in the Agents Platform

## Goal

Migrate the `agon.latere.ai` website landing page onto the agents platform as an
"Adversarial Review" capability page, so the capability borrows the platform's
narrative instead of carrying its own. The landing page does not disappear; its
content moves into agents. This replaces the standalone site's reason to exist, so
that [07](07-retire-agon.md) only tears down the emptied site shell and infra, not
the story. Migrating the page here is a prerequisite for that teardown.

Planned here for coordination; implemented in the agents repo.

## Design

The agon site is a Vue 3 + vite-ssg SPA whose copy is data in
`agon/frontend/src/content/{en,zh}.ts`, kept in structural parity by a vitest
check. The agents frontend is the same stack (Vue 3 + Vite, `views/`, `router.ts`,
i18n, a `docs/` tree). The move is therefore a content and framing port, not a
rebuild:

- **Capability page.** Add an "Adversarial Review" view under
  `agents/frontend/src/views/` and a route in `router.ts`, reachable from wherever
  the platform lists capabilities. Frame it as a capability of the platform
  ("your agent adversarially reviews its own work before you see it"), not a
  product with its own identity.
- **Copy.** Port the substantive landing copy from `agon`'s `content/{en,zh}.ts`
  into the agents i18n resources, rewritten to capability register and stripped of
  standalone-product framing (no "the project", no separate brand, no `agon`
  string). Keep the English/Chinese parity the source maintained.
- **Docs.** Add an "Adversarial Review" page to `agents/docs/` (alongside
  `concepts.md` / `overview.md`) describing what the capability does and how it is
  invoked (`latere review` for local use; automatic post-run verification inside
  the platform). Point builders at `topos/adversarial` as the engine.

The page's story is that adversarial review is a stage in the agent's own loop,
backed by `topos/adversarial`. The Doubly-Efficient Debate grounding, which was
the standalone site's differentiator, becomes a paragraph on the capability page,
not a site.

## Non-goals

- No new backend in agents. The page is presentation plus docs; the capability
  runs through `topos/adversarial` in wallfacer and the platform runtime, wired by
  the other specs.
- No carry-over of the `agon` brand, name, colors, or `og`/meta assets. This is
  where the standalone identity is dropped, not renamed.

## Steps

1. Port and reframe the copy into agents i18n (en + zh), stripping `agon`.
2. Add the capability view and route; link it from the capability list/nav.
3. Add the `agents/docs` Adversarial Review page.
4. Build the agents frontend; run its tests (including any i18n parity check).
5. `grep -rniw agon agents` returns nothing.

## Acceptance

- The Adversarial Review capability page renders in the agents frontend, in both
  English and Chinese, with the platform's chrome.
- A docs page documents the capability and points at `topos/adversarial` and
  `latere review`.
- No link or asset points at `agon.latere.ai`; `grep -rniw agon agents` is empty.
- The agents frontend build and tests pass.

## Outcome

To be written when this spec is implemented.
