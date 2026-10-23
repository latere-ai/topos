// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// holdAll sets token's hold over every trigger and answers it.
func (f *triggerFixture) holdAll(token string, held bool) holdAnswer {
	f.t.Helper()
	body := `{"held":false}`
	if held {
		body = `{"held":true}`
	}
	a := f.do(http.MethodPut, "/v1/triggers/hold", token, body)
	if a.status != http.StatusOK {
		f.t.Fatalf("hold %v: %d %s", held, a.status, a.body)
	}
	var out holdAnswer
	a.decode(f.t, &out)
	return out
}

// TestAHoldStopsEveryTrigger is Topos spec 086's hold criterion: while an
// owner is held, the minute loop fires none of their triggers and records
// nothing, the fire route answers conflict, and another owner's fire as
// before; a wake set while held keeps its time and its result says so;
// on release a schedule starts from then, firing nothing it missed, and
// a wake due during the hold fires at the next pass with its lateness in
// its message. trigger.hold names the owner, and held when it sets it.
func TestAHoldStopsEveryTrigger(t *testing.T) {
	w := newWaking(t)
	var mu sync.Mutex
	var holds []authz.Request
	next := w.authz.answer
	w.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionTriggerHold {
			mu.Lock()
			holds = append(holds, req)
			mu.Unlock()
		}
		return next(req)
	}
	hourly := w.applyTrigger("alice", "hourly", "schedule: '@hourly', session: {message: x}")
	w.apply("bob", "reviewer", "Review.")
	bobs := w.do(http.MethodPut, "/v1/triggers/bobs", "bob", triggerOf("reviewer", "bobs", "schedule: '@hourly', session: {message: y}"))
	if bobs.status != http.StatusCreated {
		t.Fatalf("bob's trigger: %d %s", bobs.status, bobs.body)
	}
	var bob v1.Trigger
	bobs.decode(t, &bob)

	if a := w.do(http.MethodGet, "/v1/triggers/hold", "alice", ""); a.status != http.StatusOK || strings.TrimSpace(string(a.body)) != `{"held":false}` {
		t.Fatalf("the hold before: %d %s", a.status, a.body)
	}
	heldAt := w.clock.Now().UTC()
	if got := w.holdAll("alice", true); !got.Held || got.Since == nil || !got.Since.Equal(heldAt) {
		t.Fatalf("the hold is %+v", got)
	}
	mu.Lock()
	if len(holds) != 2 || holds[0].Resource.String("owner") != alice || holds[0].Resource.Fields["held"] != nil ||
		holds[1].Resource.String("owner") != alice || holds[1].Resource.Fields["held"] != true || holds[1].Subject != alice {
		t.Fatalf("trigger.hold was asked %+v", holds)
	}
	mu.Unlock()

	// A wake set while held keeps its time, and its result says so.
	due := w.clock.Now().Add(30 * time.Minute)
	set, err := w.api.SessionTriggers(w.sess.ID).SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w", At: due, Title: "Look", Note: "Again."})
	if err != nil || !set.Held || set.NextFireAt == nil || !set.NextFireAt.Equal(due) {
		t.Fatalf("a wake set while held: %+v, %v", set, err)
	}
	w.runnerAppends(w.sess.ID, w.event(session.TypeWakeSet, session.WakeSet{TriggerID: set.ID, At: due, Title: "Look", Note: "Again.", ToolUseID: "toolu_w", Count: 1}))

	// Two hours pass held: nothing of alice's fires or is recorded; bob's
	// runs as before.
	w.clock.Advance(2 * time.Hour)
	w.tick()
	if c := w.counts("hourly"); c != (v1.TriggerCounts{}) {
		t.Fatalf("a held schedule recorded %+v", c)
	}
	if len(w.sessionsOf(hourly.Status.ID)) != 0 {
		t.Fatal("a held schedule started a session")
	}
	if kept, err := w.objects.Trigger(t.Context(), set.ID); err != nil || kept.Counts != (v1.TriggerCounts{}) {
		t.Fatalf("the held wake: %+v, %v", kept.Counts, err)
	}
	if len(w.sessionsOf(bob.Status.ID)) == 0 {
		t.Fatal("another owner's schedule was held")
	}
	if a := w.do(http.MethodPost, "/v1/triggers/hourly/fire", "alice", ""); a.status != http.StatusConflict || a.code() != CodeConflict {
		t.Fatalf("a held trigger's fire: %d %s", a.status, a.body)
	}
	if c := w.counts("hourly"); c != (v1.TriggerCounts{}) {
		t.Fatalf("a refused fire recorded %+v", c)
	}

	// The release: the schedule starts from now, and the wake fires late.
	released := w.clock.Now()
	if got := w.holdAll("alice", false); got.Held || got.Since != nil {
		t.Fatalf("the release is %+v", got)
	}
	stored, err := w.objects.Trigger(t.Context(), hourly.Status.ID)
	if err != nil || stored.NextFireAt == nil || !stored.NextFireAt.After(released) {
		t.Fatalf("the released schedule fires next at %v, %v", stored.NextFireAt, err)
	}
	w.tick()
	if c := w.counts("hourly"); c != (v1.TriggerCounts{}) {
		t.Fatalf("the released schedule fired what it missed: %+v", c)
	}
	msgs := w.messages(w.sess.ID)
	last := msgs[len(msgs)-1]
	if last.Wake == nil || last.Wake.TriggerID != set.ID || !last.Wake.DueAt.Equal(due) {
		t.Fatalf("the late wake's message is %+v", last)
	}
	evs := w.events()
	tr, err := session.FoldWith(evs, "", session.FoldOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lead := tr.Messages[len(tr.Messages)-1].Blocks; len(lead) == 0 || !strings.Contains(lead[len(lead)-2].Text, "It came late") {
		t.Fatalf("the late wake is led by %+v", lead)
	}
	if _, err := w.objects.Trigger(t.Context(), set.ID); err == nil {
		t.Fatal("the fired wake is kept")
	}
	if a := w.do(http.MethodPut, "/v1/triggers/hold", "alice", `{}`); a.status != http.StatusBadRequest {
		t.Fatalf("a hold of no held: %d %s", a.status, a.body)
	}
}

// TestTheQuestionsOfASessionsTriggerCalls is Topos spec 086's authorizer
// criterion: every trigger.create, trigger.update and trigger.delete a
// session's call asks carries origin {kind, session_id, tool_use_id,
// confirmed_by?}, kind proposal for the trigger tool and wake for the
// wake tool, and is asked as the session's initiator with org_id alone as
// its claims; a create or an update carries notify, and a one-shot's at;
// a list carries owner and agent; a firing's session.create carries
// attended; and trigger.hold names owner and held.
func TestTheQuestionsOfASessionsTriggerCalls(t *testing.T) {
	w := newWaking(t)
	var mu sync.Mutex
	var asked []authz.Request
	next := w.authz.answer
	w.authz.answer = func(req authz.Request) (authz.Decision, error) {
		mu.Lock()
		asked = append(asked, req)
		mu.Unlock()
		return next(req)
	}
	take := func() []authz.Request {
		mu.Lock()
		defer mu.Unlock()
		out := asked
		asked = nil
		return out
	}
	reach := w.api.SessionTriggers(w.sess.ID)
	// The proposal the fixture held was denied; this one is allowed.
	w.holdAll("alice", false)
	take()
	w.hold("toolu_p", map[string]any{"action": "propose", "name": weekly.Name, "ask": weekly.Ask, "why": weekly.Why, "schedule": weekly.Schedule, "timeZone": weekly.TimeZone})
	w.allow("toolu_p")
	p := weekly
	p.ToolUseID = "toolu_p"
	if err := reach.CheckProposal(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	made, err := reach.ApplyProposal(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reach.ApplyProposal(t.Context(), runner.Proposal{ToolUseID: "toolu_x", Action: runner.TriggerPause, ID: made.ID, Why: "Away."}); err != nil {
		t.Fatal(err)
	}
	w.hold("toolu_d", map[string]any{"action": "delete", "id": made.ID})
	w.allow("toolu_d")
	if _, err := reach.ApplyProposal(t.Context(), runner.Proposal{ToolUseID: "toolu_d", Action: runner.TriggerDelete, ID: made.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w", At: w.clock.Now().Add(time.Hour), Title: "Look", Note: "Again."}); err != nil {
		t.Fatal(err)
	}
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w2", At: w.clock.Now().Add(2 * time.Hour), Title: "Look", Note: "Again."}); err != nil {
		t.Fatal(err)
	}
	if _, err := reach.CancelWake(t.Context(), "toolu_c"); err != nil {
		t.Fatal(err)
	}
	if _, err := reach.ListTriggers(t.Context()); err != nil {
		t.Fatal(err)
	}
	type want struct {
		action, kind, toolUseID string
		confirmed               bool
	}
	wants := []want{
		{authorizer.ActionTriggerCreate, v1.OriginProposal, "toolu_p", false},
		{authorizer.ActionTriggerCreate, v1.OriginProposal, "toolu_p", true},
		{authorizer.ActionTriggerUpdate, v1.OriginProposal, "toolu_x", false},
		{authorizer.ActionTriggerDelete, v1.OriginProposal, "toolu_d", true},
		{authorizer.ActionTriggerCreate, v1.OriginWake, "toolu_w", false},
		{authorizer.ActionTriggerUpdate, v1.OriginWake, "toolu_w2", false},
		{authorizer.ActionTriggerDelete, v1.OriginWake, "toolu_c", false},
	}
	var got []authz.Request
	var list *authz.Request
	for _, q := range take() {
		switch q.Action {
		case authorizer.ActionTriggerCreate, authorizer.ActionTriggerUpdate, authorizer.ActionTriggerDelete:
			got = append(got, q)
		case authorizer.ActionTriggerList:
			list = &q
		}
	}
	if len(got) != len(wants) {
		t.Fatalf("the trigger questions are %d, want %d: %+v", len(got), len(wants), got)
	}
	for i, q := range got {
		wt := wants[i]
		o, _ := q.Resource.Fields["origin"].(map[string]any)
		_, confirmed := o["confirmed_by"]
		if q.Action != wt.action || o["kind"] != wt.kind || o["session_id"] != w.sess.ID || o["tool_use_id"] != wt.toolUseID || confirmed != wt.confirmed ||
			wt.confirmed && o["confirmed_by"] != alice {
			t.Errorf("question %d is %s with origin %+v, want %+v", i, q.Action, o, wt)
		}
		if q.Subject != alice || !maps.Equal(q.Claims, map[string]any{"org_id": ""}) {
			t.Errorf("question %d is asked as %s with claims %+v", i, q.Subject, q.Claims)
		}
		if q.Action != authorizer.ActionTriggerDelete && q.Resource.String("notify") == "" {
			t.Errorf("question %d carries no notify", i)
		}
		if wt.kind == v1.OriginWake && q.Action != authorizer.ActionTriggerDelete && q.Resource.String("at") == "" {
			t.Errorf("question %d of a wake carries no at", i)
		}
	}
	if list == nil || list.Resource.String("owner") != alice || list.Resource.String("agent") != w.reviewer() {
		t.Fatalf("the list asked %+v", list)
	}

	// A firing's session.create carries attended, and trigger.hold its
	// owner and held.
	tr := w.applyTrigger("alice", "asks", "schedule: '@hourly', session: {message: x, attended: true}")
	take()
	w.fired("alice", tr.Status.ID, "", store.OutcomeStarted)
	var create *authz.Request
	for _, q := range take() {
		if q.Action == authorizer.ActionSessionCreate {
			create = &q
		}
	}
	if create == nil || create.Resource.Fields["attended"] != true {
		t.Fatalf("the firing's session.create is %+v", create)
	}
	w.holdAll("alice", true)
	hq := take()
	if len(hq) != 1 || hq[0].Action != authorizer.ActionTriggerHold || hq[0].Resource.String("owner") != alice || hq[0].Resource.Fields["held"] != true {
		t.Fatalf("trigger.hold was asked %+v", hq)
	}
}
