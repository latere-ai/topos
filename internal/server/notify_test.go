// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// notifier applies alice's agent reviewer anew naming notify among its
// tools.
func (f *triggerFixture) notifier() {
	f.t.Helper()
	body := fmt.Sprintf("apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reviewer\nspec:\n  model: {name: claude-haiku-4-5}\n  instructions: Review.\n  machine: {kind: cella}\n  tools: [read, %s]\n", session.ToolNotify)
	if a := f.do(http.MethodPut, "/v1/agents/reviewer", "alice", body); a.status != http.StatusOK && a.status != http.StatusCreated {
		f.t.Fatalf("apply reviewer with notify: %d %s", a.status, a.body)
	}
}

// TestAnAttendedTriggerWaitsForItsOwner is Topos spec 086's attended
// criterion: a firing of a trigger with session.attended starts an
// attended session, whose questions wait for its owner, asked as
// session.create with attended; a trigger without it starts one nobody
// attends, asked with attended false.
func TestAnAttendedTriggerWaitsForItsOwner(t *testing.T) {
	f := newTriggerFixture(t)
	var mu sync.Mutex
	var creates []authz.Request
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionCreate {
			mu.Lock()
			creates = append(creates, req)
			mu.Unlock()
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	for _, c := range []struct {
		name     string
		attended bool
	}{{"asks", true}, {"tells", false}} {
		tr := f.applyTrigger("alice", c.name, fmt.Sprintf("schedule: '@hourly', session: {message: x, attended: %v}", c.attended))
		fired := f.fired("alice", tr.Status.ID, "", "started")
		s, err := f.sessions.Get(t.Context(), fired.SessionID)
		if err != nil || s.Attended != c.attended {
			t.Fatalf("%s: the session's attended is %v, %v", c.name, s.Attended, err)
		}
		mu.Lock()
		last := creates[len(creates)-1]
		mu.Unlock()
		if got, ok := last.Resource.Fields["attended"].(bool); !ok || got != c.attended {
			t.Fatalf("%s: session.create was asked with attended %v", c.name, last.Resource.Fields["attended"])
		}
	}
}

// TestAJudgedTriggerSaysSoInItsMessage: a firing of a judged trigger
// sends its message with notify judged, a new session's first and a
// continued one's alike, an always trigger's carries none, the questions
// of its apply carry notify, and judged is refused for an agent whose
// latest version does not name the notify tool.
func TestAJudgedTriggerSaysSoInItsMessage(t *testing.T) {
	f := newTriggerFixture(t)
	if a := f.do(http.MethodPut, "/v1/triggers/quiet", "alice", triggerYAML("quiet", "schedule: '@hourly', session: {message: x, notify: judged}")); a.status != http.StatusBadRequest || a.code() != "invalid_manifest" {
		t.Fatalf("judged for an agent without notify: %d %s", a.status, a.body)
	}
	f.notifier()
	var mu sync.Mutex
	var applies []authz.Request
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionTriggerCreate {
			mu.Lock()
			applies = append(applies, req)
			mu.Unlock()
		}
		return f.authz.next.Authorize(t.Context(), req)
	}
	judged := f.applyTrigger("alice", "quiet", "schedule: '@hourly', session: {message: x, notify: judged, policy: continue}")
	always := f.applyTrigger("alice", "loud", "schedule: '@hourly', session: {message: x}")
	mu.Lock()
	if len(applies) != 2 || applies[0].Resource.String("notify") != v1.NotifyJudged || applies[1].Resource.String("notify") != v1.NotifyAlways {
		t.Fatalf("the applies asked %+v", applies)
	}
	mu.Unlock()
	started := f.fired("alice", judged.Status.ID, "", "started")
	f.runnerAppends(started.SessionID, f.status(session.StatusIdle, session.StopEndTurn))
	f.fired("alice", judged.Status.ID, "", "continued")
	for _, m := range f.messages(started.SessionID) {
		if m.Notify != session.NotifyJudged || m.FiringID == "" {
			t.Fatalf("a judged trigger's message is %+v", m)
		}
	}
	loud := f.fired("alice", always.Status.ID, "", "started")
	if m := f.messages(loud.SessionID); len(m) != 1 || m[0].Notify != "" {
		t.Fatalf("an always trigger's message is %+v", m)
	}
}

// TestAnEventProposalIsJudged: a proposal on events is judged when the
// agent names notify, and always when it does not; a schedule's is
// always unless the call names judged; a change writes the notify it
// names.
func TestAnEventProposalIsJudged(t *testing.T) {
	p := newProposing(t)
	reach := p.api.SessionTriggers(p.sess.ID)
	mail := runner.Proposal{ToolUseID: "toolu_m", Name: "Invoices", Ask: "Save the invoice.", Why: "When an email arrives, I save it.", On: &v1.TriggerOn{Product: "mail", Verbs: []string{"received"}}}
	p.hold("toolu_m", map[string]any{"action": "propose", "name": mail.Name, "ask": mail.Ask, "why": mail.Why, "on": mail.On})
	p.allow("toolu_m")
	made, err := reach.ApplyProposal(t.Context(), mail)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", made.Name); got.Spec.Session.Notify != "" {
		t.Fatalf("an event proposal of an agent without notify is %q", got.Spec.Session.Notify)
	}
	p.notifier()
	mail.ToolUseID = "toolu_m2"
	p.hold("toolu_m2", map[string]any{"action": "propose", "name": mail.Name, "ask": mail.Ask, "why": mail.Why, "on": mail.On})
	p.allow("toolu_m2")
	if made, err = reach.ApplyProposal(t.Context(), mail); err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", made.Name); got.Spec.Session.Notify != v1.NotifyJudged {
		t.Fatalf("an event proposal of an agent with notify is %q", got.Spec.Session.Notify)
	}
	p.allow("toolu_1")
	weeklyMade, err := reach.ApplyProposal(t.Context(), weekly)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", weeklyMade.Name); got.Spec.Session.Notify != "" {
		t.Fatalf("a schedule's proposal is %q", got.Spec.Session.Notify)
	}
	change := runner.Proposal{ToolUseID: "toolu_c", Action: runner.TriggerChange, ID: weeklyMade.ID, Why: "Only when it changes.", Notify: v1.NotifyJudged}
	p.hold("toolu_c", map[string]any{"action": "change", "id": weeklyMade.ID, "why": change.Why, "notify": change.Notify})
	p.allow("toolu_c")
	if _, err := reach.ApplyProposal(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", weeklyMade.Name); got.Spec.Session.Notify != v1.NotifyJudged || got.Spec.Schedule != "0 9 * * 1" {
		t.Fatalf("the changed schedule is %+v", got.Spec)
	}
}
