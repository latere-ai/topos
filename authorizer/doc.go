// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package authorizer is the vocabulary an authorization endpoint for
// Topos is written against (spec 006): the actions toposd asks, the
// resource kind each acts on, and the limits an allow of session.create
// may carry. Import it to write the endpoint TOPOS_AUTHORIZER_URL points
// at, instead of keeping a copy of the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's and this package
// declares none of it: toposd posts the caller's subject, its claims
// verbatim, an action and a resource, and reads back an allow or a deny.
// Vocabulary is the whole action table as authz.Vocabulary, which the
// client refuses an unknown action against, latere.ai/x/pkg/authz/server
// validates against, and latere.ai/x/pkg/authz/conformance drives its
// cases from:
//
//	conformance.Run(t, url, token, conformance.WithVocabulary(authorizer.Vocabulary()))
//
// Kind reports the kind an action acts on, Known whether a string is in
// the vocabulary, and Create the action that makes an object of a kind.
//
// An allow of session.create may carry limits. WireLimits is the object
// as the answer renders it, and DecodeLimits is toposd's reading of it,
// here so an endpoint's tests can hold their answers to it.
//
// A decision names a subject as the issuer and the sub joined,
// "https://login.example.com|alice"; the claims are the token's
// verbatim, where an endpoint reads a plan, a team or a role from. The
// initiator cap and the sender rule of spec 006 are the endpoint's
// policy: toposd passes the initiator of session.create and the sender
// of session.send in the resource, and decides nothing about either.
//
// A deny's reason reaches the caller. toposd answers a deny it may
// disclose, a create, a list, or a mutation of an object the caller may
// read, as forbidden with the reason in the error's details.reason, when
// the reason is a snake_case token of at most 64 characters; text of
// another shape is not passed. A session create's reason is passed when
// the agent the create names is the caller's own or one the caller may
// read, since the reason may be about that agent. A denied read, and a
// mutation of an object the caller may not read, answer not_found and
// carry no reason. An endpoint gives a reason a person's client can act
// on, such as agents_not_enabled, and one that names nothing of another
// subject's.
//
// An action string never changes and never disappears, a kind stays the
// kind it is, and a limits member keeps its wire name and its meaning.
// Nothing here dials: the package builds values and decodes them.
package authorizer
