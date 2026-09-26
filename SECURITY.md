# Security

Topos runs model-driven tool calls against real machines, with credentials
in its reach. This file is what the project promises about that, what it
does not, and how to tell us when it is wrong.

## Reporting a vulnerability

Report one to security@latere.ai. Do not open a public issue for it. You
will hear back within three business days, and a fix for a high severity
issue ships within thirty days. Credit in the release notes on request.

## Status

The repository restarted on 2026-09-26 and serves only its probes. The
commitments below are the design's (specs 001 and 027); each becomes a row
with the tests that hold it as its spec is built.

## Commitments

- No credential enters a machine or a session's log. The runner resolves a
  credential for each outbound call; a sandbox sees a placeholder at most.
- Authority only narrows: a subagent holds a subset of its parent's tools,
  permissions and budget, and a hook may deny a call and never widen one.
- `toposd` decides nothing about a person: every action is the
  authorizer's answer, and an answer it cannot get is a refusal.
- Nothing stops silently: a turn that ends at a limit the model did not
  choose says which limit.

## What is out of scope

- The security of your identity provider and of the authorizer you run.
- A compromised host running a runner. It holds the credentials of the
  sessions it runs at that moment.
- Text a person pastes into a message. A secret sent to a model has
  reached the model's provider; the client warns on input that looks like
  one, and cannot promise to catch every one.
