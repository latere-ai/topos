// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// triggerFixture is a server on a fake clock with alice's agent
// reviewer applied, the one its triggers run.
type triggerFixture struct {
	*fixture
	clock *storetest.Clock
}

func newTriggerFixture(t *testing.T, mut ...func(*Options)) *triggerFixture {
	t.Helper()
	clock := storetest.NewClock()
	objects := store.NewMemory(clock.Now)
	f := newFixture(t, append([]func(*Options){func(o *Options) { o.Now, o.Objects = clock.Now, objects }}, mut...)...)
	f.apply("alice", "reviewer", "Review.")
	return &triggerFixture{fixture: f, clock: clock}
}

// triggerYAML is a Trigger manifest of the agent reviewer with the given
// spec fields after the agent.
func triggerYAML(name, spec string) string {
	return "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: " + name + "}\nspec: {agent: reviewer, " + spec + "}\n"
}

// eventSpec is an event trigger on github issues with the given message
// and further fields.
func eventSpec(extra string) string {
	spec := "on: {product: github, verbs: ['issue.*']}, session: {message: 'Triage {{event.resource}}: {{event.payload.title}}'"
	if extra != "" {
		return spec + ", " + extra + "}"
	}
	return spec + "}"
}

func (f *triggerFixture) applyTrigger(token, name, spec string) v1.Trigger {
	f.t.Helper()
	a := f.do(http.MethodPut, "/v1/triggers/"+name, token, triggerYAML(name, spec))
	if a.status != http.StatusCreated && a.status != http.StatusOK {
		f.t.Fatalf("apply trigger %s: %d %s", name, a.status, a.body)
	}
	var out v1.Trigger
	a.decode(f.t, &out)
	return out
}

// envelope is a github issue event on resource, at the fixture's now.
func (f *triggerFixture) envelope(id, verb, resource, title string) string {
	return fmt.Sprintf(`{"id":%q,"product":"github","verb":%q,"resource":%q,"actor":"ann","time":%q,"payload":{"title":%q}}`,
		id, verb, resource, f.clock.Now().Format(time.RFC3339Nano), title)
}

// fire posts body to a trigger's fire route and decodes the firing.
func (f *triggerFixture) fire(token, ref, body string, header ...string) (answer, firing) {
	f.t.Helper()
	a := f.do(http.MethodPost, "/v1/triggers/"+ref+"/fire", token, body, header...)
	var out firing
	if a.status == http.StatusOK || a.status == http.StatusServiceUnavailable {
		a.decode(f.t, &out)
	}
	return a, out
}

// fired fires and wants outcome.
func (f *triggerFixture) fired(token, ref, body, outcome string) firing {
	f.t.Helper()
	a, out := f.fire(token, ref, body)
	if a.status != http.StatusOK || out.Outcome != outcome {
		f.t.Fatalf("fire %s: %d %s, want %s", ref, a.status, a.body, outcome)
	}
	return out
}

func (f *triggerFixture) get(token, ref string) v1.Trigger {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/triggers/"+ref, token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("get trigger %s: %d %s", ref, a.status, a.body)
	}
	var out v1.Trigger
	a.decode(f.t, &out)
	return out
}

func (f *triggerFixture) firings(token, ref string) []firing {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/triggers/"+ref+"/firings", token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("firings of %s: %d %s", ref, a.status, a.body)
	}
	var p struct {
		Items []firing `json:"items"`
	}
	a.decode(f.t, &p)
	return p.Items
}

func (f *triggerFixture) counts(ref string) v1.TriggerCounts {
	f.t.Helper()
	c := f.get("alice", ref).Status.Counts
	if c == nil {
		f.t.Fatal("a trigger answers no counts")
	}
	return *c
}

// sessionsOf lists the sessions a trigger started.
func (f *triggerFixture) sessionsOf(id string) []session.Session {
	f.t.Helper()
	all, _, err := f.sessions.List(f.t.Context(), session.ListOptions{Limit: 500})
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.Session
	for _, s := range all {
		if s.TriggerID == id {
			out = append(out, s)
		}
	}
	return out
}

// messages are a session's user.messages.
func (f *triggerFixture) messages(id string) []session.UserMessage {
	f.t.Helper()
	evs, err := f.sessions.Events(f.t.Context(), id, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.UserMessage
	for _, e := range evs {
		if e.Type != session.TypeUserMessage {
			continue
		}
		var m session.UserMessage
		if err := e.Decode(&m); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// runnerAppends appends events to a session as its runner would.
func (f *triggerFixture) runnerAppends(id string, events ...session.Event) {
	f.t.Helper()
	s, err := f.sessions.Get(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	session.Stamp(id, s.LastSeq, events)
	if _, err := f.sessions.Append(f.t.Context(), id, s.LastSeq, events); err != nil {
		f.t.Fatal(err)
	}
}

func (f *triggerFixture) event(typ session.Type, payload any) session.Event {
	f.t.Helper()
	ev, err := session.NewEvent(typ, payload, f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return ev
}

func (f *triggerFixture) status(st session.Status, reason session.StopReason) session.Event {
	return f.event(session.TypeSessionStatus, session.SessionStatus{Status: st, StopReason: reason})
}

func (f *triggerFixture) tick() {
	f.t.Helper()
	if err := f.api.Tick(f.t.Context()); err != nil {
		f.t.Fatal(err)
	}
}

// TestApplyingATrigger: a trigger applied through the API is stored
// under the caller's name with its owner's context, read back by name
// and id with its firing record, versioned when its spec changes, listed
// among the caller's own, and deleted; a bad template, a machine and a
// zone the build does not know are refused at apply.
func TestApplyingATrigger(t *testing.T) {
	f := newTriggerFixture(t)
	a := f.do(http.MethodPut, "/v1/triggers/nightly", "alice@org_7", triggerYAML("nightly", "schedule: '0 9 * * *', timeZone: Europe/Berlin, session: {message: 'Review {{trigger.name}}.'}"))
	if a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	var tr v1.Trigger
	a.decode(t, &tr)
	stored, err := f.objects.TriggerByName(t.Context(), alice, "nightly")
	if err != nil || stored.OrgID != "org_7" || stored.Owner != alice || stored.ID != tr.Status.ID {
		t.Fatalf("the stored trigger: %+v, %v", stored, err)
	}
	// 2026-09-27 12:00 UTC is 14:00 in Berlin, so 09:00 there is the
	// next morning at 07:00 UTC.
	if next := tr.Status.NextFireAt; next == nil || !next.Equal(time.Date(2026, 9, 28, 7, 0, 0, 0, time.UTC)) || tr.Status.Counts == nil {
		t.Fatalf("the applied trigger's status: %+v", tr.Status)
	}
	if got := f.get("alice", tr.Status.ID); got.Metadata.Name != "nightly" || got.Status.Version != 1 {
		t.Fatalf("by id: %+v", got)
	}
	if again := f.applyTrigger("alice", "nightly", "schedule: '0 9 * * *', timeZone: Europe/Berlin, session: {message: 'Review {{trigger.name}}.'}"); again.Status.Version != 1 {
		t.Fatalf("an unchanged apply made version %d", again.Status.Version)
	}
	changed := f.applyTrigger("alice", "nightly", "schedule: '0 10 * * *', timeZone: Europe/Berlin, session: {message: 'Review {{trigger.name}}.'}")
	if changed.Status.Version != 2 || changed.Status.ID != tr.Status.ID || !changed.Status.NextFireAt.Equal(time.Date(2026, 9, 28, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("a changed schedule: %+v", changed.Status)
	}
	if paused := f.applyTrigger("alice", "nightly", "schedule: '0 10 * * *', timeZone: Europe/Berlin, suspend: true, session: {message: x}"); paused.Status.NextFireAt != nil {
		t.Fatalf("a suspended trigger fires at %s", paused.Status.NextFireAt)
	}
	f.apply("bob", "reviewer", "Bob's review.")
	bobs := f.applyTrigger("bob", "nightly", "schedule: '@daily', session: {message: x}")
	if bobs.Status.ID == tr.Status.ID {
		t.Fatal("bob's trigger of alice's name is alice's")
	}
	var p struct {
		Items []v1.Trigger `json:"items"`
	}
	f.do(http.MethodGet, "/v1/triggers", "alice", "").decode(t, &p)
	if len(p.Items) != 1 || p.Items[0].Status.ID != tr.Status.ID {
		t.Fatalf("alice lists %+v", p.Items)
	}
	if a := f.do(http.MethodGet, "/v1/triggers/nightly", "carol", ""); a.status != http.StatusNotFound {
		t.Fatalf("carol reads alice's trigger by name: %d", a.status)
	}
	if a := f.do(http.MethodGet, "/v1/triggers/"+tr.Status.ID, "bob", ""); a.status != http.StatusNotFound {
		t.Fatalf("bob reads alice's trigger by id: %d", a.status)
	}

	for _, c := range []struct{ name, spec, code, detail string }{
		{"bad", "schedule: '@daily', session: {message: '{{event.resource}}'}", "invalid_manifest", "spec.session.message"},
		{"machine", "schedule: '@daily', session: {message: x, machine: {kind: cella}}", CodeInvalidRequest, "machine"},
		{"zone", "schedule: '@daily', timeZone: Mars/Olympus, session: {message: x}", CodeInvalidRequest, "spec.timeZone"},
		{"store", "schedule: '@daily', session: {message: x, resources: [{type: memoryStore, memoryStore: notes, access: readOnly}]}", "unknown_reference", "memoryStore"},
	} {
		a := f.do(http.MethodPut, "/v1/triggers/"+c.name, "alice", triggerYAML(c.name, c.spec))
		if a.code() != c.code || !strings.Contains(string(a.body), c.detail) {
			t.Errorf("%s: %d %s", c.name, a.status, a.body)
		}
	}
	if a := f.do(http.MethodPut, "/v1/triggers/other", "alice", triggerYAML("nightly", "schedule: '@daily', session: {message: x}")); a.code() != CodeInvalidRequest {
		t.Fatalf("a manifest of another name: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPut, "/v1/triggers/reviewer", "alice", agentYAML("reviewer", "x")); a.code() != CodeInvalidRequest {
		t.Fatalf("an agent to the trigger route: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPut, "/v1/triggers/orphan", "carol", triggerYAML("orphan", "schedule: '@daily', session: {message: x}")); a.code() != "unknown_reference" {
		t.Fatalf("a trigger of an agent carol cannot read: %d %s", a.status, a.body)
	}

	if a := f.do(http.MethodDelete, "/v1/triggers/nightly", "alice", ""); a.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodGet, "/v1/triggers/nightly", "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("a deleted trigger: %d", a.status)
	}
}

// TestFilterMatchesTheEnvelope: an event inside on starts a session; one
// outside it, by product, verb, resource or a payload path, is counted
// as filtered, answered with no id, and writes no firing, and a
// redelivery of it is filtered again.
func TestFilterMatchesTheEnvelope(t *testing.T) {
	f := newTriggerFixture(t)
	f.applyTrigger("alice", "triage", "on: {product: github, verbs: ['issue.*'], resources: ['o/r#*'], match: [{path: payload.action, in: [opened]}]}, "+
		"session: {message: 'Triage {{event.resource}}'}")
	env := func(id, product, verb, resource, action string) string {
		return fmt.Sprintf(`{"id":%q,"product":%q,"verb":%q,"resource":%q,"time":%q,"payload":{"action":%q}}`, id, product, verb, resource, f.clock.Now().Format(time.RFC3339), action)
	}
	for _, body := range []string{
		env("1", "gitlab", "issue.opened", "o/r#1", "opened"),
		env("2", "github", "push", "o/r#1", "opened"),
		env("3", "github", "issue.opened", "x/y#1", "opened"),
		env("4", "github", "issue.opened", "o/r#1", "closed"),
		env("4", "github", "issue.opened", "o/r#1", "closed"),
	} {
		out := f.fired("alice", "triage", body, store.OutcomeFiltered)
		if out.ID != "" || out.Event == nil || out.Event.Product == "" {
			t.Fatalf("a filtered event's answer: %+v", out)
		}
	}
	if list := f.firings("alice", "triage"); len(list) != 0 {
		t.Fatalf("filtered events wrote firings: %+v", list)
	}
	started := f.fired("alice", "triage", env("5", "github", "issue.opened", "o/r#1", "opened"), store.OutcomeStarted)
	if started.ID == "" || started.Event.ID != "5" || started.Key != "o/r#1" || started.SessionID == "" {
		t.Fatalf("a matching event's firing: %+v", started)
	}
	if c := f.counts("triage"); c.Filtered != 5 || c.Started != 1 {
		t.Fatalf("counts %+v", c)
	}
	for _, bad := range []string{
		`{"id":"6","product":"github","verb":"issue.opened","resource":"r","time":"2026-09-27T12:00:00Z","extra":1}`,
		`{"product":"github","verb":"issue.opened","resource":"r","time":"2026-09-27T12:00:00Z"}`,
		`{"id":"6","product":"github","verb":"issue.opened","resource":"r","time":"2026-09-27T12:00:00Z","payload":[1]}`,
		`{"id":"6"} {"id":"7"}`,
		``,
	} {
		if a, _ := f.fire("alice", "triage", bad); a.code() != CodeInvalidRequest {
			t.Errorf("%q: %d %s", bad, a.status, a.body)
		}
	}
}

// TestSkipIfActive: under new with skipIfActive, an event of a key whose
// session is still active starts nothing and is counted as
// skipped_active; another key starts its own session, and once the
// key's session is idle with nothing pending the next event starts one.
func TestSkipIfActive(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "triage", eventSpec(""))
	first := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "Crash"), store.OutcomeStarted)
	skipped := f.fired("alice", "triage", f.envelope("2", "issue.edited", "o/r#1", "Crash, again"), store.OutcomeSkippedActive)
	if skipped.SessionID != "" || skipped.Key != "o/r#1" {
		t.Fatalf("the skipped firing: %+v", skipped)
	}
	other := f.fired("alice", "triage", f.envelope("3", "issue.opened", "o/r#2", "Other"), store.OutcomeStarted)
	if other.SessionID == first.SessionID {
		t.Fatal("two keys share a session")
	}
	// The first session's turn ends; it is idle with nothing pending.
	f.runnerAppends(first.SessionID, f.status(session.StatusRunning, ""), f.status(session.StatusIdle, session.StopEndTurn))
	next := f.fired("alice", "triage", f.envelope("4", "issue.edited", "o/r#1", "Crash, third"), store.OutcomeStarted)
	if next.SessionID == first.SessionID {
		t.Fatal("new continued the key's session")
	}
	if n := len(f.sessionsOf(tr.Status.ID)); n != 3 {
		t.Fatalf("%d sessions", n)
	}
	if c := f.counts("triage"); c.Started != 3 || c.SkippedActive != 1 {
		t.Fatalf("counts %+v", c)
	}
	// Without skipIfActive every firing of a key starts a session.
	f.applyTrigger("alice", "every", eventSpec("")+", skipIfActive: false")
	a := f.fired("alice", "every", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	b := f.fired("alice", "every", f.envelope("2", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	if a.SessionID == b.SessionID {
		t.Fatal("two firings share a session")
	}
}

// TestMaxAgeSkipsLateFirings: an event whose time is more than maxAge
// before now, and each scheduled firing the minute loop reaches more
// than maxAge late, is skipped_late and starts nothing; the loop still
// fires the one on time.
func TestMaxAgeSkipsLateFirings(t *testing.T) {
	f := newTriggerFixture(t)
	f.applyTrigger("alice", "triage", eventSpec("")+", maxAge: 30m")
	stale := fmt.Sprintf(`{"id":"old","product":"github","verb":"issue.opened","resource":"o/r#1","time":%q}`, f.clock.Now().Add(-31*time.Minute).Format(time.RFC3339))
	late := f.fired("alice", "triage", stale, store.OutcomeSkippedLate)
	if late.SessionID != "" {
		t.Fatalf("a late event started %s", late.SessionID)
	}
	f.fired("alice", "triage", f.envelope("fresh", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)

	hourly := f.applyTrigger("alice", "hourly", "schedule: '@hourly', skipIfActive: false, session: {message: 'Hourly at {{firing.time}}'}")
	// The server was away for three hours and five minutes: the firings
	// at 13:00 and 14:00 are more than an hour late, the one at 15:00 is
	// five minutes late.
	f.clock.Advance(3*time.Hour + 5*time.Minute)
	f.tick()
	list := f.firings("alice", "hourly")
	var outcomes []string
	for _, x := range slices.Backward(list) {
		outcomes = append(outcomes, x.Outcome)
	}
	if want := []string{store.OutcomeSkippedLate, store.OutcomeSkippedLate, store.OutcomeStarted}; !slices.Equal(outcomes, want) {
		t.Fatalf("outcomes oldest first %v, want %v", outcomes, want)
	}
	ss := f.sessionsOf(hourly.Status.ID)
	if len(ss) != 1 || f.messages(ss[0].ID)[0].Content[0].Text != "Hourly at 2026-09-27T15:00:00Z" {
		t.Fatalf("the firing on time: %+v", ss)
	}
	if c := f.counts("hourly"); c.SkippedLate != 2 || c.Started != 1 {
		t.Fatalf("counts %+v", c)
	}
	if next := f.get("alice", "hourly").Status.NextFireAt; next == nil || !next.Equal(time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("the next fire time: %v", next)
	}
	// A second pass in the same minute fires nothing again.
	f.tick()
	if n := len(f.firings("alice", "hourly")); n != 3 {
		t.Fatalf("%d firings after a second pass", n)
	}
}

// TestRefusedFiring: a firing whose session.create the authorizer
// refuses starts nothing and is recorded refused with the authorizer's
// reason, and one whose message renders past the cap is refused as
// message_too_large before anything is asked.
func TestRefusedFiring(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "triage", eventSpec(""))
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			return authz.Decision{Reason: "agents_not_enabled"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	out := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeRefused)
	if out.Reason != "agents_not_enabled" || out.SessionID != "" {
		t.Fatalf("the refused firing: %+v", out)
	}
	if n := len(f.sessionsOf(tr.Status.ID)); n != 0 {
		t.Fatalf("a refused firing started %d sessions", n)
	}
	f.authz.answer = nil
	huge := strings.Repeat("x", trigger.MaxValueBytes)
	big := fmt.Sprintf(`{"id":"2","product":"github","verb":"issue.opened","resource":"o/r#2","time":%q,"payload":{"title":%q}}`, f.clock.Now().Format(time.RFC3339), huge)
	f.applyTrigger("alice", "loud", "on: {product: github, verbs: ['*']}, session: {message: '"+strings.Repeat("{{event.payload.title}}", trigger.MaxMessageBytes/trigger.MaxValueBytes+1)+"'}")
	f.authz.take()
	tooBig := f.fired("alice", "loud", big, store.OutcomeRefused)
	if tooBig.Reason != "message_too_large" {
		t.Fatalf("a message past the cap: %+v", tooBig)
	}
	if asked := f.authz.take(); slices.Contains(asked, authorizer.ActionSessionCreate) {
		t.Fatalf("a message past the cap asked %v", asked)
	}
	if c := f.counts("triage"); c.Refused != 1 {
		t.Fatalf("counts %+v", c)
	}
}

// TestContinueSendsToTheKeysSession: under continue, two events of one
// key make one session with two messages from the trigger, each naming
// its firing; after the session ends, the next event of the key starts
// a new one.
func TestContinueSendsToTheKeysSession(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "triage", eventSpec("policy: continue"))
	first := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "Crash"), store.OutcomeStarted)
	second := f.fired("alice", "triage", f.envelope("2", "issue.edited", "o/r#1", "Crash on start"), store.OutcomeContinued)
	if second.SessionID != first.SessionID {
		t.Fatalf("the second event went to %s, not %s", second.SessionID, first.SessionID)
	}
	s, err := f.sessions.Get(t.Context(), first.SessionID)
	if err != nil || s.EndOnIdle || s.TriggerID != tr.Status.ID {
		t.Fatalf("the key's session: %+v, %v", s, err)
	}
	msgs := f.messages(first.SessionID)
	if len(msgs) != 2 || msgs[0].FiringID != first.ID || msgs[1].FiringID != second.ID ||
		msgs[1].Sender.Subject != "trigger:"+tr.Status.ID || msgs[1].Sender.Kind != session.SenderTrigger || msgs[1].Content[0].Text != "Triage o/r#1: Crash on start" {
		t.Fatalf("the session's messages: %+v", msgs)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+first.SessionID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	third := f.fired("alice", "triage", f.envelope("3", "issue.edited", "o/r#1", "Crash, again"), store.OutcomeStarted)
	if third.SessionID == first.SessionID {
		t.Fatal("an ended session took the next event")
	}
	if got := f.get("alice", "triage").Status; got.Counts.Started != 2 || got.Counts.Continued != 1 || got.LastSessionID != third.SessionID || got.LastFiredAt == nil {
		t.Fatalf("status %+v", got)
	}
	// A schedule under continue keeps one session: its key is "".
	sched := f.applyTrigger("alice", "daily", "schedule: '@hourly', session: {message: 'Next hour.', policy: continue}")
	f.clock.Advance(time.Hour)
	f.tick()
	f.clock.Advance(time.Hour)
	f.tick()
	if ss := f.sessionsOf(sched.Status.ID); len(ss) != 1 || len(f.messages(ss[0].ID)) != 2 {
		t.Fatalf("a schedule under continue: %+v", ss)
	}
}

// TestContinueHoldsWhileAPersonIsAsked: a continue firing to a session
// that waits on a confirmation is held and denies nothing; once the
// confirmation is answered and the session runs on, the minute loop
// sends it; a firing held when the session ends starts the key's next
// session instead.
func TestContinueHoldsWhileAPersonIsAsked(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "triage", eventSpec("policy: continue"))
	first := f.fired("alice", "triage", f.envelope("1", "issue.opened", "o/r#1", "Crash"), store.OutcomeStarted)
	ask := func(id, call string) {
		f.runnerAppends(id, f.status(session.StatusRunning, ""),
			f.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: call, Name: "bash", Input: json.RawMessage(`{"command":"rm -rf build"}`), Verdict: "ask"}),
			f.status(session.StatusIdle, session.StopToolConfirmation))
	}
	ask(first.SessionID, "t1")
	held := f.fired("alice", "triage", f.envelope("2", "issue.edited", "o/r#1", "More"), store.OutcomeHeld)
	later := f.fired("alice", "triage", f.envelope("3", "issue.edited", "o/r#1", "Even more"), store.OutcomeHeld)
	f.tick()
	if msgs := f.messages(first.SessionID); len(msgs) != 1 {
		t.Fatalf("a held firing sent a message while the person was asked: %+v", msgs)
	}
	if evs, err := f.sessions.Events(t.Context(), first.SessionID, 1, 0); err != nil || session.Awaiting(evs)["t1"] != session.AnswerConfirmation {
		t.Fatalf("the pending call was answered: %v", err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+first.SessionID+"/events", "alice", `{"type":"user.tool_confirmation","payload":{"tool_use_id":"t1","decision":"allow"}}`); a.status != http.StatusOK {
		t.Fatalf("confirm: %d %s", a.status, a.body)
	}
	f.tick()
	if msgs := f.messages(first.SessionID); len(msgs) != 1 {
		t.Fatal("a held firing was sent before the session left its wait")
	}
	f.runnerAppends(first.SessionID, f.status(session.StatusRunning, ""))
	f.tick()
	msgs := f.messages(first.SessionID)
	if len(msgs) != 3 || msgs[1].FiringID != held.ID || msgs[2].FiringID != later.ID {
		t.Fatalf("the held firings were not sent in order: %+v", msgs)
	}
	for _, x := range f.firings("alice", "triage")[:2] {
		if x.Outcome != store.OutcomeContinued || x.SessionID != first.SessionID {
			t.Fatalf("a sent held firing: %+v", x)
		}
	}
	if c := f.counts("triage"); c.Held != 0 || c.Continued != 2 {
		t.Fatalf("counts %+v", c)
	}

	// Asked again, and ended before the answer: the held firing starts
	// the key's next session.
	f.runnerAppends(first.SessionID,
		f.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "t2", Name: "bash", Input: json.RawMessage(`{}`), Verdict: "ask"}),
		f.status(session.StatusIdle, session.StopToolConfirmation))
	orphan := f.fired("alice", "triage", f.envelope("4", "issue.edited", "o/r#1", "After the end"), store.OutcomeHeld)
	if a := f.do(http.MethodPost, "/v1/sessions/"+first.SessionID+"/end", "alice", `{"reason":"canceled"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	f.tick()
	ss := f.sessionsOf(tr.Status.ID)
	if len(ss) != 2 {
		t.Fatalf("%d sessions", len(ss))
	}
	next := f.firings("alice", "triage")[0]
	if next.ID != orphan.ID || next.Outcome != store.OutcomeStarted || next.SessionID == first.SessionID {
		t.Fatalf("the orphaned held firing: %+v", next)
	}
	// A budget stop holds a firing too.
	f.runnerAppends(next.SessionID, f.status(session.StatusRunning, ""), f.status(session.StatusIdle, session.StopBudget))
	f.fired("alice", "triage", f.envelope("5", "issue.edited", "o/r#1", "Spend"), store.OutcomeHeld)
}

// TestRedeliveryActsOnce: a redelivered event answers its first firing
// and starts nothing; a firing that failed, as an unavailable authorizer
// fails it, answers 503 and runs again when it is redelivered.
func TestRedeliveryActsOnce(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "triage", eventSpec(""))
	body := f.envelope("1", "issue.opened", "o/r#1", "Crash")
	first := f.fired("alice", "triage", body, store.OutcomeStarted)
	again := f.fired("alice", "triage", body, store.OutcomeStarted)
	if again.ID != first.ID || again.SessionID != first.SessionID {
		t.Fatalf("a redelivery answered %+v, not %+v", again, first)
	}
	// One event id per product: another product's delivery of the id is
	// another event.
	f.applyTrigger("alice", "wide", "on: {product: github, verbs: ['*']}, session: {message: x}, skipIfActive: false")
	f.fired("alice", "wide", body, store.OutcomeStarted)
	if n := len(f.sessionsOf(tr.Status.ID)); n != 1 {
		t.Fatalf("a redelivery started a session: %d", n)
	}

	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			return authz.Decision{}, &authz.Unavailable{URL: "http://authz", Err: errors.New("connection refused")}
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	failing := f.envelope("2", "issue.opened", "o/r#2", "Other")
	a, failed := f.fire("alice", "triage", failing)
	if a.status != http.StatusServiceUnavailable || failed.Outcome != store.OutcomeFailed || failed.Reason != auth.CodeAuthorizerUnavailable {
		t.Fatalf("a failed firing: %d %s", a.status, a.body)
	}
	f.authz.answer = nil
	rerun := f.fired("alice", "triage", failing, store.OutcomeStarted)
	if rerun.ID != failed.ID || rerun.SessionID == "" {
		t.Fatalf("the rerun: %+v", rerun)
	}
	if c := f.counts("triage"); c.Failed != 0 || c.Started != 2 {
		t.Fatalf("counts %+v", c)
	}
}

// TestMaxActive: a firing that would start a session while maxActive of
// the trigger's sessions are active is skipped_busy; one that ends frees
// its place; a continued message is not bounded.
func TestMaxActive(t *testing.T) {
	f := newTriggerFixture(t)
	f.applyTrigger("alice", "burst", eventSpec("")+", skipIfActive: false, maxActive: 2")
	one := f.fired("alice", "burst", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	f.fired("alice", "burst", f.envelope("2", "issue.opened", "o/r#2", "x"), store.OutcomeStarted)
	f.fired("alice", "burst", f.envelope("3", "issue.opened", "o/r#3", "x"), store.OutcomeSkippedBusy)
	if a := f.do(http.MethodPost, "/v1/sessions/"+one.SessionID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	f.fired("alice", "burst", f.envelope("4", "issue.opened", "o/r#4", "x"), store.OutcomeStarted)
	if c := f.counts("burst"); c.Started != 3 || c.SkippedBusy != 1 {
		t.Fatalf("counts %+v", c)
	}

	f.applyTrigger("alice", "keyed", eventSpec("policy: continue")+", maxActive: 1")
	k1 := f.fired("alice", "keyed", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	f.fired("alice", "keyed", f.envelope("2", "issue.opened", "o/r#2", "x"), store.OutcomeSkippedBusy)
	if more := f.fired("alice", "keyed", f.envelope("3", "issue.edited", "o/r#1", "x"), store.OutcomeContinued); more.SessionID != k1.SessionID {
		t.Fatalf("the continued message went to %s", more.SessionID)
	}
}

// TestFireIsAskedOfTheAuthorizer: the fire route asks trigger.fire of
// its caller, and a caller refused it starts nothing; the firing's
// session.create is asked as the trigger's owner in the context the
// owner applied it in, with the trigger and the firing named, and its
// session has the owner as initiator and the trigger as sender.
func TestFireIsAskedOfTheAuthorizer(t *testing.T) {
	f := newTriggerFixture(t)
	a := f.do(http.MethodPut, "/v1/triggers/triage", "alice@org_7", triggerYAML("triage", eventSpec("")))
	if a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	var tr v1.Trigger
	a.decode(t, &tr)
	var mu sync.Mutex
	var creates []authz.Request
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			mu.Lock()
			creates = append(creates, req)
			mu.Unlock()
		}
		if req.Action == authorizer.ActionTriggerFire && req.Subject == authz.Subject(issuer, "producer") {
			return authz.Decision{Allow: true}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}

	// bob may not fire alice's trigger, and does not see it.
	if a, _ := f.fire("bob", tr.Status.ID, f.envelope("1", "issue.opened", "o/r#1", "x")); a.status != http.StatusNotFound {
		t.Fatalf("bob fires alice's trigger: %d %s", a.status, a.body)
	}
	if n := len(f.sessionsOf(tr.Status.ID)); n != 0 || len(creates) != 0 {
		t.Fatalf("a refused fire started %d sessions and asked %d creates", n, len(creates))
	}
	// A producer the authorizer grants trigger.fire delivers the event.
	f.authz.take()
	out := f.fired("producer", tr.Status.ID, f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	asked := f.authz.take()
	if len(asked) == 0 || asked[0] != authorizer.ActionTriggerFire || !slices.Contains(asked, authorizer.ActionSessionCreate) {
		t.Fatalf("the fire asked %v", asked)
	}
	if len(creates) != 1 {
		t.Fatalf("%d creates", len(creates))
	}
	q := creates[0]
	claims, _ := json.Marshal(q.Claims)
	if q.Subject != alice || q.Issuer != issuer || q.Sub != "alice" || string(claims) != `{"org_id":"org_7"}` {
		t.Fatalf("the create was asked as %s (%s, %s) with %s", q.Subject, q.Issuer, q.Sub, claims)
	}
	if q.Resource.String("initiator") != alice || q.Resource.String("trigger_id") != tr.Status.ID || q.Resource.String("firing_id") != out.ID {
		t.Fatalf("the create's resource: %+v", q.Resource)
	}
	s, err := f.sessions.Get(t.Context(), out.SessionID)
	if err != nil || s.Initiator.Subject != alice || s.Initiator.Kind != session.SenderPerson || s.TriggerID != tr.Status.ID || !s.EndOnIdle {
		t.Fatalf("the firing's session: %+v, %v", s, err)
	}
	msgs := f.messages(s.ID)
	if len(msgs) != 1 || msgs[0].Sender.Subject != "trigger:"+tr.Status.ID || msgs[0].FiringID != out.ID || msgs[0].Content[0].Text != "Triage o/r#1: x" {
		t.Fatalf("the first message: %+v", msgs)
	}
	// A personal trigger asks with an empty org.
	f.applyTrigger("alice", "mine", eventSpec("")+", skipIfActive: false")
	f.fired("alice", "mine", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	if claims, _ := json.Marshal(creates[1].Claims); string(claims) != `{"org_id":""}` {
		t.Fatalf("a personal trigger's claims: %s", claims)
	}

	// session.send of a continued message is asked the same way.
	var sends []authz.Request
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			sends = append(sends, req)
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	f.applyTrigger("alice", "thread", eventSpec("policy: continue"))
	f.fired("alice", "thread", f.envelope("1", "issue.opened", "o/r#1", "x"), store.OutcomeStarted)
	f.fired("alice", "thread", f.envelope("2", "issue.edited", "o/r#1", "x"), store.OutcomeContinued)
	if len(sends) != 1 || sends[0].Subject != alice || sends[0].Resource.String("sender") != "trigger:"+f.get("alice", "thread").Status.ID {
		t.Fatalf("the continued message's question: %+v", sends)
	}
	// A deny of session.send refuses the continued message.
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Reason: "not_owner"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if denied := f.fired("alice", "thread", f.envelope("3", "issue.edited", "o/r#1", "x"), store.OutcomeRefused); denied.Reason == "" {
		t.Fatalf("a refused send: %+v", denied)
	}
}

// TestFireASchedule: an empty fire of a schedule trigger fires it now,
// once per Idempotency-Key, and an event trigger refuses an empty body; a
// suspended trigger fires nothing and records nothing.
func TestFireASchedule(t *testing.T) {
	f := newTriggerFixture(t)
	tr := f.applyTrigger("alice", "nightly", "schedule: '@daily', skipIfActive: false, session: {message: 'Now: {{firing.time}}'}")
	a, first := f.fire("alice", "nightly", "", "Idempotency-Key", "k1")
	if a.status != http.StatusOK || first.Outcome != store.OutcomeStarted || first.Origin != store.OriginManual || first.ID == "" {
		t.Fatalf("fire now: %d %s", a.status, a.body)
	}
	b, again := f.fire("alice", "nightly", "", "Idempotency-Key", "k1")
	if b.header.Get("Idempotent-Replayed") != "true" || again.ID != first.ID {
		t.Fatalf("a repeat of the key: %s %+v", b.header, again)
	}
	_, other := f.fire("alice", "nightly", "", "Idempotency-Key", "k2")
	if other.ID == first.ID || other.Outcome != store.OutcomeStarted {
		t.Fatalf("another key: %+v", other)
	}
	if n := len(f.sessionsOf(tr.Status.ID)); n != 2 {
		t.Fatalf("%d sessions", n)
	}
	if msgs := f.messages(first.SessionID); msgs[0].Content[0].Text != "Now: 2026-09-27T12:00:00Z" {
		t.Fatalf("the manual firing's time: %+v", msgs)
	}
	if a, _ := f.fire("alice", "nightly", f.envelope("1", "issue.opened", "o/r#1", "x")); a.code() != CodeInvalidRequest {
		t.Fatalf("an event to a schedule: %d %s", a.status, a.body)
	}
	f.applyTrigger("alice", "triage", eventSpec(""))
	if a, _ := f.fire("alice", "triage", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("an empty fire of an event trigger: %d %s", a.status, a.body)
	}
	f.applyTrigger("alice", "nightly", "schedule: '@daily', suspend: true, session: {message: x}")
	if a, _ := f.fire("alice", "nightly", ""); a.code() != CodeConflict {
		t.Fatalf("a suspended trigger fired: %d %s", a.status, a.body)
	}
	f.clock.Advance(48 * time.Hour)
	f.tick()
	if n := len(f.firings("alice", "nightly")); n != 2 {
		t.Fatalf("a suspended schedule recorded %d firings", n)
	}
}

// TestTheMinuteLoopFiresInTheTriggersZone: a daily 02:30 in a zone that
// skips that hour fires at 03:00 there, and the day it repeats, once.
func TestTheMinuteLoopFiresInTheTriggersZone(t *testing.T) {
	f := newTriggerFixture(t)
	// 2026-03-28 12:00 UTC; the clock goes 02:00 to 03:00 in Berlin on
	// the 29th.
	f.clock.Advance(time.Date(2026, 3, 28, 12, 0, 0, 0, time.UTC).Sub(f.clock.Now()))
	tr := f.applyTrigger("alice", "early", "schedule: '30 2 * * *', timeZone: Europe/Berlin, skipIfActive: false, session: {message: 'Early at {{firing.time}}'}")
	if next := tr.Status.NextFireAt; next == nil || !next.Equal(time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("the skipped 02:30 is due at %v, want 03:00 local", next)
	}
	f.clock.Advance(13 * time.Hour)
	f.tick()
	if ss := f.sessionsOf(tr.Status.ID); len(ss) != 1 || f.messages(ss[0].ID)[0].Content[0].Text != "Early at 2026-03-29T01:00:00Z" {
		t.Fatalf("the skipped 02:30: %+v", ss)
	}
}

// TestEveryTriggerRouteRefusesWithoutItsTrigger: a route of a trigger no
// one holds answers not_found, and the list of firings pages newest
// first.
func TestEveryTriggerRouteRefusesWithoutItsTrigger(t *testing.T) {
	f := newTriggerFixture(t)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/triggers/none"},
		{http.MethodDelete, "/v1/triggers/none"},
		{http.MethodPost, "/v1/triggers/none/fire"},
		{http.MethodGet, "/v1/triggers/none/firings"},
		{http.MethodGet, "/v1/triggers/" + session.NewID(session.PrefixTrigger)},
	} {
		if a := f.do(c.method, c.path, "alice", ""); a.status != http.StatusNotFound {
			t.Errorf("%s %s: %d", c.method, c.path, a.status)
		}
	}
	f.applyTrigger("alice", "nightly", "schedule: '@daily', skipIfActive: false, session: {message: x}")
	var ids []string
	for range 3 {
		ids = append(ids, f.fired("alice", "nightly", "", store.OutcomeStarted).ID)
	}
	a := f.do(http.MethodGet, "/v1/triggers/nightly/firings?limit=2", "alice", "")
	var p struct {
		Items      []firing `json:"items"`
		NextCursor string   `json:"next_cursor"`
	}
	a.decode(t, &p)
	if len(p.Items) != 2 || p.Items[0].ID != ids[2] || p.NextCursor == "" {
		t.Fatalf("the first page: %s", a.body)
	}
	if a := f.do(http.MethodGet, "/v1/triggers/nightly/firings?limit=x", "alice", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("a bad limit: %d", a.status)
	}
	if a := f.do(http.MethodGet, "/v1/triggers?limit=0", "alice", ""); a.code() != CodeInvalidRequest {
		t.Fatalf("a bad limit on the list: %d", a.status)
	}
}

// TestTheTriggerQuestionsNameTheTrigger: every trigger.* question names
// the trigger by its name, its owner and its agent; a create names no
// owner yet; a create and an update carry the filter being applied, and
// a schedule's carry none.
func TestTheTriggerQuestionsNameTheTrigger(t *testing.T) {
	f := newTriggerFixture(t)
	var mu sync.Mutex
	asked := map[string][]authz.Resource{}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if strings.HasPrefix(req.Action, "trigger.") {
			mu.Lock()
			asked[req.Action] = append(asked[req.Action], req.Resource)
			mu.Unlock()
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	agentID := f.do(http.MethodGet, "/v1/agents/reviewer", "alice", "")
	var ag v1.Agent
	agentID.decode(t, &ag)
	tr := f.applyTrigger("alice", "triage", "on: {product: github, verbs: ['issue.*'], resources: ['o/r#*'], match: [{path: payload.action, in: [opened]}]}, session: {message: x}")
	f.applyTrigger("alice", "triage", "on: {product: github, verbs: [push]}, session: {message: x}")
	f.fired("alice", "triage", `{"id":"1","product":"github","verb":"push","resource":"o/r","time":"2026-09-27T12:00:00Z"}`, store.OutcomeStarted)
	f.get("alice", "triage")
	f.applyTrigger("alice", "nightly", "schedule: '@daily', session: {message: x}")
	on := func(r authz.Resource) string {
		b, err := json.Marshal(r.Fields["on"])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	creates := asked[authorizer.ActionTriggerCreate]
	if len(creates) != 2 || creates[0].String("name") != "triage" || creates[0].String("agent") != ag.Status.ID || creates[0].String("owner") != "" ||
		on(creates[0]) != `{"product":"github","resources":["o/r#*"],"verbs":["issue.*"]}` || creates[1].Fields["on"] != nil {
		t.Fatalf("the creates: %+v", creates)
	}
	updates := asked[authorizer.ActionTriggerUpdate]
	if len(updates) != 1 || updates[0].ID != tr.Status.ID || updates[0].String("owner") != alice || on(updates[0]) != `{"product":"github","verbs":["push"]}` {
		t.Fatalf("the update: %+v", updates)
	}
	for _, action := range []string{authorizer.ActionTriggerFire, authorizer.ActionTriggerRead} {
		for _, r := range asked[action] {
			if r.ID != tr.Status.ID || r.String("name") != "triage" || r.String("owner") != alice || r.String("agent") != ag.Status.ID || r.Fields["on"] != nil {
				t.Fatalf("%s: %+v", action, r)
			}
		}
		if len(asked[action]) == 0 {
			t.Fatalf("%s was not asked", action)
		}
	}
}
