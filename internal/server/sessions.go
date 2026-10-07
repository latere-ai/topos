// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/identity"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// sessionResource is an existing session as the authorizer reads it,
// with the fields of the action beside the ones every action carries.
func sessionResource(s session.Session, fields map[string]any) authz.Resource {
	all := map[string]any{"agent": s.Agent.ID, "owner": s.Initiator.Subject, "runner": s.Runner}
	maps.Copy(all, fields)
	return authz.NewResource(authorizer.KindSession, s.ID, all)
}

// session reads the route's session and asks action about it; a denied
// read answers as a missing session.
func (c *call) session(action string, fields map[string]any) (session.Session, error) {
	return c.s.sessionAs(c.r.Context(), c.asker(), c.r.PathValue("id"), action, fields)
}

// sessionAs reads a session and asks q's caller action about it.
func (s *Server) sessionAs(ctx context.Context, q asker, id, action string, fields map[string]any) (session.Session, error) {
	sess, err := s.o.Sessions.Get(ctx, id)
	if err != nil {
		return session.Session{}, err
	}
	if _, err := q.ask(ctx, action, sessionResource(sess, fields)); err != nil {
		return session.Session{}, err
	}
	return sess, nil
}

// asker asks the authorizer as one caller: a route's, held to the
// actions its row names, or a trigger's owner, whose firing asks
// session.create and session.send (spec 022). r is the request the
// question carries the id, address and agent of, nil for a firing.
type asker struct {
	caller auth.Caller
	r      *http.Request
	ask    func(ctx context.Context, action string, res authz.Resource) (authz.Decision, error)
	limits func(ctx context.Context, action string, res authz.Resource) (authorizer.Limits, error)
}

// asker is the route's caller as an asker.
func (c *call) asker() asker {
	return asker{caller: c.caller, r: c.r, ask: c.ask, limits: c.askLimits}
}

// replySession answers a session with its stream's URL.
func (c *call) replySession(status int, s session.Session) error {
	c.w.Header().Set("Link", "<"+c.url("/sessions/"+s.ID+"/stream", nil)+`>; rel="stream"`)
	return c.reply(status, s)
}

// createBody is the body of POST /sessions.
type createBody struct {
	Agent     string            `json:"agent"`
	Runner    string            `json:"runner,omitempty"`
	ID        string            `json:"id,omitempty"`
	Message   string            `json:"message,omitempty"`
	Title     string            `json:"title,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Budget    *createBudget     `json:"budget,omitempty"`
	Limits    *createLimits     `json:"limits,omitempty"`
	Capture   *session.Capture  `json:"capture,omitempty"`
	EndOnIdle bool              `json:"end_on_idle,omitempty"`
	// Attended declares that a person answers the session's questions,
	// through a client that shows them (spec 039).
	Attended  bool            `json:"attended,omitempty"`
	Machine   json.RawMessage `json:"machine,omitempty"`
	Resources json.RawMessage `json:"resources,omitempty"`
}

// repositories reads a create's resources and checks them.
func repositories(raw json.RawMessage) ([]session.Resource, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rs []session.Resource
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return nil, refuse(CodeInvalidRequest, "resources is a list of {type, url, ref}: %v", err)
	}
	return rs, checkRepositories(rs)
}

// checkRepositories checks a create's resources: repositories alone, at
// most session.MaxRepositories, each an https URL with no credential in
// it and a ref git reads as a name (spec 019). A memory store is its
// agent's (spec 020).
func checkRepositories(rs []session.Resource) error {
	if len(rs) > session.MaxRepositories {
		return refuse(CodeInvalidRequest, "%d resources, at most %d repositories", len(rs), session.MaxRepositories)
	}
	for i, r := range rs {
		switch {
		case r.Type != session.ResourceRepository:
			return refuse(CodeInvalidRequest, "resources[%d] is of type %q; a session names repositories, and its memory stores are its agent's", i, r.Type)
		case r.MemoryStoreID != "" || r.Access != "":
			return refuse(CodeInvalidRequest, "resources[%d]: a repository has a url and a ref alone", i)
		case r.App != nil || r.Attached:
			return refuse(CodeInvalidRequest, "resources[%d]: a request names a repository's url and ref; the app a repository publishes is attached by the authorizer", i)
		}
		if err := session.CheckRepository(r, "https"); err != nil {
			return refuse(CodeInvalidRequest, "resources[%d]: %v", i, err)
		}
	}
	return nil
}

type createBudget struct {
	MaxCostUSDMicro *int64 `json:"max_cost_usd_micro,omitempty"`
}

type createLimits struct {
	TurnTimeout string `json:"turn_timeout,omitempty"`
	MaxAge      string `json:"max_age,omitempty"`
}

// createSession is POST /sessions: a session of an agent's version,
// capped by the agent, the request and the authorizer's limits.
func (c *call) createSession() error {
	var b createBody
	if err := c.decode(&b); err != nil {
		return err
	}
	switch {
	case b.Agent == "":
		return refuse(CodeInvalidRequest, "agent is required")
	case b.Runner != "" && b.Runner != session.RunnerHosted:
		return refuse(CodeInvalidRequest, "runner %q: this server runs hosted sessions; an external runner's sessions are not built", b.Runner)
	case b.ID != "":
		return refuse(CodeInvalidRequest, "id is an external session's, and this server runs hosted sessions")
	case len(b.Machine) > 0:
		return refuse(CodeInvalidRequest, "a session's machine is its agent's; a request cannot set it yet")
	}
	if err := session.CheckMetadata(b.Metadata); err != nil {
		return refuse(CodeInvalidRequest, "%v", err)
	}
	resources, err := repositories(b.Resources)
	if err != nil {
		return err
	}
	in := creation{agent: b.Agent, title: b.Title, message: b.Message, metadata: b.Metadata, endOnIdle: b.EndOnIdle, attended: b.Attended, resources: resources,
		capture: b.Capture, sender: session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}}
	if b.Budget != nil {
		in.budget = b.Budget.MaxCostUSDMicro
	}
	if b.Limits != nil {
		in.limits = *b.Limits
	}
	s, err := c.s.create(c.r.Context(), c.asker(), in)
	if err != nil {
		return err
	}
	return c.replySession(http.StatusCreated, s)
}

// creation is what a session's create takes: from the create route's
// body, or from a trigger's firing, whose first message is the
// trigger's.
type creation struct {
	agent     string
	title     string
	message   string
	metadata  map[string]string
	endOnIdle bool
	// attended is the creator's declaration that a person answers the
	// session's questions; a trigger's firing never makes one.
	attended bool
	// resources are checked repositories; none takes the agent's.
	resources []session.Resource
	// budget is the requested spend ceiling, nil for none.
	budget  *int64
	limits  createLimits
	capture *session.Capture
	// sender is who the first message is from: the initiator, or a
	// trigger.
	sender session.Sender
	// triggerID and firingID name the trigger and the firing that start
	// the session, empty for any other.
	triggerID, firingID string
	// fork is the session and the log a fork continues, nil for any
	// other create (spec 017).
	fork *forkOrigin
}

// forkOrigin is what a fork starts from: the session forked, the fork
// point, its log up to that point, which the new session copies, the
// model it stood on there, nil for its agent's, the fork's title, the
// message it is sent in the same call, nil for none, and whether it
// starts a fork tree of its own (spec 056).
type forkOrigin struct {
	parent  session.Session
	seq     uint64
	events  []session.Event
	model   *session.ModelRef
	title   string
	message *forkMessage
	newTree bool
}

// root is the root of the fork tree a fork of f joins, whose id is id:
// its own for a fork that starts a tree, its parent's tree's otherwise.
func (f *forkOrigin) root(id string) string {
	if f.newTree {
		return id
	}
	return f.parent.TreeRoot()
}

// create creates a session of in's agent with q's caller as its
// initiator: the one code the create route, a trigger's firing and a
// fork start a session by, asked of the authorizer as session.create, or
// as session.fork for a fork, which runs the parent's agent version and
// copies its log to the fork point.
func (s *Server) create(ctx context.Context, q asker, in creation) (session.Session, error) {
	// A fork carries the repositories its parent's request or agent
	// named; what its parent's allow attached it reaches only when its
	// own allow attaches it again (spec 058).
	if in.fork != nil {
		p := in.fork.parent
		in.agent = p.Agent.ID + "@" + strconv.Itoa(p.Agent.Version)
		in.resources, in.title, in.metadata, in.capture = session.Requested(p.Resources), in.fork.title, p.Metadata, &p.Capture
	}
	name, n, pinned := strings.Cut(in.agent, "@")
	a, err := store.FindAgent(ctx, s.o.Objects, contextOf(q.caller).subject, name)
	if err != nil {
		return session.Session{}, err
	}
	version := a.Latest
	if pinned {
		if version, err = strconv.Atoi(n); err != nil || strconv.Itoa(version) != n {
			return session.Session{}, refuse(CodeInvalidRequest, "agent %q names no version", in.agent)
		}
	}
	v, err := s.o.Objects.Version(ctx, a.ID, version)
	if err != nil {
		return session.Session{}, err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return session.Session{}, err
	}
	cfg, err := r.AgentConfig(nil)
	if err != nil {
		return session.Session{}, err
	}
	// A session that names no repositories works in its agent version's,
	// which the manifest checked as the API checks a create's (spec 019).
	resources := in.resources
	if len(resources) == 0 {
		for _, repo := range r.Agent.Spec.Repositories {
			resources = append(resources, session.Resource{Type: session.ResourceRepository, URL: repo.URL, Ref: repo.Ref})
		}
	}
	m, err := s.sessionMachine(a.Name, cfg.Machine)
	if err != nil {
		return session.Session{}, err
	}
	// start is the model the session starts on when it is not its
	// agent's. A fork starts on the one the session it forks stood on at
	// the fork point, which may be one an authorizer named (spec 038); a
	// header that names the agent's own model at its own level names
	// nothing a new session does not run already.
	var start *session.ModelRef
	if f := in.fork; f != nil && f.model != nil && standing(session.Session{Model: f.model}, cfg) != standing(session.Session{}, cfg) {
		start = new(*f.model)
	}
	// The session's id is minted before the question, so the authorizer
	// records the session every later token names, with the agent's
	// identity those tokens carry as their subject (spec 018). The
	// question names the model the session would start on, the agent's
	// name for its model at a create, which the allow may answer with the
	// model to run in its place (spec 038).
	id := session.NewID(session.PrefixSession)
	fields := map[string]any{
		"agent": a.ID, "agent_version": version, "agent_owner": ownerOf(a).field(),
		"runner": session.RunnerHosted, "machine": m.Kind, "initiator": q.caller.Subject,
		"permissions": permissionsField(r.Agent.Spec.Permissions, agentModels(r)), "session_id": id,
		"repositories": repositoriesField(resources), "model": cfg.Model.Name,
	}
	if start != nil {
		fields["model"] = start.Name
		if start.Via != "" {
			fields["model_via"] = start.Via
		}
	}
	// The metadata the session will hold, a fork's copied from the
	// session it forks, so an authorizer decides by a label it files
	// sessions under (spec 057).
	if len(in.metadata) > 0 {
		fields["metadata"] = metadataField(in.metadata)
	}
	// A trigger's session names the trigger and the firing that start it
	// (spec 022).
	if in.triggerID != "" {
		fields["trigger_id"], fields["firing_id"] = in.triggerID, in.firingID
	}
	// A fork is asked about the session it forks, with that session's
	// owner and the fork point beside the new session's fields, and the
	// root of the tree the new session joins, its own id when it starts
	// a conversation of its own (spec 056).
	action, resourceID := authorizer.ActionSessionCreate, ""
	if f := in.fork; f != nil {
		action, resourceID = authorizer.ActionSessionFork, f.parent.ID
		fields["owner"], fields["parent"], fields["seq"], fields["root"] = f.parent.Initiator.Subject, f.parent.ID, f.seq, f.root(id)
	}
	if s.o.Identities != nil {
		st, err := s.agentStatus(ctx, a)
		if err != nil {
			return session.Session{}, err
		}
		if st.Identity == "" {
			return session.Session{}, refuse(CodeAgentIdentityMissing, "agent %s was applied before this server had an identity provider", a.Name)
		}
		fields["agent_identity"] = st.Identity
		// An agent a person applied in an organization's context before
		// an organization could own one is the person's, while its
		// identity was created for the organization, which its sessions
		// name as they always did.
		if st.Owner != nil && st.Owner.Type == identity.OwnerOrganization {
			fields["agent_owner"] = map[string]any{"type": st.Owner.Type, "id": st.Owner.ID}
		}
	}
	limits, err := q.limits(ctx, action, authz.NewResource(authorizer.KindSession, resourceID, fields))
	if err != nil {
		// The deny's reason may be about the agent. The caller applied
		// an agent of its own and hears why; another subject's agent,
		// named by id, the caller hears about only when it may read it.
		if a.Owner == q.caller.Subject {
			return session.Session{}, err
		}
		return session.Session{}, s.o.Guard.Disclose(ctx, err, auth.Envelope(q.caller, authorizer.ActionAgentRead, agentResource(a), q.r))
	}
	// A session the authorizer routed starts on the model its allow names
	// and keeps the agent's name beside it, which a client reads as the
	// choice that was asked, and at the reasoning level its allow names,
	// "" being the agent's own (spec 049). A fork is not routed: it
	// continues its parent's model at its parent's level, and its first
	// send is asked as any send is.
	if in.fork == nil {
		own := session.ModelRef{Name: cfg.Model.Name, Effort: cfg.Effort}
		routed := own
		if limits.Model != "" && limits.Model != cfg.Model.Name {
			routed.Name, routed.Via = limits.Model, cfg.Model.Name
		}
		if r := limits.Reasoning; r != nil {
			routed.Effort = cmp.Or(*r, cfg.Effort)
		}
		if routed != own {
			start = &routed
		}
	}
	// The model the session starts on is checked by the rule a switch of
	// its model is checked by (spec 007). The check follows the question,
	// since the agent's name may be one only the authorizer resolves.
	started, overlay := cfg.Model, cfg.Overlay
	if start != nil {
		started, overlay = cfg.SessionModel(start.Name)
	}
	if err := s.runnable(ctx, started, overlay); err != nil {
		return session.Session{}, err
	}
	if a.ArchivedAt != nil {
		return session.Session{}, refuse(CodeConflict, "the agent %s is archived", a.Name)
	}
	ref, blobs, err := runner.AgentRef(r)
	if err != nil {
		return session.Session{}, err
	}
	now := s.o.Now()
	sess := session.New(ref, session.Sender{Subject: q.caller.Subject, Kind: session.SenderPerson}, session.RunnerHosted, m, now)
	sess.ID = id
	sess.Title, sess.Metadata, sess.EndOnIdle, sess.Resources, sess.TriggerID = in.title, in.metadata, in.endOnIdle, resources, in.triggerID
	sess.Attended = in.attended
	// The header holds the model the session starts on from its create;
	// a fork's copied model changes name it again as the log is copied.
	sess.Model = start
	// The session records its approval policy merged from the agent's and
	// the organization's limits, so every runner applies the same one.
	var thresholds *harness.Thresholds
	if t := limits.Thresholds; t != nil {
		thresholds = &harness.Thresholds{FlagAt: t.FlagAt, AskAt: t.AskAt, BlockAt: t.BlockAt}
	}
	policy := cfg.Policy.Merge(limits.AlwaysConfirm, limits.AlwaysAllow, thresholds).Session()
	sess.Policy = &policy
	if in.capture != nil {
		sess.Capture = *in.capture
	}
	sess.Budget.MaxCostUSDMicro = lowestCost(in.budget, cfg.MaxCostUSDMicro, limits.BudgetUSDMicro)
	turn, err := lowest("limits.turn_timeout", in.limits.TurnTimeout, cfg.TurnTimeout, limits.TurnTimeout)
	if err != nil {
		return session.Session{}, err
	}
	age, err := lowest("limits.max_age", in.limits.MaxAge, cfg.MaxAge, limits.MaxAge)
	if err != nil {
		return session.Session{}, err
	}
	sess.Limits = session.Limits{TurnTimeout: turn.String(), MaxAge: age.String()}
	if limits.Retention > 0 {
		sess.Limits.Retention = limits.Retention.String()
	}
	sess.ExpiresAt = sess.CreatedAt.Add(age)
	sess.Scope = limits.Scope
	// The session's network is the one its allow names, or its agent's
	// mode with ask false (spec 052); a fork is asked as a create is, and
	// the hosts a person allowed in its parent are not carried. The
	// initiator's instructions are read at the create alone (spec 053).
	sess.Network = createdNetwork(limits.Network, cfg.Machine)
	sess.Instructions = limits.Instructions
	// The allow's repositories follow the request's and the agent's, and
	// its context follows the initiator's instructions, each fixed for
	// the session's life (spec 058). An allow that would give the session
	// more repositories than it holds is one the server cannot apply.
	if sess.Resources, err = session.Attach(resources, limits.Repositories); err != nil {
		return session.Session{}, &apiError{code: auth.CodeAuthorizerUnavailable, detail: "limits.repositories: " + err.Error(), err: err}
	}
	sess.Context = limits.Context
	if f := in.fork; f != nil {
		sess.Root = f.root(sess.ID)
		return s.writeFork(ctx, q, sess, cfg, blobs, f)
	}
	if err := s.o.Sessions.Create(ctx, sess, blobs); err != nil {
		return session.Session{}, err
	}
	if in.message == "" {
		return sess, nil
	}
	msg := session.UserMessage{Sender: in.sender, Content: []lux.Block{{Type: ir.BlockText, Text: in.message}}, FiringID: in.firingID}
	ev, err := session.NewEvent(session.TypeUserMessage, msg, now)
	if err != nil {
		return session.Session{}, err
	}
	if _, err := s.append(ctx, sess.ID, ev); err != nil {
		return session.Session{}, err
	}
	if sess, err = s.o.Sessions.Get(ctx, sess.ID); err != nil {
		return session.Session{}, err
	}
	s.o.Notify()
	return sess, nil
}

// sessionMachine is the machine a hosted session of the agent named
// agent runs on (spec 015), from the agent's spec.machine m. An agent
// whose machine asks for nothing beyond the default runs on a Cella
// sandbox of Cella's default image on a server whose runners have Cella
// and run no session on its own host, since the manifest's host default
// is for a local run; any other agent runs on the machine it names. A
// host machine needs TOPOS_HOST_SESSIONS=on, and its roots and read
// paths name paths of the host itself, which a session on a server never
// reaches.
func (s *Server) sessionMachine(agent string, m v1.Machine) (session.Machine, error) {
	if manifest.DefaultMachine(m) && s.o.Cella && !s.o.HostSessions {
		return session.Machine{Kind: session.MachineCella, Image: manifest.DefaultImage}, nil
	}
	switch {
	case m.Kind == session.MachineHost && !s.o.HostSessions:
		return session.Machine{}, refuse(CodeMachineUnavailable, "agent %s runs on machine kind host, and this server runs sessions on its own host only with TOPOS_HOST_SESSIONS=on; an agent that names no machine runs on Cella where TOPOS_CELLA_URL is set", agent)
	case m.Kind == session.MachineHost && (len(m.Roots) > 0 || len(m.ReadPaths) > 0):
		return session.Machine{}, refuse(CodeMachineUnavailable, "agent %s names machine.roots or machine.readPaths, paths of the server's own host, which a session on it never reaches", agent)
	case m.Kind != session.MachineCella && m.Kind != session.MachineHost:
		return session.Machine{}, refuse(CodeMachineUnavailable, "agent %s runs on machine kind %q; a hosted session runs on cella or the host", agent, m.Kind)
	}
	return session.Machine{Kind: m.Kind, Environment: m.Environment, Image: m.Image}, nil
}

// repositoriesField is the session's repositories as the authorizer reads
// them, each {type, url, ref}: an authorizer that holds the git host's
// registry grants the session read, and write where the initiator may
// write, on exactly these, so a session reaches the repositories it names
// and no other without the agent listing them in its permissions.
func repositoriesField(rs []session.Resource) []any {
	out := make([]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{"type": r.Type, "url": r.URL}
		if r.Ref != "" {
			m["ref"] = r.Ref
		}
		out = append(out, m)
	}
	return out
}

// metadataField is a session's metadata as the authorizer reads it, an
// object of strings.
func metadataField(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// permissionsField is the pinned agent version's permissions as the
// session.create resource carries them, so the authorizer compares them
// with what the initiator may do (the initiator cap, spec 006). An agent
// with none carries an empty list, never an absent field.
func permissionsField(ps []v1.Permission, models []string) []any {
	out := make([]any, 0, len(ps)+len(models))
	for _, p := range ps {
		out = append(out, map[string]any{"action": p.Action, "resource": p.Resource})
	}
	// An agent cannot run without the models it names, so using exactly
	// those is part of its definition and no author has to spell it out.
	for _, m := range models {
		out = append(out, map[string]any{"action": modelUse, "resource": m})
	}
	return out
}

// modelUse is the action at the model gateway an agent's own models need.
const modelUse = "lux:model.use"

// agentModels are the models an agent's sessions call: its own, its
// advisor's, and its subagents', inline and pinned, each once, in order.
func agentModels(r manifest.Resolved) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	var walk func(s v1.AgentSpec, depth int)
	walk = func(s v1.AgentSpec, depth int) {
		add(s.Model.Name)
		if s.Advisor != nil {
			add(s.Advisor.Model.Name)
		}
		// The resolver pins subagents no deeper than a thread may spawn.
		if depth >= harness.MaxDepthLimit {
			return
		}
		for _, sub := range s.Subagents {
			if sub.Spec != nil {
				walk(*sub.Spec, depth+1)
			} else if p := r.Pinned[sub.Agent]; p != nil {
				walk(p.Spec, depth+1)
			}
		}
	}
	if r.Agent != nil {
		walk(r.Agent.Spec, 0)
	}
	return out
}

// lowestCost is the lowest of the budgets that are set, nil when none
// is.
func lowestCost(costs ...*int64) *int64 {
	var out *int64
	for _, c := range costs {
		if c != nil && (out == nil || *c < *out) {
			v := *c
			out = &v
		}
	}
	return out
}

// lowest is the lowest of the request's duration and the two ceilings
// that are set; the agent's is always set.
func lowest(field, requested string, agent, authorizer time.Duration) (time.Duration, error) {
	out := agent
	if authorizer > 0 && authorizer < out {
		out = authorizer
	}
	if requested != "" {
		d, err := time.ParseDuration(requested)
		if err != nil || d <= 0 {
			return 0, refuse(CodeInvalidRequest, "%s is %q, not a positive duration", field, requested)
		}
		out = min(out, d)
	}
	return out, nil
}

// listSessions is GET /sessions, filtered by agent, status, runner and
// whether a session is archived, leaving archived ones out unless asked,
// by fork tree and parent, grouped by tree when asked (spec 056), and
// narrowed to the owners the authorizer's allow names.
func (c *call) listSessions() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	tree, err := treeFilters(c.r.URL.Query())
	if err != nil {
		return err
	}
	entry, err := metadataFilter(c.r.URL.Query())
	if err != nil {
		return err
	}
	o, none, err := c.sessionScope(session.Status(c.r.URL.Query().Get("status")))
	if err != nil {
		return err
	}
	o.Root, o.Parent, o.Group, o.Metadata = tree.Root, tree.Parent, tree.Group, entry
	if none {
		return c.replyPage([]session.Session{}, "")
	}
	o.Limit, o.Cursor = limit, cursor
	all, next, err := c.s.o.Sessions.List(c.r.Context(), o)
	if err != nil {
		return err
	}
	if all == nil {
		all = []session.Session{}
	}
	return c.replyPage(all, next)
}

// getSessionSummary is GET /sessions/summary: how many sessions
// GET /sessions would list for the caller under the same filters, by
// status, and how many agents they belong to (spec 015). It is scoped by
// the list's own scope, so a count reveals nothing the list would not.
func (c *call) getSessionSummary() error {
	entry, err := metadataFilter(c.r.URL.Query())
	if err != nil {
		return err
	}
	o, none, err := c.sessionScope("")
	if err != nil {
		return err
	}
	o.Metadata = entry
	if none {
		return c.reply(http.StatusOK, session.Summary{})
	}
	sum, err := summarize(c.r.Context(), c.s.o.Sessions, o)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, sum)
}

// summarizePage is the page a summary reads a store without counts in.
const summarizePage = MaxLimit

// summarize counts the sessions o keeps: the store's own count where it
// has one, and otherwise its list, read a page at a time.
func summarize(ctx context.Context, st session.Store, o session.ListOptions) (session.Summary, error) {
	if s, ok := st.(session.Summarizer); ok {
		return s.Summarize(ctx, o)
	}
	o.Status, o.Limit, o.Cursor = "", summarizePage, ""
	var sum session.Summary
	agents := map[string]struct{}{}
	for {
		page, next, err := st.List(ctx, o)
		if err != nil {
			return session.Summary{}, err
		}
		for _, s := range page {
			sum.Count(s)
			agents[s.Agent.ID] = struct{}{}
		}
		if next == "" {
			break
		}
		o.Cursor = next
	}
	sum.Agents = len(agents)
	return sum, nil
}

// sessionScope is the caller's view of the sessions, read from the
// request's filters and narrowed by the authorizer's session.list
// decision: what GET /sessions pages through and GET /sessions/summary
// counts. status is the list's status filter, empty for a summary. none
// reports an agent name the caller holds no agent of, whose list is
// empty.
func (c *call) sessionScope(status session.Status) (session.ListOptions, bool, error) {
	q := c.r.URL.Query()
	o := session.ListOptions{Status: status, Runner: q.Get("runner")}
	switch a := q.Get("archived"); a {
	case "", "false":
		o.Archived = session.ArchivedExclude
	case "true":
		o.Archived = session.ArchivedOnly
	case "any":
		o.Archived = session.ArchivedAny
	default:
		return o, false, refuse(CodeInvalidRequest, "archived is %q, not true, false or any", a)
	}
	switch o.Status {
	case "", session.StatusIdle, session.StatusRunning, session.StatusEnded:
	default:
		return o, false, refuse(CodeInvalidRequest, "status is %q, not idle, running or ended", o.Status)
	}
	if o.Runner != "" && o.Runner != session.RunnerHosted && o.Runner != session.RunnerExternal {
		return o, false, refuse(CodeInvalidRequest, "runner is %q, not hosted or external", o.Runner)
	}
	// The scope holds the sessions of the agents the context owns, which
	// the question names, and the authorizer's owners narrow it by
	// initiator within them (spec 036).
	here := c.here()
	d, err := c.ask(c.r.Context(), authorizer.ActionSessionList, authz.NewResource(authorizer.KindSession, "",
		map[string]any{"status": string(o.Status), "runner": o.Runner, "agent_owner": here.field()}))
	if err != nil {
		return o, false, err
	}
	if d.Filter != nil {
		o.Owners = d.Filter.Owners
	}
	if o.Agents, err = c.s.agentsOf(c.r.Context(), here); err != nil {
		return o, false, err
	}
	if ref := q.Get("agent"); ref != "" {
		a, err := store.FindAgent(c.r.Context(), c.s.o.Objects, here.subject, ref)
		if errors.Is(err, store.ErrNotFound) {
			return o, true, nil
		}
		if err != nil {
			return o, false, err
		}
		o.AgentID = a.ID
	}
	return o, len(o.Agents) == 0 || (o.AgentID != "" && !slices.Contains(o.Agents, o.AgentID)), nil
}

// MetadataRule is the create route's sentence on a session's metadata
// (spec 057).
var MetadataRule = fmt.Sprintf("The body's metadata is an object of at most %d strings, each key %s, each value at most %d bytes without a control character; any other is invalid_request. "+
	"The session holds it as given, a fork holds the metadata of the session it forks, and the authorizer's session.create and session.fork carry it as metadata, absent when there is none.",
	session.MaxMetadata, session.MetadataKeyRule, session.MaxMetadataValue)

// AttachRule is the create route's sentence on what the authorizer's
// allow attaches (spec 058).
var AttachRule = fmt.Sprintf("The authorizer's allow of session.create or session.fork may attach repositories, each {url, ref, app}, app {slug, name, url} the app at the installation's app host it is the source of, "+
	"which follow the request's resources marked attached, one whose url the request names left out, at most %d repositories together; "+
	"and context, titled text {title, text} of at most %d bytes together, which the model reads after the initiator's instructions for the session's whole life. "+
	"An app's repository is delivered into the directory of its slug under the working directory, on the session's branch at the commit the app serves. "+
	"A fork carries the repositories its parent's request named, and what its own allow attaches; a request that names a repository's app or marks one attached is invalid_request.",
	session.MaxRepositories, session.MaxContext)

// MetadataFilterRule is the list route's sentence on its filter by a
// metadata entry (spec 057).
var MetadataFilterRule = "metadata.<key>=<value>, one such parameter per request, keeps the sessions whose metadata holds key with exactly value, beside every other filter and within the owners the authorizer's decision names; " +
	"under group=tree a tree stands for itself when one of its sessions holds the entry. A second such parameter, a key that breaks the key rule of a create's metadata, and an empty value are invalid_request naming the parameter."

// metadataPrefix begins the name of the query parameter that filters a
// list by one metadata entry, metadata.<key>=<value> (spec 057).
const metadataPrefix = "metadata."

// metadataFilter reads the list's filter by one metadata entry, nil when
// the query names none. A second filter, a key outside the key rule and
// an empty value are refused, each naming the parameter.
func metadataFilter(q url.Values) (*session.MetadataEntry, error) {
	var out *session.MetadataEntry
	for _, name := range slices.Sorted(maps.Keys(q)) {
		key, ok := strings.CutPrefix(name, metadataPrefix)
		if !ok {
			continue
		}
		values := q[name]
		switch {
		case out != nil || len(values) > 1:
			return nil, refuse(CodeInvalidRequest, "%s: a list filters by one metadata entry at most", name)
		case !session.ValidMetadataKey(key):
			return nil, refuse(CodeInvalidRequest, "%s: the key is not %s", name, session.MetadataKeyRule)
		case values[0] == "":
			return nil, refuse(CodeInvalidRequest, "%s: the value is empty", name)
		}
		out = &session.MetadataEntry{Key: key, Value: values[0]}
	}
	return out, nil
}

// getSession is GET /sessions/{id}.
func (c *call) getSession() error {
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}

// endBody is the body of POST /sessions/{id}/end.
type endBody struct {
	Reason session.StopReason `json:"reason"`
}

// endSession is POST /sessions/{id}/end: an idle session ends completed
// or canceled. A running one is interrupted first, by its sender.
//
// The authorizer may act on an allowed session.end before it answers,
// as a decider that revokes the session's credentials does, so the
// route asks it only of a session it would end: it reads the session,
// which a caller who may not see it hears as not_found, refuses a
// running or ended one, and only then asks session.end. The session is
// read again after the decision, since a runner may have claimed it
// meanwhile.
func (c *call) endSession() error {
	var b endBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.Reason != session.StopCompleted && b.Reason != session.StopCanceled {
		return refuse(CodeInvalidRequest, "reason is %q, not completed or canceled", b.Reason)
	}
	ctx := c.r.Context()
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	if err := endable(s); err != nil {
		return err
	}
	if _, err := c.ask(ctx, authorizer.ActionSessionEnd, sessionResource(s, nil)); err != nil {
		return err
	}
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	if err := endable(s); err != nil {
		return err
	}
	ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: b.Reason}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.s.append(ctx, s.ID, ev); err != nil {
		return err
	}
	if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
}

// endable refuses an end of a session that has ended, or that is
// running and is interrupted first.
func endable(s session.Session) error {
	switch s.Status {
	case session.StatusEnded:
		return refuse(CodeConflict, "the session ended %s", s.StopReason)
	case session.StatusRunning:
		return refuse(CodeConflict, "the session is running; interrupt it first")
	}
	return nil
}

// deleteSession is DELETE /sessions/{id}: the session, its log and its
// blobs (spec 040).
//
// The authorizer may act on an allowed session.delete before it answers,
// as on session.end, so the route asks it only of a session it would
// delete: it reads the session, which a caller who may not see it hears
// as not_found, refuses a running one, and only then asks
// session.delete. The store's lease check still holds after the
// question: a runner that claimed the session meanwhile makes the delete
// a conflict, and the session stays for a retry.
func (c *call) deleteSession() error {
	ctx := c.r.Context()
	s, err := c.session(authorizer.ActionSessionRead, nil)
	if err != nil {
		return err
	}
	if s.Status == session.StatusRunning {
		return refuse(CodeConflict, "the session is running; interrupt it first")
	}
	if _, err := c.ask(ctx, authorizer.ActionSessionDelete, sessionResource(s, nil)); err != nil {
		return err
	}
	if err := c.s.o.Sessions.Delete(ctx, s.ID); err != nil {
		return err
	}
	// The session is gone whatever its leftovers do, so a failed removal
	// is logged for the operator and the delete still succeeds.
	if c.s.o.Deleted != nil {
		if err := c.s.o.Deleted(s.ID); err != nil {
			c.s.o.Log.ErrorContext(c.r.Context(), "remove a deleted session's files", "session", s.ID, "err", err)
		}
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

// appendRetries bounds how often an append follows a session that moved
// on under it.
const appendRetries = 8

// errEnded is what an append to an ended session wraps, so a trigger's
// firing tells it from any other conflict.
var errEnded = errors.New("the session ended")

// append appends one event after the session's last, following the log
// when another writer appended first.
func (s *Server) append(ctx context.Context, id string, ev session.Event) (session.Event, error) {
	batch := []session.Event{ev}
	if err := s.appendBatch(ctx, id, batch); err != nil {
		return session.Event{}, err
	}
	return batch[0], nil
}

// appendBatch appends batch after the session's last event as one
// append, so its events land together or not at all, and stamps it in
// place. It follows the log when another writer appended first.
func (s *Server) appendBatch(ctx context.Context, id string, batch []session.Event) error {
	var err error
	for range appendRetries {
		var sess session.Session
		if sess, err = s.o.Sessions.Get(ctx, id); err != nil {
			return err
		}
		if sess.Status == session.StatusEnded {
			e := refuse(CodeConflict, "the session ended %s", sess.StopReason)
			e.err = errEnded
			return e
		}
		session.Stamp(id, sess.LastSeq, batch)
		if _, err = s.o.Sessions.Append(ctx, id, sess.LastSeq, batch); err == nil {
			return nil
		}
		if !errors.Is(err, session.ErrSequenceConflict) {
			return err
		}
	}
	return err
}

// requestWindow is how many of a log's last events sendAs reads first to
// find the session's last model request, which between turns is among
// the last few; it doubles the window until it finds one or the log
// begins.
const requestWindow = 64

// lastRequest is when the session's last model.request ended, the time
// its event was appended, on whichever thread it ran; ok is false for a
// session that has made none.
func (s *Server) lastRequest(ctx context.Context, sess session.Session) (time.Time, bool, error) {
	for end, window := sess.LastSeq, uint64(requestWindow); end > 0; window *= 2 {
		from := uint64(1)
		if end > window {
			from = end - window + 1
		}
		evs, err := s.o.Sessions.Events(ctx, sess.ID, from, int(end-from+1))
		if err != nil {
			return time.Time{}, false, err
		}
		for _, e := range slices.Backward(evs) {
			if e.Type == session.TypeModelRequest {
				return e.Time, true, nil
			}
		}
		end = from - 1
	}
	return time.Time{}, false, nil
}

// sendAs reads a session and asks q's caller session.send about it: the
// one question the send route and a trigger's firing send by (spec 022).
// Beside fields the question carries the model the session stands on,
// the name it was asked by, and the whole seconds since its last model
// request ended, absent before its first, which is what an authorizer
// that keeps a session on one model while a provider's cache is warm
// decides from (spec 038). change is the switch the allow made, nil when
// the model and the reasoning level it names are the ones the session
// stands on: the model it names, checked by the rule a person's switch is
// checked by, with the name asked, at the level it names, "" being the
// agent's own, or the level the session had when it names none (spec
// 049). A level alone moves the level and keeps the model.
func (s *Server) sendAs(ctx context.Context, q asker, id string, fields map[string]any) (session.Session, sendChanges, error) {
	sess, err := s.o.Sessions.Get(ctx, id)
	if err != nil {
		return session.Session{}, sendChanges{}, err
	}
	cfg, err := s.agentConfig(ctx, sess)
	if err != nil {
		return session.Session{}, sendChanges{}, err
	}
	at, made, err := s.lastRequest(ctx, sess)
	if err != nil {
		return session.Session{}, sendChanges{}, err
	}
	out, err := s.askSend(ctx, q, sess, cfg, fields, at, made)
	if err != nil {
		return session.Session{}, sendChanges{}, err
	}
	return sess, out, nil
}

// askSend asks q's caller session.send about sess, an agent's session of
// configuration cfg, whose last model request ended at at, made false
// before its first, and answers the changes the allow made, as sendAs
// describes. A fork that is sent its message in the same call asks it of
// the fork's header before the fork is written (spec 056).
func (s *Server) askSend(ctx context.Context, q asker, sess session.Session, cfg manifest.AgentConfig, fields map[string]any, at time.Time, made bool) (sendChanges, error) {
	old := standing(sess, cfg)
	all := maps.Clone(fields)
	all["model"] = old.Name
	if old.Via != "" {
		all["model_via"] = old.Via
	}
	if made {
		all["idle_seconds"] = max(int(s.o.Now().Sub(at)/time.Second), 0)
	}
	limits, err := q.limits(ctx, authorizer.ActionSessionSend, sessionResource(sess, all))
	if err != nil {
		return sendChanges{}, err
	}
	// A send's allow may name the session's network, which replaces its
	// base network before the next turn (spec 052); its instructions are
	// not read, since they sit in the prompt's cached prefix (spec 053).
	out := sendChanges{network: sentNetwork(sess, limits.Network)}
	next := old
	if limits.Model != "" && limits.Model != old.Name {
		m, overlay := cfg.SessionModel(limits.Model)
		if err := s.runnable(ctx, m, overlay); err != nil {
			return sendChanges{}, err
		}
		next.Name, next.Via = limits.Model, cmp.Or(old.Via, old.Name)
		if next.Via == next.Name {
			next.Via = ""
		}
	}
	if r := limits.Reasoning; r != nil {
		next.Effort = cmp.Or(*r, cfg.Effort)
	}
	if next == old {
		return out, nil
	}
	by := session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService}
	out.model = &session.ModelChanged{By: by, Old: old, New: next}
	return out, nil
}

// sendChanges are the changes an allow of a send made: the model, nil
// when it names the one the session stands on, and the network, nil when
// it names none or the one the session runs.
type sendChanges struct {
	model   *session.ModelChanged
	network *session.NetworkChanged
}

// events are the changes as the events a send appends before what it
// sent, each at time at: the model's change, then the network's.
func (c sendChanges) events(at time.Time) ([]session.Event, error) {
	var out []session.Event
	if c.model != nil {
		changed, err := session.NewEvent(session.TypeModelChanged, *c.model, at)
		if err != nil {
			return nil, err
		}
		out = append(out, changed)
	}
	if c.network != nil {
		changed, err := session.NewEvent(session.TypeNetworkChanged, *c.network, at)
		if err != nil {
			return nil, err
		}
		out = append(out, changed)
	}
	return out, nil
}

// appendSent appends a sent event, after the model and network changes
// its allow made, as one batch: the turn the event starts runs on the new
// model and network, and a send that is refused changes nothing. check,
// when set, is held to the log the batch follows: the batch is appended
// after the sequence the check read, and when another writer appended
// first the log is read and checked again rather than followed, so an
// event that answers a call is never appended after something else
// answered it.
func (s *Server) appendSent(ctx context.Context, id string, changes sendChanges, ev session.Event, check func([]session.Event) error) (session.Event, error) {
	batch, err := changes.events(ev.Time)
	if err != nil {
		return session.Event{}, err
	}
	batch = append(batch, ev)
	if check == nil {
		err = s.appendBatch(ctx, id, batch)
	} else {
		err = s.appendChecked(ctx, id, batch, check)
	}
	if err != nil {
		return session.Event{}, err
	}
	return batch[len(batch)-1], nil
}

// appendChecked appends batch after the session's last event when check
// passes on the log up to it, as one conditional append: a sequence
// conflict reads the log and checks it again.
func (s *Server) appendChecked(ctx context.Context, id string, batch []session.Event, check func([]session.Event) error) error {
	var err error
	for range appendRetries {
		var sess session.Session
		if sess, err = s.o.Sessions.Get(ctx, id); err != nil {
			return err
		}
		if sess.Status == session.StatusEnded {
			e := refuse(CodeConflict, "the session ended %s", sess.StopReason)
			e.err = errEnded
			return e
		}
		var evs []session.Event
		if evs, err = s.o.Sessions.Events(ctx, id, 1, 0); err != nil {
			return err
		}
		// The check reads the log the append follows: the events through
		// the header's last sequence, whatever was appended since.
		evs = slices.DeleteFunc(evs, func(e session.Event) bool { return e.Seq > sess.LastSeq })
		if err := check(evs); err != nil {
			return err
		}
		session.Stamp(id, sess.LastSeq, batch)
		if _, err = s.o.Sessions.Append(ctx, id, sess.LastSeq, batch); err == nil {
			return nil
		}
		if !errors.Is(err, session.ErrSequenceConflict) {
			return err
		}
	}
	return err
}
