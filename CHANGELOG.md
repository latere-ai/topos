# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

- `ModelLux` with no `BaseURL` reaches `https://api.latere.ai/v1/models`,
  Latere's Lux core under the platform origin. The default was
  `https://lux.latere.ai`, the retired hosted gateway, whose host no longer
  resolves. A `BaseURL` set by the caller is unaffected.

## v0.6.0 - 2026-09-23

- **Breaking:** the PreToolUse hook payload names its validated tool input
  `normalized_input`, and the Go field is `PreToolUsePayload.NormalizedInput`.
  A hook that reads `normalised_input`, or a modified payload that sets it,
  must use the new key; a payload stored before this release carries the old
  one.

## v0.5.0 - 2026-09-18

- The identity gate now also reads the frontend for the retired admin flag
  and refuses a second copy of the authorizer envelope, the question and the
  decision that `latere.ai/x/pkg` declares once (ci-gate v0.42.0). Nothing
  changes for a user of Topos.

- The Cella provider's documentation describes the credential that exists.
  Cella issues no bearer of its own: present the short-lived token your issuer
  mints for the audience `sandboxd`. Because that token lives for minutes
  rather than days, a run that outlasts one token needs `TokenFunc`, which is
  asked per request, rather than a bearer bridged once at run start. No code
  changed; the `TokenSource` interface and its three implementations are
  unchanged.

- Agent tool grants now restrict the available tools and their execution,
  including delegated agents. Traces show the concrete tools offered.
- Native critics have no tools by default. Explicit grants enable selected
  tools; persisted empty grants remain empty after a JSON round-trip.
