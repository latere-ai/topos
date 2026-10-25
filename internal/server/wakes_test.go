// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/manifest/trigger"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// waking is alice's idle session of reviewer, its turn over, which sets
// its own wake.
type waking struct {
	*proposing
}

func newWaking(t *testing.T) *waking {
	t.Helper()
	w := &waking{proposing: newProposing(t)}
	// The proposal the fixture holds is answered, and the turn is over.
	if a := w.confirm("alice", session.DecisionDeny); a.status != http.StatusOK {
		t.Fatalf("the deny: %d %s", a.status, a.body)
	}
	w.runnerAppends(w.sess.ID, w.status(session.StatusIdle, session.StopEndTurn))
	return w
}

// set sets the session's wake for the call id at at, and records it as
// the runner does after the apply, as wake n in the chain.
func (w *waking) set(id string, at time.Time, n int) runner.ListedTrigger {
	w.t.Helper()
	tr, err := w.api.SessionTriggers(w.sess.ID).SetWake(w.t.Context(), runner.Wake{ToolUseID: id, At: at, Title: "Flight prices", Note: "Look at the {{fare}} again."})
	if err != nil {
		w.t.Fatalf("the wake %s: %v", id, err)
	}
	w.runnerAppends(w.sess.ID, w.event(session.TypeWakeSet, session.WakeSet{TriggerID: tr.ID, At: at.UTC(), Title: "Flight prices", Note: "Look at the {{fare}} again.", ToolUseID: id, Count: n}))
	return tr
}

// header reads the session.
func (w *waking) header() session.Session {
	w.t.Helper()
	s, err := w.sessions.Get(w.t.Context(), w.sess.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return s
}

// events reads the session's log.
func (w *waking) events() []session.Event {
	w.t.Helper()
	evs, err := w.sessions.Events(w.t.Context(), w.sess.ID, 1, 0)
	if err != nil {
		w.t.Fatal(err)
	}
	return evs
}

// cleared is the session's last session.wake_cleared, and false for none.
func (w *waking) cleared() (session.WakeCleared, bool) {
	w.t.Helper()
	for _, e := range slices.Backward(w.events()) {
		if e.Type == session.TypeWakeCleared {
			var c session.WakeCleared
			if err := e.Decode(&c); err != nil {
				w.t.Fatal(err)
			}
			return c, true
		}
	}
	return session.WakeCleared{}, false
}

// TestAWakeContinuesItsSession is Topos spec 086's wake criterion: a
// wake is applied as the session's initiator under the session's name,
// with the agent's label, its origin, its time and the note as a literal
// message, and asks trigger.create with at; a second set replaces it, one
// trigger with the new time; at its time it continues the session that
// set it with the note and the wake field, the header's wake clears, and
// the trigger is spent; a wake of a session that has ended is
// skipped_ended and sends nothing.
func TestAWakeContinuesItsSession(t *testing.T) {
	w := newWaking(t)
	now := w.clock.Now()
	w.questions()
	first := w.set("toolu_w1", now.Add(2*time.Hour), 1)
	name := "wake-" + strings.ToLower(strings.TrimPrefix(w.sess.ID, "ses_"))
	stored := w.get("alice", name)
	if first.Name != name || stored.Metadata.Labels["applied-by"] != "reviewer" || stored.Metadata.DisplayName != "Flight prices" ||
		stored.Spec.At == nil || !stored.Spec.At.Equal(now.Add(2*time.Hour)) || stored.Spec.Session.Target != w.sess.ID ||
		stored.Spec.Session.Message != `Look at the \{{fare}} again.` || stored.Spec.MaxAge != trigger.WakeMaxAge.String() ||
		stored.Status.Origin == nil || *stored.Status.Origin != (v1.TriggerOrigin{Kind: v1.OriginWake, SessionID: w.sess.ID, ToolUseID: "toolu_w1"}) {
		t.Fatalf("the wake is %s: %+v %+v", first.Name, stored.Spec, stored.Status.Origin)
	}
	var create authz.Request
	for _, q := range w.questions() {
		if q.Action == authorizer.ActionTriggerCreate {
			create = q
		}
	}
	if create.Subject != alice || create.Resource.String("at") != now.Add(2*time.Hour).UTC().Format(time.RFC3339) || create.Resource.String("name") != name {
		t.Fatalf("the wake asked %+v", create)
	}
	if o, _ := create.Resource.Fields["origin"].(map[string]any); o["kind"] != v1.OriginWake || o["tool_use_id"] != "toolu_w1" || o["confirmed_by"] != nil {
		t.Fatalf("the wake's origin is %+v", o)
	}
	if h := w.header(); h.Wake == nil || h.Wake.TriggerID != first.ID || h.Wake.Count != 1 || h.Wake.Title != "Flight prices" {
		t.Fatalf("the header's wake is %+v", h.Wake)
	}

	// A second set replaces the first: one trigger, the new time.
	second := w.set("toolu_w2", now.Add(time.Hour), 1)
	if second.ID != first.ID || second.NextFireAt == nil || !second.NextFireAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("the replacing wake is %+v", second)
	}
	w.clock.Advance(time.Hour)
	w.tick()
	msgs := w.messages(w.sess.ID)
	last := msgs[len(msgs)-1]
	if last.Wake == nil || last.Wake.TriggerID != first.ID || last.Wake.Count != 1 || !last.Wake.DueAt.Equal(now.Add(time.Hour)) || last.Wake.SetAt.IsZero() ||
		len(last.Content) != 1 || last.Content[0].Text != "Look at the {{fare}} again." || last.Sender.Subject != session.TriggerSubjectPrefix+first.ID || last.FiringID == "" {
		t.Fatalf("the wake's message is %+v", last)
	}
	if h := w.header(); h.Wake != nil {
		t.Fatalf("the header keeps the fired wake %+v", h.Wake)
	}
	if _, err := w.objects.Trigger(t.Context(), first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the fired wake's trigger: %v", err)
	}

	// A wake of a session that ended is skipped_ended and sends nothing.
	w.runnerAppends(w.sess.ID, w.status(session.StatusIdle, session.StopEndTurn))
	third := w.set("toolu_w3", w.clock.Now().Add(time.Hour), 2)
	w.runnerAppends(w.sess.ID, w.status(session.StatusEnded, session.StopCompleted))
	n := len(w.messages(w.sess.ID))
	w.clock.Advance(time.Hour)
	w.tick()
	if len(w.messages(w.sess.ID)) != n {
		t.Fatal("a wake of an ended session sent its message")
	}
	if _, err := w.objects.Trigger(t.Context(), third.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the skipped wake's trigger: %v", err)
	}
}

// TestAWakeIsHeldToItsBoundsAtTheServer: whatever applies it, a wake
// sooner than MinWakeDelay, later than MaxWakeAhead or past its session's
// end is refused with the bound named as the reason, and a wake past its
// maxAge after a stop of the server is skipped_late and recorded as
// skipped in its session.
func TestAWakeIsHeldToItsBoundsAtTheServer(t *testing.T) {
	w := newWaking(t)
	reach := w.api.SessionTriggers(w.sess.ID)
	now := w.clock.Now()
	for at, reason := range map[time.Time]string{
		now.Add(trigger.MinWakeDelay - time.Minute): trigger.CodeTooSoon,
		now.Add(trigger.MaxWakeAhead + time.Minute): trigger.CodeTooLate,
	} {
		if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_b", At: at, Title: "t", Note: "n"}); !refusedAs(err, reason) {
			t.Errorf("a wake at %s: %v, want %s", at, err, reason)
		}
	}
	if _, err := reach.SetWake(t.Context(), runner.Wake{At: now.Add(time.Hour), Title: "t", Note: "n"}); !refusedAs(err, CodeInvalidRequest) {
		t.Errorf("a wake of no call: %v", err)
	}
	// Past maxAge: skipped_late, recorded as skipped.
	tr := w.set("toolu_w", now.Add(time.Hour), 1)
	w.clock.Advance(time.Hour + trigger.WakeMaxAge + time.Minute)
	w.tick()
	if c, ok := w.cleared(); !ok || c.TriggerID != tr.ID || c.Reason != session.WakeSkipped {
		t.Fatalf("the late wake was recorded as %+v (%v)", c, ok)
	}
	if h := w.header(); h.Wake != nil {
		t.Fatalf("the header keeps the skipped wake %+v", h.Wake)
	}
	// An ended session takes no wake.
	w.runnerAppends(w.sess.ID, w.status(session.StatusEnded, session.StopCompleted))
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_e", At: w.clock.Now().Add(time.Hour), Title: "t", Note: "n"}); !refusedAs(err, CodeConflict) {
		t.Fatalf("a wake of an ended session: %v", err)
	}
}

// TestAWakeEndsWithItsSession is Topos spec 086's criterion of a wake's
// end: the agent's cancel deletes it asked as trigger.delete with its
// origin; a session's end, archive and deletion delete its wake, and the
// end and the archive record session.wake_cleared with ended; a person's
// delete of the wake's trigger records it with canceled and the sender.
func TestAWakeEndsWithItsSession(t *testing.T) {
	w := newWaking(t)
	reach := w.api.SessionTriggers(w.sess.ID)
	tr := w.set("toolu_w", w.clock.Now().Add(time.Hour), 1)
	w.questions()
	if id, err := reach.CancelWake(t.Context(), "toolu_c"); err != nil || id != tr.ID {
		t.Fatalf("the cancel: %q, %v", id, err)
	}
	if q := w.questions(); len(q) != 1 || q[0].Action != authorizer.ActionTriggerDelete || q[0].Resource.Fields["origin"].(map[string]any)["kind"] != v1.OriginWake {
		t.Fatalf("the cancel asked %+v", q)
	}
	if id, err := reach.CancelWake(t.Context(), "toolu_c2"); err != nil || id != "" {
		t.Fatalf("a cancel of no wake: %q, %v", id, err)
	}

	// A person's delete of the wake's trigger, at the route.
	tr = w.set("toolu_w2", w.clock.Now().Add(time.Hour), 1)
	if a := w.do(http.MethodDelete, "/v1/triggers/"+tr.ID, "alice", ""); a.status != http.StatusNoContent {
		t.Fatalf("the delete: %d %s", a.status, a.body)
	}
	if c, ok := w.cleared(); !ok || c.Reason != session.WakeCanceled || c.Sender == nil || c.Sender.Subject != alice || c.TriggerID != tr.ID {
		t.Fatalf("the delete was recorded as %+v (%v)", c, ok)
	}
	if h := w.header(); h.Wake != nil {
		t.Fatalf("the header keeps the deleted wake %+v", h.Wake)
	}

	// The archive of an idle session.
	tr = w.set("toolu_w3", w.clock.Now().Add(time.Hour), 1)
	if a := w.do(http.MethodPost, "/v1/sessions/"+w.sess.ID+"/archive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("the archive: %d %s", a.status, a.body)
	}
	if c, ok := w.cleared(); !ok || c.Reason != session.WakeEnded || c.TriggerID != tr.ID {
		t.Fatalf("the archive was recorded as %+v (%v)", c, ok)
	}
	if _, err := w.objects.Trigger(t.Context(), tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the archived session's wake: %v", err)
	}

	// The end of the session: the clear comes before the end.
	tr = w.set("toolu_w4", w.clock.Now().Add(time.Hour), 1)
	if a := w.do(http.MethodPost, "/v1/sessions/"+w.sess.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("the end: %d %s", a.status, a.body)
	}
	evs := w.events()
	if n := len(evs); evs[n-2].Type != session.TypeWakeCleared || evs[n-1].Type != session.TypeSessionStatus {
		t.Fatalf("the end's events are %s, %s", evs[n-2].Type, evs[n-1].Type)
	}
	if c, _ := w.cleared(); c.Reason != session.WakeEnded || c.TriggerID != tr.ID {
		t.Fatalf("the end was recorded as %+v", c)
	}
	if _, err := w.objects.Trigger(t.Context(), tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the ended session's wake: %v", err)
	}

	// The deletion of a session, and the reaper's end of an expired one.
	for _, end := range []string{"delete", "expire"} {
		other := newWaking(t)
		tr := other.set("toolu_w", other.clock.Now().Add(time.Hour), 1)
		switch end {
		case "delete":
			if a := other.do(http.MethodDelete, "/v1/sessions/"+other.sess.ID, "alice", ""); a.status != http.StatusNoContent {
				t.Fatalf("the delete: %d %s", a.status, a.body)
			}
		case "expire":
			other.clock.Advance(session.DefaultMaxAge + time.Minute)
			if err := other.api.Reap(t.Context()); err != nil {
				t.Fatal(err)
			}
			if c, ok := other.cleared(); !ok || c.Reason != session.WakeEnded {
				t.Fatalf("the expiry was recorded as %+v (%v)", c, ok)
			}
		}
		if _, err := other.objects.Trigger(t.Context(), tr.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the %s session's wake: %v", end, err)
		}
	}
}

// TestOnlyTheInitiatorConfirmsAWake: a held wake, the eleventh in a row,
// is answered by the person who started the session alone, and a set
// she allowed names her as confirmed_by.
func TestOnlyTheInitiatorConfirmsAWake(t *testing.T) {
	w := newWaking(t)
	w.runnerAppends(w.sess.ID,
		w.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_11", Name: session.ToolWake, Input: []byte(`{"action":"set","after":"1h","title":"t","note":"n"}`), Verdict: "ask", Reason: "held"}),
		w.status(session.StatusIdle, session.StopToolConfirmation))
	confirm := func(token string) answer {
		return w.do(http.MethodPost, "/v1/sessions/"+w.sess.ID+"/events", token, `{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_11","decision":"allow"}}`)
	}
	if a := confirm("bob"); a.status != http.StatusForbidden || a.code() != auth.CodeForbidden {
		t.Fatalf("bob's allow: %d %s", a.status, a.body)
	}
	if a := confirm("alice"); a.status != http.StatusOK {
		t.Fatalf("alice's allow: %d %s", a.status, a.body)
	}
	w.questions()
	if _, err := w.api.SessionTriggers(w.sess.ID).SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_11", At: w.clock.Now().Add(time.Hour), Title: "t", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range w.questions() {
		if q.Action == authorizer.ActionTriggerCreate {
			if o, _ := q.Resource.Fields["origin"].(map[string]any); o["confirmed_by"] != alice {
				t.Fatalf("the allowed wake's origin is %+v", o)
			}
		}
	}
}

// TestAReplacedHeldWakeSendsNoStaleNote: a wake that came due while its
// session waited for a person is held; a set that replaces it before the
// held message is sent replaces its trigger, so the stale note is never
// sent and the new wake fires at its own time.
func TestAReplacedHeldWakeSendsNoStaleNote(t *testing.T) {
	w := newWaking(t)
	first := w.set("toolu_w1", w.clock.Now().Add(time.Hour), 1)
	w.runnerAppends(w.sess.ID,
		w.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_d", Name: "deploy", Input: []byte(`{}`), Verdict: "ask", Reason: "held"}),
		w.status(session.StatusIdle, session.StopToolConfirmation))
	w.clock.Advance(time.Hour)
	w.tick()
	if held, err := w.objects.Trigger(t.Context(), first.ID); err != nil || held.Counts.Held != 1 {
		t.Fatalf("the wake that came due while the session waited: %+v, %v", held.Counts, err)
	}
	n := len(w.messages(w.sess.ID))
	// The person answers; the turn runs and sets a later wake.
	w.runnerAppends(w.sess.ID, w.status(session.StatusRunning, ""))
	later := w.clock.Now().Add(2 * time.Hour)
	second := w.set("toolu_w2", later, 1)
	if second.ID == first.ID {
		t.Fatal("the replacing wake kept the held one's trigger")
	}
	w.tick()
	if got := w.messages(w.sess.ID); len(got) != n {
		t.Fatalf("the stale note was sent: %+v", got[n:])
	}
	w.runnerAppends(w.sess.ID, w.status(session.StatusIdle, session.StopEndTurn))
	w.clock.Advance(2 * time.Hour)
	w.tick()
	msgs := w.messages(w.sess.ID)
	if len(msgs) != n+1 || msgs[n].Wake == nil || msgs[n].Wake.TriggerID != second.ID || !msgs[n].Wake.DueAt.Equal(later) {
		t.Fatalf("the replacing wake's message is %+v", msgs[n:])
	}
}

// TestARefusedWakeLeavesItsSession: a wake whose message the authorizer
// refuses for a limit that resets, as a spent proactive pool does, waits:
// its trigger and the session's wake are kept, and it is sent once the
// limit resets, under its own time (spec 087). A wake refused for good is
// spent, sends nothing, and its session records session.wake_cleared with
// refused and the refusal's reason, so the header holds no wake that will
// not come.
func TestARefusedWakeLeavesItsSession(t *testing.T) {
	w := newWaking(t)
	due := w.clock.Now().Add(time.Hour)
	tr := w.set("toolu_w", due, 1)
	if h := w.header(); h.Wake == nil || h.Wake.TriggerID != tr.ID {
		t.Fatalf("the header before the wake: %+v", h.Wake)
	}
	resets := due.Add(6 * time.Hour)
	limits := json.RawMessage(`{"resets_at":"` + resets.Format(time.RFC3339) + `"}`)
	reason := "proactive_share_spent"
	w.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Reason: reason, Limits: limits}, nil
		}
		return w.authz.next.Authorize(context.Background(), req)
	}
	w.clock.Advance(time.Hour)
	w.tick()
	if _, ok := w.cleared(); ok {
		t.Fatal("a wake that waits for its limit was cleared")
	}
	if h := w.header(); h.Wake == nil || h.Wake.TriggerID != tr.ID {
		t.Fatalf("the header while the wake waits: %+v", h.Wake)
	}
	kept, err := w.objects.Trigger(t.Context(), tr.ID)
	if err != nil || kept.NextFireAt == nil || !kept.NextFireAt.Equal(resets) || kept.Counts.Refused != 1 {
		t.Fatalf("the waiting wake's trigger: %+v, %v", kept, err)
	}
	// The read of the runs answers the refused run, and the wake's next
	// try when the limit resets (spec 088).
	a := w.do(http.MethodGet, "/v1/triggers/runs", "alice", "")
	var page struct {
		Items []triggerRuns `json:"items"`
	}
	a.decode(t, &page)
	var item *triggerRuns
	for i := range page.Items {
		if page.Items[i].TriggerID == tr.ID {
			item = &page.Items[i]
		}
	}
	if item == nil || len(item.Fires) != 1 || !item.Fires[0].Equal(resets) || len(item.Runs) != 1 || item.Runs[0].Outcome != store.OutcomeRefused || item.Runs[0].Reason != reason {
		t.Fatalf("the waiting wake in the read of the runs: %d %s", a.status, a.body)
	}
	// Once the limit resets, the wake is sent, due at its own time.
	w.authz.answer = nil
	w.clock.Advance(6 * time.Hour)
	w.tick()
	if h := w.header(); h.Wake != nil {
		t.Fatalf("the header after the wake was sent: %+v", h.Wake)
	}
	var sent *session.MessageWake
	for _, e := range w.events() {
		var m session.UserMessage
		if e.Type == session.TypeUserMessage && e.Decode(&m) == nil && m.Wake != nil {
			sent = m.Wake
		}
	}
	if sent == nil || sent.TriggerID != tr.ID || !sent.DueAt.Equal(due) {
		t.Fatalf("the wake's message: %+v", sent)
	}

	// A wake refused for good is spent and leaves its session.
	w.runnerAppends(w.sess.ID, w.status(session.StatusIdle, session.StopEndTurn))
	gone := w.set("toolu_g", w.clock.Now().Add(time.Hour), 2)
	limits = nil
	w.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Reason: reason}, nil
		}
		return w.authz.next.Authorize(context.Background(), req)
	}
	w.clock.Advance(time.Hour)
	w.tick()
	if c, ok := w.cleared(); !ok || c.TriggerID != gone.ID || c.Reason != session.WakeRefused || c.Refusal != reason {
		t.Fatalf("the refused wake was recorded as %+v (%v)", c, ok)
	}
	if h := w.header(); h.Wake != nil {
		t.Fatalf("the header keeps the refused wake %+v", h.Wake)
	}
	if _, err := w.objects.Trigger(t.Context(), gone.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the refused wake's trigger: %v", err)
	}
}

// TestATriggerThatNamesATargetIsAppliedThroughItsRoute: PUT
// /triggers/{name} of a trigger whose session.target names an open
// session of its agent asks session.send on that session, as the
// trigger's sender, and stores the trigger; a deny of that send refuses
// the apply, which stores nothing.
func TestATriggerThatNamesATargetIsAppliedThroughItsRoute(t *testing.T) {
	f := newTriggerFixture(t)
	open := f.create("alice", "reviewer")
	var sends []authz.Request
	deny := false
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionSend {
			sends = append(sends, req)
			if deny {
				return authz.Decision{Reason: "not_owner"}, nil
			}
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	a := f.do(http.MethodPut, "/v1/triggers/follow", "alice", triggerYAML("follow", eventSpec("policy: continue, target: "+open.ID)))
	if a.status != http.StatusCreated {
		t.Fatalf("apply: %d %s", a.status, a.body)
	}
	if len(sends) != 1 || sends[0].Resource.ID != open.ID || sends[0].Resource.String("sender") != session.TriggerSubjectPrefix+"follow" {
		t.Fatalf("the apply asked session.send %+v", sends)
	}
	deny = true
	if a := f.do(http.MethodPut, "/v1/triggers/other", "alice", triggerYAML("other", eventSpec("policy: continue, target: "+open.ID))); a.code() != auth.CodeForbidden {
		t.Fatalf("an apply whose send is denied: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodGet, "/v1/triggers/other", "alice", ""); a.status != http.StatusNotFound {
		t.Fatalf("a refused trigger was stored: %d %s", a.status, a.body)
	}
}
