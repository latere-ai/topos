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
// A name may stand for a choice of other routed names, each standing for
// models (spec 061). Beside model an allow may name route, the routed
// name the endpoint resolved the name asked to on the way to the model.
// toposd keeps it on the session's model beside via, in the header and in
// session.model_changed, and an allow that names a model without it
// clears it; a route that moves while the model stays is a change of its
// own at a send. Every question that carries model_via carries the route
// beside it: model_route on a send and a fork, current_model_route on an
// update. A question about a person's message, a send of a user.message
// and a create that carries a first message, also carries its shape and
// never its words: message_chars, the characters of its text; attachments,
// its files and images; links, the web addresses in its text; and
// tools_last_turn, whether the session's last turn called a tool.
//
// A turn may ask for another model in its middle (spec 051). When the
// model of a session that has a via cannot serve, the gateway answering
// upstream_error, provider_unavailable or upstream_timeout, toposd asks
// session.update as the session's initiator, its claims org_id alone, the
// context the session runs in, with model the via, current_model and
// current_model_via the model that failed and that name, failed_model the
// model that failed, and failed_detail, when the gateway sent one, its
// developer detail of the failure, at most 1024 bytes, such as the
// upstream's own status. An endpoint that routes answers it as a switch
// to the routed name, passing over failed_model, and names the model in
// model; the turn continues on it. An allow that names no model or the
// failed one moves nothing, and the turn ends. A model an allow names
// that the runner cannot connect, such as one the session's key does not
// reach even once the gateway has had a moment to apply the endpoint's
// widening of the key, counts as a move that failed: the next question
// names it as failed_model, beside current_model and current_model_via,
// the model the session still stands on and its name, with why it could
// not be connected as failed_detail, and an endpoint that routes passes
// over it as over any failed model. An endpoint that reads the fields
// rolls out before a server that sends them, and one that does not read
// them keeps answering as before.
//
// A request the model's provider rejected asks the same question (spec
// 051). When the gateway answers upstream_rejected at a 4xx, the
// provider's own refusal and not one of the gateway's, toposd asks it
// with failed_reason rejected, failed_model the model the request was
// sent on, which is also current_model, and failed_detail the gateway's
// code and its developer detail, such as "upstream_rejected: upstream
// status 404: ...". The provider may have refused the request for what
// it holds, or may no longer serve the model under that name, as a
// provider withdraws a free variant without notice: which it is, and
// whether another model answers, is the endpoint's to decide. An allow
// that names another model moves the turn as for a model that cannot
// serve, within the same bound of moves; an allow that names none, or a
// deny, ends the turn with model_error and the gateway's sentence. An
// endpoint that reads failed_model but not failed_reason answers the
// question as for a model that cannot serve, and would move a turn on
// any rejection, so an endpoint refuses a failed_reason it does not know,
// and one that reads it rolls out before a server that sends it. The
// gateway's own refusals, a request it finds invalid, a model the key may
// not use, a rate limit on the key or a spent budget, ask nothing.
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
// A session.update also files a session away and names it (spec 054). An
// archive or an unarchive carries archived, true or false, beside
// session_id alone. A change of the title carries title, the trimmed
// title, beside the fields of the rest of the change, and alone when the
// change names nothing else. Neither reaches a model, and an allow's
// limits are not read for either.
//
// A session's metadata is a label the installation files sessions under
// (spec 057), an object of strings toposd gives no meaning. session.create
// and session.fork carry metadata, the object the session will hold, a
// fork's copied from the session it forks, and leave it out when there is
// none, so an endpoint may refuse a session filed under a label the
// caller may not use. A change of it is a session.update that carries
// metadata, the change as the client sent it, a string setting its key
// and null deleting it, and current_metadata, the value each key it names
// holds now, a key the session does not hold left out; alone it is taken
// in every status, an ended session's among them, and beside a change of
// the model, the mode or the title it is decided with them. No event
// records it, and an allow's limits are not read for it. An endpoint that
// decides session.update by the fields it knows rolls out before a server
// that sends metadata, since it would refuse the change otherwise. A list
// filtered by one entry asks session.list as any list does: the filter
// narrows what an allow admits and widens nothing.
//
// A third member, network, is what the session's machine may reach (spec
// 052), read on an allow of session.create and of session.send:
//
//	"network": {"mode": "allowlist", "hosts": ["example.com", "*.example.org"], "ask": true}
//
// mode is open, allowlist or none; hosts are an allowlist's host patterns,
// each an exact name or one leading "*.", never an address, a port or a
// single label, at most MaxNetworkHosts; ask makes a first contact with a
// host outside the network ask the person attending the session, and only
// an allowlist takes hosts or ask. At a create the session's network is
// the answer's, and absent it is the agent's spec.machine egressMode with
// ask false. At a send an answer that names another network than the
// session's replaces it before the next turn, and the running sandbox is
// narrowed or widened to it; absent keeps the session's. Under an
// allowlist the agent's spec.machine.egress, the hosts of the session's
// named secrets and the git hosts of its repositories are always joined,
// so a narrow answer never cuts a session off from its model gateway or
// its repositories; an endpoint that wants no egress answers none. The
// hosts a person allows in a session stay in its network for the rest of
// it whatever a later answer names, and a fork does not carry them. A
// network toposd cannot read is refused as authorizer_unavailable. An
// endpoint may only answer what the installation's sandbox admission
// admits: a sandbox admission narrowed runs narrowed.
//
// A fourth member, instructions, is the initiator's standing instructions
// (spec 053): text of at most MaxInitiatorInstructions bytes of valid
// UTF-8, read on an allow of session.create alone, including the create
// of a fork. The session records it and the model reads it after its
// agent's own instructions, in the prompt's cached prefix, so no later
// answer changes it; a send's answer that carries it is not read. Whose
// text it is, and in which contexts it applies, is the endpoint's to
// decide; toposd never parses it.
//
// Two members attach what a session starts with beyond its request (spec
// 058), read on an allow of session.create alone, a fork's included, and
// fixed for the session's life:
//
//	"repositories": [{"url": "https://git.example.com/r/7f3c.git", "app": {"slug": "tide-tables", "name": "Tide tables", "url": "https://tide-tables.apps.example.com"}}],
//	"context": [{"title": "Project", "text": "Tide tables for the harbor club."}]
//
// repositories are WireRepository values: a repository as a request names
// it, an https url and an optional ref, and app, the app at the
// installation's app host the repository is the source of. They follow
// the request's repositories, or the agent's when the request names none,
// in the answer's order; one whose url those already name is dropped,
// and the request's keeps its place and its ref. The question's
// repositories names what the session holds before the answer, so an
// endpoint has session.MaxRepositories less that many to attach. A
// repository with app is checked out into the directory of its slug, on
// the session's branch at the commit the app serves, and the publish
// tool publishes to it; a request never names an app. context is titled
// text, each title one line of at most session.MaxContextTitle
// characters, at most session.MaxContext bytes together, which the model
// reads after the initiator's instructions. A fork carries what its
// parent's request named and what its own answer attaches, never what its
// parent's answer attached. An answer past a bound, an app or a part that
// breaks its rule, or a url or an app named twice is refused as
// authorizer_unavailable, its detail naming the member. Whether the
// session may push to an attached repository is the scope the answer
// carries. An endpoint sends these members only to a server that reads
// them, which a server before them ignores as unknown members; this
// release reads no files member.
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
// session.fork carries, beside a create's fields, owner, parent and seq,
// the session forked, its initiator and the copy's end, 0 for a fork
// before the session's opening message, and root, the root of the fork
// tree the new session joins (spec 056): the forked session's tree's
// root for a fork that is another version of its conversation, and the
// new session's own id, session_id, for a fork that starts a
// conversation of its own. An endpoint that meters conversations rather
// than sessions tells the two apart by whether root is session_id.
//
// A fork may be sent its first message in the same call, the edit of a
// person's message (spec 056). toposd then asks session.fork, as for any
// fork, and after its allow session.send of the new session, as a
// send is asked, with model and model_via the model the fork starts on
// and idle_seconds the whole seconds since the last model request the
// copy holds, absent when it holds none. The new session is not written
// until both are allowed: an endpoint that records the sessions it
// allows has recorded the fork at the allow of session.fork, and answers
// the send from that record. A send denied after that allow writes
// nothing, and toposd reports the refusal as an event of spec 023's
// sink, session.fork of the new session with the deny's code as its
// outcome, which an endpoint that recorded the fork reads to close the
// record. A send's allow that names another model or network changes
// the fork before its message, as on any send.
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
