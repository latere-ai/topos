// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
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
	Machine   json.RawMessage   `json:"machine,omitempty"`
	Resources json.RawMessage   `json:"resources,omitempty"`
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
	case len(b.Metadata) > session.MaxMetadata:
		return refuse(CodeInvalidRequest, "%d metadata entries, at most %d", len(b.Metadata), session.MaxMetadata)
	}
	resources, err := repositories(b.Resources)
	if err != nil {
		return err
	}
	in := creation{agent: b.Agent, title: b.Title, message: b.Message, metadata: b.Metadata, endOnIdle: b.EndOnIdle, resources: resources,
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
// point, and its log up to that point, which the new session copies.
type forkOrigin struct {
	parent session.Session
	seq    uint64
	events []session.Event
}

// create creates a session of in's agent with q's caller as its
// initiator: the one code the create route, a trigger's firing and a
// fork start a session by, asked of the authorizer as session.create, or
// as session.fork for a fork, which runs the parent's agent version and
// copies its log to the fork point.
func (s *Server) create(ctx context.Context, q asker, in creation) (session.Session, error) {
	if in.fork != nil {
		p := in.fork.parent
		in.agent = p.Agent.ID + "@" + strconv.Itoa(p.Agent.Version)
		in.resources, in.title, in.metadata, in.capture = p.Resources, continuedTitle(p.Title), p.Metadata, &p.Capture
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
	// The session runs its agent's model, checked by the rule a switch of
	// its model is checked by (spec 007).
	if err := s.runnable(ctx, cfg.Model, cfg.Overlay); err != nil {
		return session.Session{}, err
	}
	// The session's id is minted before the question, so the authorizer
	// records the session every later token names, with the agent's
	// identity those tokens carry as their subject (spec 018).
	id := session.NewID(session.PrefixSession)
	fields := map[string]any{
		"agent": a.ID, "agent_version": version, "agent_owner": ownerOf(a).field(),
		"runner": session.RunnerHosted, "machine": m.Kind, "initiator": q.caller.Subject,
		"permissions": permissionsField(r.Agent.Spec.Permissions, agentModels(r)), "session_id": id,
		"repositories": repositoriesField(resources),
	}
	// A trigger's session names the trigger and the firing that start it
	// (spec 022).
	if in.triggerID != "" {
		fields["trigger_id"], fields["firing_id"] = in.triggerID, in.firingID
	}
	// A fork is asked about the session it forks, with that session's
	// owner and the fork point beside the new session's fields.
	action, resourceID := authorizer.ActionSessionCreate, ""
	if f := in.fork; f != nil {
		action, resourceID = authorizer.ActionSessionFork, f.parent.ID
		fields["owner"], fields["parent"], fields["seq"] = f.parent.Initiator.Subject, f.parent.ID, f.seq
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
	if f := in.fork; f != nil {
		return session.Fork(ctx, s.o.Sessions, sess, blobs, f.parent.ID, f.events)
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
// and narrowed to the owners the authorizer's allow names.
func (c *call) listSessions() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	o, none, err := c.sessionScope(session.Status(c.r.URL.Query().Get("status")))
	if err != nil {
		return err
	}
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
	o, none, err := c.sessionScope("")
	if err != nil {
		return err
	}
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
// blobs.
func (c *call) deleteSession() error {
	s, err := c.session(authorizer.ActionSessionDelete, nil)
	if err != nil {
		return err
	}
	if err := c.s.o.Sessions.Delete(c.r.Context(), s.ID); err != nil {
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
	var err error
	for range appendRetries {
		var sess session.Session
		if sess, err = s.o.Sessions.Get(ctx, id); err != nil {
			return session.Event{}, err
		}
		if sess.Status == session.StatusEnded {
			e := refuse(CodeConflict, "the session ended %s", sess.StopReason)
			e.err = errEnded
			return session.Event{}, e
		}
		batch := []session.Event{ev}
		session.Stamp(id, sess.LastSeq, batch)
		if _, err = s.o.Sessions.Append(ctx, id, sess.LastSeq, batch); err == nil {
			return batch[0], nil
		}
		if !errors.Is(err, session.ErrSequenceConflict) {
			return session.Event{}, err
		}
	}
	return session.Event{}, err
}
