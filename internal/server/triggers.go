// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	ctx := c.r.Context()
	name := c.r.PathValue("name")
	body, err := c.body()
	if err != nil {
		return err
	}
	// The agent a trigger names is read in the caller's context, the
	// trigger's own name among the caller's triggers (spec 036).
	rs, err := manifest.Resolve(ctx, body, manifest.Options{Lookup: scopedLookup{store.LookupIn(c.s.o.Objects, c.here().subject, c.caller.Subject), c}, Now: c.s.o.Now})
	if err != nil {
		return err
	}
	if len(rs) != 1 || rs[0].Trigger == nil {
		return refuse(CodeInvalidRequest, "the body holds %d documents; PUT /triggers takes one Trigger", len(rs))
	}
	r := rs[0]
	if r.Name != name {
		return refuse(CodeInvalidRequest, "the manifest names the trigger %q and the path %q", r.Name, name)
	}
	spec := r.Trigger.Spec
	if err := checkTriggerSession(spec.Session); err != nil {
		return err
	}
	now := c.s.o.Now().UTC()
	next, err := triggers.NextFire(spec, now)
	if err != nil {
		return refuse(CodeInvalidRequest, "spec.timeZone: %v", err)
	}
	st := r.Trigger.Status
	agentID, _, _ := strings.Cut(spec.Agent, "@")
	stored, err := c.s.o.Objects.TriggerByName(ctx, c.caller.Subject, name)
	exists := err == nil
	switch {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	case exists:
		res := triggerResource(stored)
		if on := onField(spec); on != nil {
			res.Fields["on"] = on
		}
		if _, err := c.ask(ctx, authorizer.ActionTriggerUpdate, res); err != nil {
			return err
		}
		if st.ID != stored.ID {
			return refuse(CodeConflict, "the trigger %s changed while it was applied; apply again", name)
		}
		prev, err := triggers.Spec(stored)
		if err != nil {
			return err
		}
		// A schedule that did not change keeps its next fire time, so a
		// firing due before the apply is not skipped; one that was
		// suspended starts from now, and fires nothing it missed.
		if prev.Schedule == spec.Schedule && prev.TimeZone == spec.TimeZone && !stored.Suspended && stored.NextFireAt != nil {
			next = stored.NextFireAt
		}
	default:
		fields := map[string]any{"name": name, "agent": agentID}
		if on := onField(spec); on != nil {
			fields["on"] = on
		}
		// The id the resolver minted names the trigger from its first
		// question, so an authorizer that keeps state per trigger, such as
		// the events it delivers to it, can key that state by the id the
		// fire route takes.
		if _, err := c.ask(ctx, authorizer.ActionTriggerCreate, authz.NewResource(authorizer.KindTrigger, st.ID, fields)); err != nil {
			return err
		}
	}
	if spec.Suspend {
		next = nil
	}
	doc, err := session.Marshal(r.Trigger)
	if err != nil {
		return err
	}
	t := store.Trigger{ID: st.ID, Name: name, Owner: c.caller.Subject, Claims: auth.TriggerClaims(c.caller), AgentID: agentID, Version: st.Version, Digest: st.Digest,
		Doc: doc, Suspended: spec.Suspend, NextFireAt: next, CreatedAt: st.CreatedAt, UpdatedAt: now}
	if err := c.s.o.Objects.PutTrigger(ctx, t); err != nil {
		return err
	}
	if t, err = c.s.o.Objects.Trigger(ctx, t.ID); err != nil {
		return err
	}
	out, err := renderTrigger(t)
	if err != nil {
		return err
	}
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	return c.reply(status, out)
}

// checkTriggerSession refuses what a session's create cannot take yet: a
// machine (spec 015). A memory store the resolver refuses already, as a
// reference the API holds nothing for.
func checkTriggerSession(s v1.TriggerSession) error {
	if s.Machine != nil {
		return refuse(CodeInvalidRequest, "spec.session.machine: a session's machine is its agent's; a create cannot set it yet")
	}
	return nil
}

// listTriggers is GET /triggers.
func (c *call) listTriggers() error {
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	d, err := c.ask(c.r.Context(), authorizer.ActionTriggerList, authz.NewResource(authorizer.KindTrigger, "", nil))
	if err != nil {
		return err
	}
	o := store.TriggerList{Limit: limit, Cursor: cursor}
	if d.Filter != nil {
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
	if err := c.s.o.Objects.DeleteTrigger(c.r.Context(), t.ID); err != nil {
		return err
	}
	c.w.WriteHeader(http.StatusNoContent)
	return nil
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
// failed one, which a redelivery runs again.
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
	f, err := c.s.triggers.Fire(c.r.Context(), t, env)
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
	q := asker{caller: auth.Caller{Subject: t.Owner, Issuer: issuer, Sub: sub, Claims: t.Claims}}
	q.ask = func(ctx context.Context, action string, res authz.Resource) (authz.Decision, error) {
		return s.o.Guard.Ask(ctx, auth.Envelope(q.caller, action, res, nil))
	}
	q.limits = func(ctx context.Context, action string, res authz.Resource) (authorizer.Limits, error) {
		return s.o.Guard.Limits(ctx, auth.Envelope(q.caller, action, res, nil))
	}
	return q
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
	in := creation{agent: st.Spec.Agent, title: st.Title, message: st.Message, resources: st.Resources,
		sender: sender(st.Trigger), triggerID: st.Trigger.ID, firingID: st.FiringID}
	ss := st.Spec.Session
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
	sess, err := a.s.sessionAs(ctx, a.s.owner(sd.Trigger), sd.SessionID, authorizer.ActionSessionSend,
		map[string]any{"sender": from.Subject, "event_type": string(session.TypeUserMessage)})
	if err != nil {
		return err
	}
	if sess.Status == session.StatusEnded {
		return triggers.ErrEnded
	}
	msg := session.UserMessage{Sender: from, Content: []lux.Block{{Type: ir.BlockText, Text: sd.Message}}, FiringID: sd.FiringID}
	ev, err := session.NewEvent(session.TypeUserMessage, msg, a.s.o.Now())
	if err != nil {
		return err
	}
	if _, err := a.s.append(ctx, sess.ID, ev); err != nil {
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
