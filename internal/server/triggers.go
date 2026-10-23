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
	"strings"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/triggers"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// TickInterval is how often serve runs Tick: the minute loop of spec
// 022.
const TickInterval = time.Minute

// triggerResource is an existing trigger as the authorizer reads it: its
// name, its owner, the person who applied it, and the agent it runs.
func triggerResource(t store.Trigger) authz.Resource {
	return authz.NewResource(authorizer.KindTrigger, t.ID, map[string]any{"name": t.Name, "owner": t.Owner, "agent": t.AgentID})
}

// onField is a trigger's event filter as the questions of its create and
// its update carry it, so an authorizer that delivers events from the
// producers it runs records the filter when the trigger is applied:
// product, verbs and the resources it names; nil for a schedule.
func onField(spec v1.TriggerSpec) map[string]any {
	if spec.On == nil {
		return nil
	}
	on := map[string]any{"product": spec.On.Product, "verbs": spec.On.Verbs}
	if len(spec.On.Resources) > 0 {
		on["resources"] = spec.On.Resources
	}
	return on
}

// trigger finds a trigger by id, or by a name among the caller's own,
// and asks action about it. A denied read answers as a missing trigger.
func (c *call) trigger(ref, action string) (store.Trigger, error) {
	t, err := store.FindTrigger(c.r.Context(), c.s.o.Objects, c.caller.Subject, ref)
	if err != nil {
		return store.Trigger{}, err
	}
	if _, err := c.ask(c.r.Context(), action, triggerResource(t)); err != nil {
		return store.Trigger{}, err
	}
	return t, nil
}

// renderTrigger is a stored trigger as the API answers it: the document
// last applied, with its firing record in its status.
func renderTrigger(t store.Trigger) (*v1.Trigger, error) {
	doc, err := store.DecodeTrigger(t.Doc)
	if err != nil {
		return nil, err
	}
	counts := t.Counts
	doc.Status.LastFiredAt, doc.Status.LastSessionID, doc.Status.NextFireAt, doc.Status.Counts = t.LastFiredAt, t.LastSessionID, t.NextFireAt, &counts
	return doc, nil
}

// applyTrigger is PUT /triggers/{name}: resolve one Trigger manifest and
// store it under the caller's own name. The caller becomes its owner,
// the initiator of every session it starts, applying in the context its
// token names.
func (c *call) applyTrigger() error {
	body, err := c.body()
	if err != nil {
		return err
	}
	t, created, err := c.s.putTrigger(c.r.Context(), c.asker(), c.r.PathValue("name"), body, triggerApply{})
	if err != nil {
		return err
	}
	out, err := renderTrigger(t)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	return c.reply(status, out)
}

// triggerApply is how a trigger is applied beside its manifest: by a
// session's call (origin), and whether only its questions are asked and
// nothing is stored (check).
type triggerApply struct {
	// origin is the session call that applies the trigger (spec 086), nil
	// for a person's or a program's own apply at the route. Its questions
	// carry it.
	origin *v1.TriggerOrigin
	// made writes origin as the trigger's status.origin: the call made the
	// trigger, as a propose and a wake do. A change, a pause or a resume
	// keeps the origin the trigger already has, whoever made it.
	made bool
	// agent is the agent id the trigger must run, for a session's call
	// whose trigger is of its own agent alone; "" takes the manifest's.
	agent string
	// check asks the questions of the apply and stores nothing.
	check bool
}

// putTrigger resolves one Trigger manifest as q in q's context, asks
// trigger.create or trigger.update about it, and stores it under q's own
// name unless the apply only checks: q becomes its owner, the initiator of
// every session it starts. It answers the trigger as stored and whether
// the apply created it. A trigger a session's call applies carries its
// origin in its questions and keeps it in its status; one that already
// has an origin keeps it through later applies at the route.
func (s *Server) putTrigger(ctx context.Context, q asker, name string, body []byte, how triggerApply) (store.Trigger, bool, error) {
	// The agent a trigger names is read in the caller's context, the
	// trigger's own name among the caller's triggers (spec 036).
	rs, err := manifest.Resolve(ctx, body, manifest.Options{Lookup: scopedLookup{store.LookupIn(s.o.Objects, contextOf(q.caller).subject, q.caller.Subject), s.o.Objects, q.ask}, Now: s.o.Now})
	if err != nil {
		return store.Trigger{}, false, err
	}
	if len(rs) != 1 || rs[0].Trigger == nil {
		return store.Trigger{}, false, refuse(CodeInvalidRequest, "the body holds %d documents; PUT /triggers takes one Trigger", len(rs))
	}
	r := rs[0]
	if r.Name != name {
		return store.Trigger{}, false, refuse(CodeInvalidRequest, "the manifest names the trigger %q and the path %q", r.Name, name)
	}
	spec := r.Trigger.Spec
	if err := checkTriggerSession(spec.Session); err != nil {
		return store.Trigger{}, false, err
	}
	now := s.o.Now().UTC()
	next, err := triggers.NextFire(spec, now)
	if err != nil {
		return store.Trigger{}, false, refuse(CodeInvalidRequest, "spec.timeZone: %v", err)
	}
	st := r.Trigger.Status
	agentID, _, _ := strings.Cut(spec.Agent, "@")
	if how.agent != "" && agentID != how.agent {
		return store.Trigger{}, false, refuse(CodeInvalidRequest, "spec.agent: a session's call applies a trigger of the session's own agent, %s, and this one names %s", how.agent, agentID)
	}
	stored, err := s.o.Objects.TriggerByName(ctx, q.caller.Subject, name)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return store.Trigger{}, false, err
	case exists:
		res := triggerResource(stored)
		if on := onField(spec); on != nil {
			res.Fields["on"] = on
		}
		maps.Copy(res.Fields, deviceFields(spec.Session))
		if how.origin != nil {
			res.Fields["origin"] = originField(*how.origin)
		}
		if _, err := q.ask(ctx, authorizer.ActionTriggerUpdate, res); err != nil {
			return store.Trigger{}, false, err
		}
		if st.ID != stored.ID {
			return store.Trigger{}, false, refuse(CodeConflict, "the trigger %s changed while it was applied; apply again", name)
		}
		prev, err := triggers.Spec(stored)
		if err != nil {
			return store.Trigger{}, false, err
		}
		// A schedule that did not change keeps its next fire time, so a
		// firing due before the apply is not skipped; one that was
		// suspended starts from now, and fires nothing it missed.
		if prev.Schedule == spec.Schedule && prev.TimeZone == spec.TimeZone && !stored.Suspended && stored.NextFireAt != nil {
			next = stored.NextFireAt
		}
		doc, err := store.DecodeTrigger(stored.Doc)
		if err != nil {
			return store.Trigger{}, false, err
		}
		r.Trigger.Status.Origin = doc.Status.Origin
	default:
		fields := map[string]any{"name": name, "agent": agentID}
		if on := onField(spec); on != nil {
			fields["on"] = on
		}
		maps.Copy(fields, deviceFields(spec.Session))
		if how.origin != nil {
			fields["origin"] = originField(*how.origin)
		}
		// The id the resolver minted names the trigger from its first
		// question, so an authorizer that keeps state per trigger, such as
		// the events it delivers to it, can key that state by the id the
		// fire route takes.
		if _, err := q.ask(ctx, authorizer.ActionTriggerCreate, authz.NewResource(authorizer.KindTrigger, st.ID, fields)); err != nil {
			return store.Trigger{}, false, err
		}
	}
	if how.check {
		return store.Trigger{}, !exists, nil
	}
	if how.made {
		r.Trigger.Status.Origin = how.origin
	}
	if spec.Suspend {
		next = nil
	}
	doc, err := session.Marshal(r.Trigger)
	if err != nil {
		return store.Trigger{}, false, err
	}
	t := store.Trigger{ID: st.ID, Name: name, Owner: q.caller.Subject, Claims: auth.TriggerClaims(q.caller), AgentID: agentID, Version: st.Version, Digest: st.Digest,
		Doc: doc, Suspended: spec.Suspend, NextFireAt: next, CreatedAt: st.CreatedAt, UpdatedAt: now}
	if err := s.o.Objects.PutTrigger(ctx, t); err != nil {
		return store.Trigger{}, false, err
	}
	t, err = s.o.Objects.Trigger(ctx, t.ID)
	return t, !exists, err
}

// originField is a trigger's origin as its questions carry it (spec 086):
// the kind, the session and its call, and who confirmed the call once a
// person allowed it.
func originField(o v1.TriggerOrigin) map[string]any {
	f := map[string]any{"kind": o.Kind, "session_id": o.SessionID, "tool_use_id": o.ToolUseID}
	if o.ConfirmedBy != "" {
		f["confirmed_by"] = o.ConfirmedBy
	}
	return f
}

// checkTriggerSession refuses what a session's create cannot take: a
// machine of any kind but device, since a session's machine is its
// agent's and a create names a person's own computer alone (specs 015
// and 080). A memory store the resolver refuses already, as a reference
// the API holds nothing for.
func checkTriggerSession(s v1.TriggerSession) error {
	if s.Machine != nil && s.Machine.Kind != session.MachineDevice {
		return refuse(CodeInvalidRequest, "spec.session.machine: a session's machine is its agent's, and a trigger names a machine of kind device alone")
	}
	return nil
}

// triggerMachine is the device a trigger's sessions work on, nil for the
// agent's machine.
func triggerMachine(s v1.TriggerSession) *session.Machine {
	if s.Machine == nil || s.Machine.Kind != session.MachineDevice {
		return nil
	}
	return &session.Machine{Kind: session.MachineDevice, Environment: s.Machine.Environment, Workdir: s.Machine.Workdir}
}

// deviceFields is the device a trigger names as the questions of its
// create and its update carry it, in session.create's flat form: machine
// device, with environment and workdir beside it, so an authorizer decides
// whether the person who applies the trigger may use that computer when
// it is applied, and not only at each firing's session.create (spec
// 080). It is nil for a trigger of the agent's machine, whose questions
// name none.
func deviceFields(s v1.TriggerSession) map[string]any {
	m := triggerMachine(s)
	if m == nil {
		return nil
	}
	return map[string]any{"machine": m.Kind, "environment": m.Environment, "workdir": m.Workdir}
}

// firesOn is what a trigger fires on, as its sessions record it.
func firesOn(spec v1.TriggerSpec) string {
	if spec.On != nil {
		return session.TriggerEvents
	}
	return session.TriggerSchedule
}

// listTriggers is GET /triggers: the triggers of the owners the allow's
// filter names, every trigger for an allow with none, narrowed to the
// owner the request names (spec 074).
func (c *call) listTriggers() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	named := c.namedOwner()
	d, err := c.ask(c.r.Context(), authorizer.ActionTriggerList, authz.NewResource(authorizer.KindTrigger, "", listFields(named)))
	if err != nil {
		return err
	}
	o := store.TriggerList{Limit: limit, Cursor: cursor}
	switch {
	case named != "" && !admits(d, named):
		return c.replyPage([]*v1.Trigger{}, "")
	case named != "":
		o.Owners = []string{named}
	case d.Filter != nil:
		o.Owners = d.Filter.Owners
	}
	list, next, err := c.s.o.Objects.ListTriggers(c.r.Context(), o)
	if err != nil {
		return err
	}
	items := make([]*v1.Trigger, 0, len(list))
	for _, t := range list {
		doc, err := renderTrigger(t)
		if err != nil {
			return err
		}
		items = append(items, doc)
	}
	return c.replyPage(items, next)
}

// getTrigger is GET /triggers/{ref}.
func (c *call) getTrigger() error {
	t, err := c.trigger(c.r.PathValue("ref"), authorizer.ActionTriggerRead)
	if err != nil {
		return err
	}
	doc, err := renderTrigger(t)
	if err != nil {
		return err
	}
	return c.reply(http.StatusOK, doc)
}

// deleteTrigger is DELETE /triggers/{ref}: the trigger, its firings and
// its keys. The sessions it started keep running.
func (c *call) deleteTrigger() error {
	t, err := c.trigger(c.r.PathValue("ref"), authorizer.ActionTriggerDelete)
	if err != nil {
		return err
	}
	if err := c.s.removeTrigger(c.r.Context(), t, &c.caller); err != nil {
		return err
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

// removeTrigger deletes t with its firings and its keys, for by, the
// caller of the delete route, or nil for a session's call.
func (s *Server) removeTrigger(ctx context.Context, t store.Trigger, by *auth.Caller) error {
	return s.o.Objects.DeleteTrigger(ctx, t.ID)
}

// isWake reports whether a trigger is a session's wake (spec 086), which
// only its session sets and cancels.
func isWake(doc *v1.Trigger) bool {
	return doc.Status.Origin != nil && doc.Status.Origin.Kind == v1.OriginWake
}

// firingEvent is the envelope a firing names.
type firingEvent struct {
	Product  string `json:"product"`
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	ID       string `json:"id"`
}

// firing is a firing as the API answers it (spec 022). A filtered event
// has no id, and an unrecorded firing no outcome.
type firing struct {
	ID         string       `json:"id,omitempty"`
	TriggerID  string       `json:"trigger_id"`
	Origin     string       `json:"origin"`
	Event      *firingEvent `json:"event,omitempty"`
	Key        string       `json:"key"`
	Outcome    string       `json:"outcome,omitempty"`
	Reason     string       `json:"reason,omitempty"`
	SessionID  string       `json:"session_id,omitempty"`
	ReceivedAt time.Time    `json:"received_at"`
}

func renderFiring(f store.Firing) (firing, error) {
	out := firing{ID: f.ID, TriggerID: f.TriggerID, Origin: f.Origin, Key: f.Key, Outcome: f.Outcome, Reason: f.Reason, SessionID: f.SessionID, ReceivedAt: f.ReceivedAt.UTC()}
	if len(f.Envelope) > 0 {
		var e trigger.Envelope
		if err := json.Unmarshal(f.Envelope, &e); err != nil {
			return firing{}, err
		}
		out.Event = &firingEvent{Product: e.Product, Verb: e.Verb, Resource: e.Resource, ID: e.ID}
	}
	return out, nil
}

// fireTrigger is POST /triggers/{ref}/fire: an event trigger takes one
// envelope, and a schedule trigger an empty body, which fires it now. It
// answers the firing, a redelivery's as first recorded, with 503 for a
// failed one, which a redelivery runs again. A session the firing starts
// names the key the caller's token was minted from, when one was (spec
// 078).
func (c *call) fireTrigger() error {
	t, err := c.trigger(c.r.PathValue("ref"), authorizer.ActionTriggerFire)
	if err != nil {
		return err
	}
	spec, err := triggers.Spec(t)
	if err != nil {
		return err
	}
	body, err := c.body()
	if err != nil {
		return err
	}
	var env *trigger.Envelope
	switch empty := len(bytes.TrimSpace(body)) == 0; {
	case spec.On == nil && !empty:
		return refuse(CodeInvalidRequest, "a schedule trigger fires now with an empty body; it takes no event")
	case spec.On != nil && empty:
		return refuse(CodeInvalidRequest, "an event trigger takes one envelope: id, product, verb, resource, time and payload")
	case spec.On != nil:
		env = &trigger.Envelope{}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(env); err != nil {
			return refuse(CodeInvalidRequest, "the envelope does not decode: %v", err)
		}
		if dec.More() {
			return refuse(CodeInvalidRequest, "the body holds more than one envelope")
		}
		if err := env.Check(); err != nil {
			return refuse(CodeInvalidRequest, "%v", err)
		}
	}
	f, err := c.s.triggers.Fire(c.r.Context(), t, env, auth.FiredBy(c.caller))
	if errors.Is(err, triggers.ErrSuspended) {
		return refuse(CodeConflict, "the trigger %s is suspended and fires nothing", t.Name)
	}
	if err != nil {
		return err
	}
	out, err := renderFiring(f)
	if err != nil {
		return err
	}
	if f.Outcome == store.OutcomeFailed {
		return c.reply(http.StatusServiceUnavailable, out)
	}
	return c.reply(http.StatusOK, out)
}

// listFirings is GET /triggers/{ref}/firings: newest first.
func (c *call) listFirings() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	t, err := c.trigger(c.r.PathValue("ref"), authorizer.ActionTriggerRead)
	if err != nil {
		return err
	}
	list, next, err := c.s.o.Objects.Firings(c.r.Context(), t.ID, limit, cursor)
	if err != nil {
		return err
	}
	items := make([]firing, 0, len(list))
	for _, f := range list {
		out, err := renderFiring(f)
		if err != nil {
			return err
		}
		items = append(items, out)
	}
	return c.replyPage(items, next)
}

// Tick is one pass of spec 022's minute loop: the schedules due, and the
// held firings whose session stopped waiting for a person.
func (s *Server) Tick(ctx context.Context) error { return s.triggers.Tick(ctx) }

// actor starts and continues a trigger's sessions through the create and
// send code of the API, asked of the authorizer as the trigger's owner.
type actor struct{ s *Server }

// owner is the trigger's owner as the asker of its firings: the subject
// stored at apply, with the claims the apply kept to forward, so the
// authorizer decides a firing as it would the owner's own request in the
// context it was applied in (spec 022).
func (s *Server) owner(t store.Trigger) asker {
	issuer, sub, ok := authz.SplitSubject(t.Owner)
	if !ok {
		sub = t.Owner
	}
	return s.as(auth.Caller{Subject: t.Owner, Issuer: issuer, Sub: sub, Claims: t.Claims})
}

// sender is who a trigger's messages are from.
func sender(t store.Trigger) session.Sender {
	return session.Sender{Subject: session.TriggerSubjectPrefix + t.ID, Kind: session.SenderTrigger}
}

// Start creates a firing's session as the create route creates one, with
// the rendered message as its first user.message, from the trigger.
func (a actor) Start(ctx context.Context, st triggers.Start) (session.Session, error) {
	if err := checkRepositories(st.Resources); err != nil {
		return session.Session{}, err
	}
	ss := st.Spec.Session
	in := creation{agent: st.Spec.Agent, title: st.Title, message: st.Message, resources: st.Resources, machine: triggerMachine(ss),
		sender: sender(st.Trigger), triggerID: st.Trigger.ID, firingID: st.FiringID, firedBy: st.FiredBy, firesOn: firesOn(st.Spec)}
	if ss.EndOnIdle != nil {
		in.endOnIdle = *ss.EndOnIdle
	}
	if b := ss.Budget; b != nil && b.MaxCost != "" {
		p, err := models.ParsePrice(b.MaxCost)
		if err != nil {
			return session.Session{}, refuse(CodeInvalidRequest, "spec.session.budget.maxCost: %v", err)
		}
		micro := int64(p)
		in.budget = &micro
	}
	if l := ss.Limits; l != nil {
		in.limits = createLimits{TurnTimeout: l.TurnTimeout, MaxAge: l.MaxAge}
	}
	return a.s.create(ctx, a.s.owner(st.Trigger), in)
}

// Send sends a firing's message to the key's open session as the send
// route sends one, asked as session.send with the trigger as sender.
func (a actor) Send(ctx context.Context, sd triggers.Send) error {
	from := sender(sd.Trigger)
	content := []lux.Block{{Type: ir.BlockText, Text: sd.Message}}
	sess, change, err := a.s.sendAs(ctx, a.s.owner(sd.Trigger), sd.SessionID,
		map[string]any{"sender": from.Subject, "event_type": string(session.TypeUserMessage)}, new(shapeOf(content, 0)))
	if err != nil {
		return err
	}
	if sess.Status == session.StatusEnded {
		return triggers.ErrEnded
	}
	msg := session.UserMessage{Sender: from, Content: content, FiringID: sd.FiringID}
	ev, err := session.NewEvent(session.TypeUserMessage, msg, a.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := a.s.appendSent(ctx, sess.ID, change, ev, nil); err != nil {
		if errors.Is(err, errEnded) {
			return triggers.ErrEnded
		}
		return err
	}
	a.s.o.Notify()
	return nil
}

// Refusal names an error of Start or Send as the API would answer it:
// its code, or the authorizer's reason for a deny that carries one, and
// transient when the API would answer it with a 5xx status.
func (actor) Refusal(err error) (string, bool) {
	e := classify(err)
	reason := e.code
	if r, ok := e.details["reason"].(string); ok && r != "" {
		reason = r
	}
	row, ok := codes[e.code]
	return reason, !ok || row.status >= http.StatusInternalServerError
}
