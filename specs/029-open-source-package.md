---
title: Open Source Package Presentation
status: proposed
track: runtime
depends_on:
  - specs/003-embeddable-sdk.md
affects:
  - README.md
  - CONTRIBUTING.md
  - SECURITY.md
  - .github/ISSUE_TEMPLATE/bug_report.yml
  - .github/ISSUE_TEMPLATE/feature_request.yml
effort: small
created: 2026-08-04
updated: 2026-08-04
author: changkun
dispatched_task_id: null
---

# Open Source Package Presentation

## Problem

The module is public, licensed, documented, and tested, but its repository does
not yet present the standard paths a Go package consumer or contributor expects.
The README has no build, API-reference, version, or license badges and does not
show the `go get` command. GitHub has no topics or published release, and the
repository has no contribution, security, or structured issue-reporting guides.

## Decision

Treat the repository root as the developer-facing package landing page. Keep the
README code-first, link to canonical Go and GitHub sources of truth, and add only
community files that describe workflows this repository actually supports.

Publish the accumulated changes since `v0.2.3` as `v0.3.0`. This is a minor
pre-1.0 release because that range removes public, unused API fields and options.

## Scope

- Add README badges for CI, Go Reference, latest GitHub release, and license.
- Add an installation command and direct links to examples and API docs.
- Add concise contribution and vulnerability-reporting guidance.
- Add structured bug and feature issue forms.
- Set the GitHub description, homepage, and package-relevant topics.
- Tag the verified main-branch commit and publish a GitHub release.

The runtime API and behavior are unchanged.

## Verification

A repository contract test checks the canonical module path, README entry points,
community files, and issue forms. The normal test, race, vet, lint, and coverage
gates remain authoritative for the Go module. Before release, validate that the
GitHub metadata is visible and that the release tag resolves to the verified main
commit.

## Acceptance criteria

- A new Go user can find the install command, API reference, examples, license,
  project status, and CI state from the first README screen.
- A prospective contributor can find the local quality gate and pull-request
  expectations without reading maintainer-only instructions.
- Security reports are directed to a private channel rather than public issues.
- Bug reports request a Go version, Topos version, reproduction, and logs.
- GitHub search can classify the project by Go, agents, LLMs, and sandboxes.
- `v0.3.0` is an annotated tag and a published GitHub release on the verified
  main-branch commit.

