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
// One member, model, routes a session (spec 038): the allow names the
// model to run in place of the one asked. toposd reads it on an allow of
// session.create, where the session starts on it instead of its
// agent's; of a session.update that names a model, where the session
// changes to it instead of the one asked; and of session.send, where the
// session changes to it before its next turn when it is another than the
// one the session stands on. An allow without it leaves each as it was,
// and no other allow is read for it: a fork starts on the model the
// session it forks stood on. The resource tells an endpoint what it
// routes from: model, the name asked, on a create and an update;
// current_model and current_model_via, the model the session stands on
// and the name it was asked by, on an update; and model, model_via and
// idle_seconds on a send, the model the session stands on, the name it
// was asked by, and the whole seconds since the session's last model
// request ended, absent before its first. toposd keeps both names,
// checks that the installation runs the one named, and decides nothing
// about which model a name stands for or how long a provider keeps a
// prompt cache.
//
// A second member, reasoning, sets the reasoning level the session runs
// at (spec 049), read where model is: absent keeps the level the session
// has, one of manifest/v1's Efforts sets it for the next turn, and ""
// returns the session to its agent's own. A fork starts at the level the
// session it forks stood on. toposd resolves "" to the agent's own before
// it compares, so an allow that answers "" for a session already there
// changes nothing. A session.update whose change names a level carries
// it under both names, effort and reasoning, through every v0.x release,
// so an endpoint that reads either name decides it.
//
// session.send is asked of every event a person sends to a session but
// an interrupt, and its resource carries the event's type as event_type:
// user.message, a message; user.tool_confirmation, the allow or deny of
// a tool call that waits for a person; user.tool_result, the result of a
// tool a client runs; and user.answer, a person's answer to a question
// the agent put to them with its question tool. A user.interrupt is
// asked as session.interrupt. An answer, like a confirmation, continues
// a turn the person already started, so an endpoint that meters messages
// tells them apart by event_type; a message sent in place of an answer
// is a user.message. An endpoint that lists the event types it allows
// adds a new one before a server that sends it is rolled out, and a
// deployment rolls in that order: the endpoint, then toposd and every
// runner, then a client that sets attended at a session's create, after
// which an agent's question waits for an answer. Until a client sets
// attended no session waits on a question and no user.answer is sent.
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
// subject's. A deny that carries limits passes them on beside the reason,
// as the error's details.limits, so a deny for a bound that resets can
// say when it does (resets_at); a reason that is withheld withholds its
// limits too.
//
// An action string never changes and never disappears, a kind stays the
// kind it is, and a limits member keeps its wire name and its meaning.
// Nothing here dials: the package builds values and decodes them.
package authorizer
