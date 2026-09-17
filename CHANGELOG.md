# Changelog

Every tag has a section here, and the section is the body of the GitHub
release. A tag without one is refused at the pre-push and fails the release
workflow. Write under `Unreleased` as work lands; `lateregate release vX.Y.Z`
turns that into the tag's section, commits, tags and pushes.

A section says what changed for whoever uses the release, not what was
committed: the commit log already holds that.

## Unreleased

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
