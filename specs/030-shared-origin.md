---
title: "Serving behind a shared origin: the base path, the public URL, trusted proxies, every URL toposd writes"
status: drafted
track: core
depends_on: [002-scaffold-and-configuration.md, 006-identity.md, 015-api.md]
affects: [internal/server/, internal/serve/, internal/config/, client/, internal/toposcli/]
effort: small
created: 2026-09-27
updated: 2026-09-27
author: changkun
---

# Serving behind a shared origin

## Overview

An installation may serve `toposd` under a path of an origin it shares
with other services, as one capability among several behind one host.
`TOPOS_BASE_PATH` puts the whole API under that path in the place of
`/v1`; `TOPOS_PUBLIC_URL` is the absolute URL clients reach, and every
URL `toposd` writes is built from it; `TOPOS_TRUSTED_PROXIES` says whose
forwarded headers to believe. The `client` package and the `topos`
command compose every path under the base, so nothing a client does
depends on where the server is mounted.

## Current state

v0.7.0 served nothing. The retired hosted service ran on a host of its
own and could not be mounted under a shared origin. The rule here is
the one Lux and Cella adopted for serving under a base path: the base
path stands in the place of the version segment, so an address carries
one version segment.

## Design

### The base path

| `TOPOS_BASE_PATH` | The API root | The agent collection | Memory stores |
|---|---|---|---|
| unset | `/v1` | `/v1/agents` | `/v1/memory-stores` |
| `/v1/agents` | `/v1/agents` | `/v1/agents/agents` | `/v1/agents/memory-stores` |

The base path replaces `/v1` whole: every route of [[015-api]] keeps
its path after the root. Under a capability prefix such as
`/v1/agents`, the agent collection is therefore `/v1/agents/agents`,
and the core accepts that path rather than renaming the collection, so
one route table serves both mounts. A base path starts with `/`, has no
trailing `/`, and must equal the path of `TOPOS_PUBLIC_URL`; a
mismatch is a start-up failure. A request outside the base path is
`not_found`. The probes stay at the listener's root (`/livez`,
`/readyz`, `/version`), and the local issuer's key set is at
`<TOPOS_PUBLIC_URL>/.well-known/jwks.json` ([[006-identity]]).

### URLs toposd writes

Every absolute URL in an answer is `TOPOS_PUBLIC_URL` joined with the
route's path after the root, never taken from the request's `Host`:

| URL | Where |
|---|---|
| the created object | the `Location` header of a 201 |
| the next page | a list answer's `Link` header with `rel="next"`, beside `next_cursor` |
| the stream | a session answer's `Link` header with `rel="stream"` |
| the local issuer's name | the `iss` of tokens `toposd token` signs, and the issuer it accepts |
| the API document | the `servers` entry of the served `openapi.yaml` |

### Trusted proxies

With `TOPOS_TRUSTED_PROXIES` set to CIDR ranges, a request from an
address in one of them has its client address read from the rightmost
`X-Forwarded-For` entry not in the ranges, and its scheme from
`X-Forwarded-Proto`; from any other address both headers are ignored.
The client address is what the authorizer's envelope and the rate
limit see ([[015-api]]). Unset, no forwarded header is believed.

### Clients

`client.New(baseURL)` takes the API root including any base path, for
example `https://topos.example.com/v1` or
`https://api.example.com/v1/agents`, and joins every route's path
after it; `topos` takes the same value in `TOPOS_URL`
([[024-client-cli-skill]]). Neither ever inserts `/v1` itself.

## Not in this spec

The routes ([[015-api]]); the listeners and the variables' table
([[002-scaffold-and-configuration]]); the ingress an installation puts
in front.

## Acceptance criteria

| Criterion | Test that proves it | State |
|---|---|---|
| The conformance suite passes with `TOPOS_BASE_PATH` set to `/v1/agents` | the `base_path` group of [[029-conformance]] | not built |
| With the base path set, the agent collection answers at `/v1/agents/agents`, `/v1/agents/v1/agents` is `not_found`, and the probes answer at the root | `TestBasePathReplacesTheRoot` | not built |
| A base path that differs from the public URL's path is a start-up failure | `TestBasePathMustMatchPublicURL` | not built |
| Every `Location`, `Link`, issuer and `servers` URL starts with `TOPOS_PUBLIC_URL`, whatever `Host` the request carried | `TestWrittenURLsUsePublicURL` | not built |
| Forwarded headers are believed only from a trusted range | `TestTrustedProxies` | not built |
| `client` and `topos` reach every route under a base path without adding `/v1` | `TestClientComposesUnderBasePath` | not built |
