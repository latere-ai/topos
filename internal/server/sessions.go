// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
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

// session reads a session and asks action about it; a denied read
// answers as a missing session.
func (c *call) session(action string, fields map[string]any) (session.Session, error) {
	s, err := c.s.o.Sessions.Get(c.r.Context(), c.r.PathValue("id"))
	if err != nil {
		return session.Session{}, err
	}
	if _, err := c.ask(c.r.Context(), action, sessionResource(s, fields)); err != nil {
		return session.Session{}, err
	}
	return s, nil
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

// repositories reads a create's resources: repositories alone, at most
// session.MaxRepositories, each an https URL with no credential in it and
// a ref git reads as a name (spec 019). A memory store is its agent's
// (spec 020).
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
	if len(rs) > session.MaxRepositories {
		return nil, refuse(CodeInvalidRequest, "%d resources, at most %d repositories", len(rs), session.MaxRepositories)
	}
	for i, r := range rs {
		switch {
		case r.Type != runner.ResourceRepository:
			return nil, refuse(CodeInvalidRequest, "resources[%d] is of type %q; a session names repositories, and its memory stores are its agent's", i, r.Type)
		case r.MemoryStoreID != "" || r.Access != "":
			return nil, refuse(CodeInvalidRequest, "resources[%d]: a repository has a url and a ref alone", i)
		}
		if err := session.CheckRepository(r, "https"); err != nil {
			return nil, refuse(CodeInvalidRequest, "resources[%d]: %v", i, err)
		}
	}
	return rs, nil
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
	ctx := c.r.Context()
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
	name, n, pinned := strings.Cut(b.Agent, "@")
	a, err := store.FindAgent(ctx, c.s.o.Objects, c.caller.Subject, name)
	if err != nil {
		return err
	}
	version := a.Latest
	if pinned {
		if version, err = strconv.Atoi(n); err != nil || strconv.Itoa(version) != n {
			return refuse(CodeInvalidRequest, "agent %q names no version", b.Agent)
		}
	}
	v, err := c.s.o.Objects.Version(ctx, a.ID, version)
	if err != nil {
		return err
	}
	r, err := manifest.ReadBundle(v.Bundle)
	if err != nil {
		return err
	}
	cfg, err := r.AgentConfig(nil)
	if err != nil {
		return err
	}
	// A session that names no repositories works in its agent version's,
	// which the manifest checked as the API checks a create's (spec 019).
	if len(resources) == 0 {
		for _, repo := range r.Agent.Spec.Repositories {
			resources = append(resources, session.Resource{Type: runner.ResourceRepository, URL: repo.URL, Ref: repo.Ref})
		}
	}
	// A hosted session runs on a Cella machine, or on the server's own
	// host when the operator turned host sessions on; the manifest's host
	// default is otherwise for a local run. Roots and read paths name
	// paths of the host itself, which a session on a server never reaches.
	kind := cfg.Machine.Kind
	switch {
	case kind == session.MachineHost && !c.s.o.HostSessions:
		return refuse(CodeMachineUnavailable, "agent %s runs on machine kind host, and this server runs sessions on its own host only with TOPOS_HOST_SESSIONS=on", a.Name)
	case kind == session.MachineHost && (len(cfg.Machine.Roots) > 0 || len(cfg.Machine.ReadPaths) > 0):
		return refuse(CodeMachineUnavailable, "agent %s names machine.roots or machine.readPaths, paths of the server's own host, which a session on it never reaches", a.Name)
	case kind != session.MachineCella && kind != session.MachineHost:
		return refuse(CodeMachineUnavailable, "agent %s runs on machine kind %q; a hosted session runs on cella or the host", a.Name, kind)
	}
	// The session's id is minted before the question, so the authorizer
	// records the session every later token names, with the agent's
	// identity those tokens carry as their subject (spec 018).
	id := session.NewID(session.PrefixSession)
	fields := map[string]any{
		"agent": a.ID, "agent_version": version, "agent_owner": a.Owner,
		"runner": session.RunnerHosted, "machine": kind, "initiator": c.caller.Subject,
		"permissions": permissionsField(r.Agent.Spec.Permissions, agentModels(r)), "session_id": id,
		"repositories": repositoriesField(resources),
	}
	if c.s.o.Identities != nil {
		st, err := c.s.agentStatus(ctx, a)
		if err != nil {
			return err
		}
		if st.Identity == "" {
			return refuse(CodeAgentIdentityMissing, "agent %s was applied before this server had an identity provider", a.Name)
		}
		fields["agent_identity"] = st.Identity
		// An organization's agent belongs to the organization its identity
		// was created for, not to whoever applied it. A person's agent
		// keeps the person's subject, which every authorizer reads.
		if st.Owner != nil && st.Owner.Type == identity.OwnerOrganization {
			fields["agent_owner"] = map[string]any{"type": st.Owner.Type, "id": st.Owner.ID}
		}
	}
	limits, err := c.askCreate(ctx, authorizer.ActionSessionCreate, authz.NewResource(authorizer.KindSession, "", fields))
	if err != nil {
		// The deny's reason may be about the agent. The caller applied
		// an agent of its own and hears why; another subject's agent,
		// named by id, the caller hears about only when it may read it.
		if a.Owner == c.caller.Subject {
			return err
		}
		return c.s.o.Guard.Disclose(ctx, err, auth.Envelope(c.caller, authorizer.ActionAgentRead, agentResource(a), c.r))
	}
	if a.ArchivedAt != nil {
		return refuse(CodeConflict, "the agent %s is archived", a.Name)
	}
	ref, blobs, err := runner.AgentRef(r)
	if err != nil {
		return err
	}
	now := c.s.o.Now()
	s := session.New(ref, session.Sender{Subject: c.caller.Subject, Kind: session.SenderPerson}, session.RunnerHosted,
		session.Machine{Kind: kind, Environment: cfg.Machine.Environment, Image: cfg.Machine.Image}, now)
	s.ID = id
	s.Title, s.Metadata, s.EndOnIdle, s.Resources = b.Title, b.Metadata, b.EndOnIdle, resources
	// The session records its approval policy merged from the agent's and
	// the organization's limits, so every runner applies the same one.
	var thresholds *harness.Thresholds
	if t := limits.Thresholds; t != nil {
		thresholds = &harness.Thresholds{FlagAt: t.FlagAt, AskAt: t.AskAt, BlockAt: t.BlockAt}
	}
	policy := cfg.Policy.Merge(limits.AlwaysConfirm, limits.AlwaysAllow, thresholds).Session()
	s.Policy = &policy
	if b.Capture != nil {
		s.Capture = *b.Capture
	}
	var asked *int64
	if b.Budget != nil {
		asked = b.Budget.MaxCostUSDMicro
	}
	s.Budget.MaxCostUSDMicro = lowestCost(asked, cfg.MaxCostUSDMicro, limits.BudgetUSDMicro)
	var req createLimits
	if b.Limits != nil {
		req = *b.Limits
	}
	turn, err := lowest("limits.turn_timeout", req.TurnTimeout, cfg.TurnTimeout, limits.TurnTimeout)
	if err != nil {
		return err
	}
	age, err := lowest("limits.max_age", req.MaxAge, cfg.MaxAge, limits.MaxAge)
	if err != nil {
		return err
	}
	s.Limits = session.Limits{TurnTimeout: turn.String(), MaxAge: age.String()}
	if limits.Retention > 0 {
		s.Limits.Retention = limits.Retention.String()
	}
	s.ExpiresAt = s.CreatedAt.Add(age)
	s.Scope = limits.Scope
	if err := c.s.o.Sessions.Create(ctx, s, blobs); err != nil {
		return err
	}
	if b.Message != "" {
		ev, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: b.Message}}}, now)
		if err != nil {
			return err
		}
		if _, err := c.append(s.ID, ev); err != nil {
			return err
		}
		if s, err = c.s.o.Sessions.Get(ctx, s.ID); err != nil {
			return err
		}
		c.s.o.Notify()
	}
	return c.replySession(http.StatusCreated, s)
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

// listSessions is GET /sessions, filtered by agent, status and runner,
// and narrowed to the owners the authorizer's allow names.
func (c *call) listSessions() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	o := session.ListOptions{Status: session.Status(q.Get("status")), Runner: q.Get("runner"), Limit: limit, Cursor: cursor}
	switch o.Status {
	case "", session.StatusIdle, session.StatusRunning, session.StatusEnded:
	default:
		return refuse(CodeInvalidRequest, "status is %q, not idle, running or ended", o.Status)
	}
	if o.Runner != "" && o.Runner != session.RunnerHosted && o.Runner != session.RunnerExternal {
		return refuse(CodeInvalidRequest, "runner is %q, not hosted or external", o.Runner)
	}
	d, err := c.ask(c.r.Context(), authorizer.ActionSessionList, authz.NewResource(authorizer.KindSession, "", map[string]any{"status": string(o.Status), "runner": o.Runner}))
	if err != nil {
		return err
	}
	if d.Filter != nil {
		o.Owners = d.Filter.Owners
	}
	if ref := q.Get("agent"); ref != "" {
		a, err := store.FindAgent(c.r.Context(), c.s.o.Objects, c.caller.Subject, ref)
		if errors.Is(err, store.ErrNotFound) {
			return c.replyPage([]session.Session{}, "")
		}
		if err != nil {
			return err
		}
		o.AgentID = a.ID
	}
	all, next, err := c.s.o.Sessions.List(c.r.Context(), o)
	if err != nil {
		return err
	}
	if all == nil {
		all = []session.Session{}
	}
	return c.replyPage(all, next)
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
func (c *call) endSession() error {
	var b endBody
	if err := c.decode(&b); err != nil {
		return err
	}
	if b.Reason != session.StopCompleted && b.Reason != session.StopCanceled {
		return refuse(CodeInvalidRequest, "reason is %q, not completed or canceled", b.Reason)
	}
	s, err := c.session(authorizer.ActionSessionEnd, nil)
	if err != nil {
		return err
	}
	switch s.Status {
	case session.StatusEnded:
		return refuse(CodeConflict, "the session ended %s", s.StopReason)
	case session.StatusRunning:
		return refuse(CodeConflict, "the session is running; interrupt it first")
	}
	ev, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: b.Reason}, c.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := c.append(s.ID, ev); err != nil {
		return err
	}
	if s, err = c.s.o.Sessions.Get(c.r.Context(), s.ID); err != nil {
		return err
	}
	return c.replySession(http.StatusOK, s)
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

// append appends one event after the session's last, following the log
// when another writer appended first.
func (c *call) append(id string, ev session.Event) (session.Event, error) {
	ctx := c.r.Context()
	var err error
	for range appendRetries {
		var s session.Session
		if s, err = c.s.o.Sessions.Get(ctx, id); err != nil {
			return session.Event{}, err
		}
		if s.Status == session.StatusEnded {
			return session.Event{}, refuse(CodeConflict, "the session ended %s", s.StopReason)
		}
		batch := []session.Event{ev}
		session.Stamp(id, s.LastSeq, batch)
		if _, err = c.s.o.Sessions.Append(ctx, id, s.LastSeq, batch); err == nil {
			return batch[0], nil
		}
		if !errors.Is(err, session.ErrSequenceConflict) {
			return session.Event{}, err
		}
	}
	return session.Event{}, err
}
