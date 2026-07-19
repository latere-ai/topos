---
title: Retire agon from the Latere Landscape
status: proposed
depends_on:
  - specs/.archive/017-migrate-wallfacer.md
  - specs/.archive/018-migrate-latere-cli.md
  - specs/.archive/019-agents-capability-page.md
affects:
  - terraform/dns.tf
  - terraform/specs/
effort: medium
created: 2026-07-07
updated: 2026-07-07
author: changkun
dispatched_task_id: null
---

# Retire agon from the Latere Landscape

## Goal

Remove `agon` from the Latere landscape entirely: tear down the site and its
infrastructure, archive the repository, and prove nothing named `agon` remains.
This is the terminal step and the barrier of the program. It may only run after
[04](017-migrate-wallfacer.md), [05](018-migrate-latere-cli.md), and
[06](019-agents-capability-page.md) have landed, because it archives the repository
those steps migrate away from.

Planned here for coordination; implementation spans the terraform repo, the agon
repo, and the cluster.

## Preconditions

- No consumer imports `x/agon`: `grep -rl "latere.ai/x/agon" ../*/go.mod` is empty.
- The agents Adversarial Review capability page is live ([06](019-agents-capability-page.md)),
  so `agon.latere.ai` has no story left to serve.

## Scope

**DNS and infrastructure (terraform repo).**

- Remove `digitalocean_record "agon"` in `terraform/dns.tf` (the `agon`
  subdomain A record) and any ingress/routing that fronts `agon.latere.ai`.
- Remove the `agon-web` service from the cluster: delete its Deployment in the
  `latere` namespace and the manifests under `agon/deploy/prod/`.
- Update the terraform service inventories that list agon as a running service, so
  the infra docs match reality: `terraform/specs/architecture.md` (service table,
  subdomain list), `terraform/specs/release-unification.md` (drop agon from the
  `cli-release.yml` callers), `terraform/specs/local-build-deploy.md`, and the
  `frontend-cdn-decoupling` rollout trackers. Remove agon rows rather than marking
  them migrated; the service is gone, not moved.

**Shared footer link (latere-ui).** The shared `latere-ui` `SiteFooter` renders an
`Agon` product link to `https://agon.latere.ai/` on every page across the Latere
sites (landing, agents, and others that depend on `latere-ui`), driven by a
`footer.products.agon` i18n key and hardcoded in the `latere-ui` package. Removing
the site without removing this link leaves a dead product link everywhere. Scrub it
as part of retirement: remove the `agon` entry from the `latere-ui` `SiteFooter`
component and delete the `footer.products.agon` key from every consuming app's
i18n (`agents/frontend/src/i18n/{en,zh}.ts` and any sibling). This needs a
`latere-ui` release plus a dependency bump in the consumers.

**Site and build (agon repo, pre-archive).**

- The site machinery is deleted with the repo archive, but record what it was so
  the teardown is auditable: `cmd/agon-web`, `internal/web` (embedded SPA),
  `frontend/`, `Dockerfile.web`, `.github/workflows/site.yml`, and `deploy/prod/`.
  None of this moves anywhere; the story lives on the agents page now.

**Repository.**

- Archive the `latere.ai/x/agon` GitHub repository (read-only, history preserved).
  Do this only after the migration specs have landed and the engine lives in
  `topos/adversarial`, so no history is lost and nothing depends on the repo.

## Definition of done

These are the program's acceptance checks from the [overview](013-overview.md),
made executable:

- `grep -rl "latere.ai/x/agon" ../*/go.mod` returns nothing across every Latere
  repo.
- `grep -rniw agon ../{topos,wallfacer,latere-cli,agents}` returns nothing in
  source, config, docs, or deploy manifests.
- `terraform/dns.tf` has no `agon` record; `curl -sSf https://agon.latere.ai`
  fails to resolve or serve.
- No `agon-web` Deployment exists in the `latere` namespace.
- The `latere.ai/x/agon` repository is archived.
- Adversarial review still works end to end from its new home: `latere review`
  runs, and wallfacer post-run verification runs, both against `topos/adversarial`.

## Out of scope

- Generated test artifacts that incidentally mention `agon.latere.ai` (for example
  `sandbox/frontend/qa/**/error-context.md`) are stale fixtures, not live
  references. They are not part of the landscape and need not be scrubbed to call
  the program done; clean them opportunistically if regenerated.

## Risks and decisions

- **Ordering is load-bearing.** Archiving before a consumer has bumped off `agon`
  would break that consumer's build. The precondition grep is the gate; do not
  archive until it is empty.
- **DNS teardown is outward-facing.** Removing the record is user-visible. Confirm
  no external doc, bookmark, or link that matters still points at `agon.latere.ai`
  before removing; a redirect to the agents capability page may be kinder than a
  dead name, decided at teardown time.

## Outcome

To be written when this spec is implemented. On completion, update the
[overview](013-overview.md) Outcome to mark the program done and record the final
verification run.
