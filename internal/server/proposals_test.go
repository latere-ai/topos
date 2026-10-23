// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/sdk-go/agents"
	"latere.ai/x/sdk-go/agents/routine"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/internal/store"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// weekly is a proposal of alice's session's call toolu_1.
var weekly = runner.Proposal{ToolUseID: "toolu_1", Name: "Weekly sign-ups", Ask: "Count last week's {{sign-ups}}.",
	Why: "Every Monday at 9:00, Berlin time, I count last week's sign-ups.", Schedule: "0 9 * * 1", TimeZone: "Europe/Berlin"}

// proposing is a session of alice's agent reviewer whose call toolu_1
// proposes weekly, held for her, with the trigger questions the server
// asked kept.
type proposing struct {
	*triggerFixture
	sess  session.Session
	mu    sync.Mutex
	asked []authz.Request
}

func newProposing(t *testing.T) *proposing {
	t.Helper()
	p := &proposing{triggerFixture: newTriggerFixture(t)}
	p.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if strings.HasPrefix(req.Action, "trigger.") {
			p.mu.Lock()
			p.asked = append(p.asked, req)
			p.mu.Unlock()
		}
		// Bob may send to alice's session, as a member of her
		// organization may to one of its sessions.
		if req.Sub == "bob" && req.Action == authorizer.ActionSessionSend {
			return authz.Decision{Allow: true}, nil
		}
		return p.authz.next.Authorize(t.Context(), req)
	}
	p.sess = p.create("alice", "reviewer")
	in, err := json.Marshal(map[string]string{"action": "propose", "name": weekly.Name, "ask": weekly.Ask, "why": weekly.Why, "schedule": weekly.Schedule, "timeZone": weekly.TimeZone})
	if err != nil {
		t.Fatal(err)
	}
	p.runnerAppends(p.sess.ID,
		p.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_1", Name: session.ToolTrigger, Input: in, Verdict: "ask", Reason: "held"}),
		p.status(session.StatusIdle, session.StopToolConfirmation))
	return p
}

// questions takes the trigger questions asked since the last take.
func (p *proposing) questions() []authz.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.asked
	p.asked = nil
	return out
}

// confirm sends a confirmation of toolu_1 as token.
func (p *proposing) confirm(token, decision string) answer {
	p.t.Helper()
	return p.do(http.MethodPost, "/v1/sessions/"+p.sess.ID+"/events", token, `{"type":"user.tool_confirmation","payload":{"tool_use_id":"toolu_1","decision":"`+decision+`"}}`)
}

// TestAnAllowedProposalIsTheOwners is Topos spec 086's third criterion:
// the check before the hold asks trigger.create as the session's initiator
// with the call's origin and stores nothing; an apply before the
// initiator's allow is refused; after it the routine is applied as hers,
// in her context, in the shape the SDK reads as a routine, with its origin
// in its status; a retry of the call applies the same trigger, and a
// later apply at the route keeps the origin.
func TestAnAllowedProposalIsTheOwners(t *testing.T) {
	p := newProposing(t)
	reach := p.api.SessionTriggers(p.sess.ID)
	name := routine.Name(p.sess.ID + "/toolu_1")
	if err := reach.CheckProposal(t.Context(), weekly); err != nil {
		t.Fatalf("the check: %v", err)
	}
	asked := p.questions()
	if len(asked) != 1 || asked[0].Action != authorizer.ActionTriggerCreate || asked[0].Subject != alice || asked[0].Resource.String("name") != name ||
		asked[0].Resource.String("agent") != p.reviewer() {
		t.Fatalf("the check asked %+v", asked)
	}
	if o, _ := asked[0].Resource.Fields["origin"].(map[string]any); o["kind"] != v1.OriginProposal || o["session_id"] != p.sess.ID || o["tool_use_id"] != "toolu_1" || o["confirmed_by"] != nil {
		t.Fatalf("the check's origin is %+v", asked[0].Resource.Fields["origin"])
	}
	if _, err := p.objects.TriggerByName(t.Context(), alice, name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the check stored a trigger: %v", err)
	}
	if _, err := reach.ApplyProposal(t.Context(), weekly); !refusedAs(err, auth.CodeForbidden) {
		t.Fatalf("an apply before the allow: %v", err)
	}
	if a := p.confirm("alice", session.DecisionAllow); a.status != http.StatusOK {
		t.Fatalf("alice's allow: %d %s", a.status, a.body)
	}
	p.questions()
	made, err := reach.ApplyProposal(t.Context(), weekly)
	if err != nil {
		t.Fatalf("the apply: %v", err)
	}
	if made.Name != name || made.Title != weekly.Name || made.NextFireAt == nil || made.Origin == nil || made.Origin.ConfirmedBy != alice {
		t.Fatalf("the apply answered %+v", made)
	}
	if o, _ := p.questions()[0].Resource.Fields["origin"].(map[string]any); o["confirmed_by"] != alice {
		t.Fatalf("the apply's origin is %+v", o)
	}
	stored := p.get("alice", name)
	if stored.Status.Origin == nil || *stored.Status.Origin != (v1.TriggerOrigin{Kind: v1.OriginProposal, SessionID: p.sess.ID, ToolUseID: "toolu_1", ConfirmedBy: alice}) {
		t.Fatalf("the trigger's origin is %+v", stored.Status.Origin)
	}
	// The SDK reads it as a routine of the agent, as every client lists
	// one: the shape is the SDK's own.
	raw := p.do(http.MethodGet, "/v1/triggers/"+name, "alice", "")
	var tr agents.Trigger
	raw.decode(t, &tr)
	r, ok := agents.RoutineOf(&tr, "reviewer")
	if !ok || r.Title != weekly.Name || r.Instruction != weekly.Ask || r.Schedule == nil || r.Schedule.Hour != 9 || r.Schedule.Zone != "Europe/Berlin" {
		t.Fatalf("the trigger reads as the routine %+v (%v): %s", r, ok, raw.body)
	}
	// A retry of the call applies the same trigger, asked as its update.
	p.questions()
	again, err := reach.ApplyProposal(t.Context(), weekly)
	if err != nil || again.ID != made.ID {
		t.Fatalf("the retry answered %+v, %v", again, err)
	}
	if q := p.questions(); len(q) != 1 || q[0].Action != authorizer.ActionTriggerUpdate {
		t.Fatalf("the retry asked %+v", q)
	}
	// The person renames it at the route; it keeps where it came from.
	renamed := strings.Replace(string(raw.body), `"displayName":"Weekly sign-ups"`, `"displayName":"Sign-ups"`, 1)
	if a := p.do(http.MethodPut, "/v1/triggers/"+name, "alice", renamed); a.status != http.StatusOK {
		t.Fatalf("the rename: %d %s", a.status, a.body)
	}
	if got := p.get("alice", name); got.Metadata.DisplayName != "Sign-ups" || got.Status.Origin == nil || got.Status.Origin.ToolUseID != "toolu_1" {
		t.Fatalf("after the rename: %+v, origin %+v", got.Metadata, got.Status.Origin)
	}
}

// refusedAs reports whether err is a refusal of code.
func refusedAs(err error, code string) bool {
	re, ok := errors.AsType[*runner.TriggerRefusal](err)
	return ok && re.Code == code
}

// TestOnlyTheInitiatorConfirmsATrigger is Topos spec 086's third
// criterion's second half: another sender, who may send to the session,
// is refused forbidden when they answer a trigger call, and a deny by the
// initiator leaves nothing to apply.
func TestOnlyTheInitiatorConfirmsATrigger(t *testing.T) {
	p := newProposing(t)
	for _, d := range []string{session.DecisionAllow, session.DecisionDeny} {
		if a := p.confirm("bob", d); a.status != http.StatusForbidden || a.code() != auth.CodeForbidden {
			t.Fatalf("bob's %s: %d %s", d, a.status, a.body)
		}
	}
	if a := p.confirm("alice", session.DecisionDeny); a.status != http.StatusOK {
		t.Fatalf("alice's deny: %d %s", a.status, a.body)
	}
	if _, err := p.api.SessionTriggers(p.sess.ID).ApplyProposal(t.Context(), weekly); !refusedAs(err, auth.CodeForbidden) {
		t.Fatalf("an apply after a deny: %v", err)
	}
}

// TestAProposalTheAuthorizerRefusesIsARefusal: a deny with a reason is a
// refusal that names it, a manifest the resolver refuses one of its code,
// and a session that ended asks nothing.
func TestAProposalTheAuthorizerRefusesIsARefusal(t *testing.T) {
	p := newProposing(t)
	next := p.authz.answer
	p.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionTriggerCreate {
			return authz.Decision{Reason: "proposals_full"}, nil
		}
		return next(req)
	}
	reach := p.api.SessionTriggers(p.sess.ID)
	if err := reach.CheckProposal(t.Context(), weekly); !refusedAs(err, "proposals_full") {
		t.Fatalf("a denied check: %v", err)
	}
	p.authz.answer = next
	bad := weekly
	bad.Schedule = "*/5 * * * *"
	if err := reach.CheckProposal(t.Context(), bad); !refusedAs(err, CodeInvalidRequest) {
		t.Fatalf("a schedule no routine runs on: %v", err)
	}
	p.runnerAppends(p.sess.ID, p.status(session.StatusEnded, session.StopEndTurn))
	if err := reach.CheckProposal(t.Context(), weekly); !refusedAs(err, CodeConflict) {
		t.Fatalf("a check in an ended session: %v", err)
	}
}

// TestTheListIsTheAgentsOwn: a list asks trigger.list as the initiator
// naming the owner and the agent, and answers her triggers of the
// session's agent alone, newest first, each with its origin.
func TestTheListIsTheAgentsOwn(t *testing.T) {
	p := newProposing(t)
	p.apply("alice", "writer", "Write.")
	p.applyTrigger("alice", "nightly", "schedule: '0 2 * * *', session: {message: x}")
	p.clock.Advance(1)
	p.do(http.MethodPut, "/v1/triggers/drafts", "alice", triggerOf("writer", "drafts", "schedule: '0 3 * * *', session: {message: y}"))
	p.clock.Advance(1)
	if a := p.confirm("alice", session.DecisionAllow); a.status != http.StatusOK {
		t.Fatalf("the allow: %d %s", a.status, a.body)
	}
	reach := p.api.SessionTriggers(p.sess.ID)
	if _, err := reach.ApplyProposal(t.Context(), weekly); err != nil {
		t.Fatal(err)
	}
	p.questions()
	list, err := reach.ListTriggers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != routine.Name(p.sess.ID+"/toolu_1") || list[0].Origin == nil || list[1].Name != "nightly" || list[1].Origin != nil || list[1].Schedule != "0 2 * * *" {
		t.Fatalf("the list is %+v", list)
	}
	q := p.questions()
	if len(q) != 1 || q[0].Action != authorizer.ActionTriggerList || q[0].Resource.String("owner") != alice || q[0].Resource.String("agent") != p.reviewer() {
		t.Fatalf("the list asked %+v", q)
	}
}

// hold appends a held trigger call id of input in the session, as its
// runner does, and the session going idle on it.
func (p *proposing) hold(id string, input map[string]any) {
	p.t.Helper()
	in, err := json.Marshal(input)
	if err != nil {
		p.t.Fatal(err)
	}
	p.runnerAppends(p.sess.ID,
		p.event(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: session.ToolTrigger, Input: in, Verdict: "ask", Reason: "held"}),
		p.status(session.StatusIdle, session.StopToolConfirmation))
}

// allow sends alice's allow of the call id.
func (p *proposing) allow(id string) {
	p.t.Helper()
	a := p.do(http.MethodPost, "/v1/sessions/"+p.sess.ID+"/events", "alice", `{"type":"user.tool_confirmation","payload":{"tool_use_id":"`+id+`","decision":"allow"}}`)
	if a.status != http.StatusOK {
		p.t.Fatalf("alice's allow of %s: %d %s", id, a.status, a.body)
	}
}

// TestTriggerActionsOnlyTouchTheAgentsOwn is Topos spec 086's fifth
// criterion at the server: a change, a resume and a delete are done only
// on the initiator's allow, a pause at once with no confirmation; each is
// asked as the initiator with the call's origin and keeps the trigger's
// own origin; a change keeps every field it does not name; and none
// reaches a trigger of another agent, another owner's, or a wake, each
// answered not_found.
func TestTriggerActionsOnlyTouchTheAgentsOwn(t *testing.T) {
	p := newProposing(t)
	reach := p.api.SessionTriggers(p.sess.ID)
	p.allow("toolu_1")
	made, err := reach.ApplyProposal(t.Context(), weekly)
	if err != nil {
		t.Fatal(err)
	}
	p.questions()

	// A change names its new name and schedule; the ask stays.
	change := runner.Proposal{ToolUseID: "toolu_2", Action: runner.TriggerChange, ID: made.ID, Name: "Sign-ups at eight", Why: "Every Monday at 8:00 instead.", Schedule: "0 8 * * 1", TimeZone: "Europe/Berlin"}
	if err := reach.CheckProposal(t.Context(), change); err != nil {
		t.Fatalf("the change's check: %v", err)
	}
	if q := p.questions(); len(q) != 1 || q[0].Action != authorizer.ActionTriggerUpdate || q[0].Subject != alice {
		t.Fatalf("the change's check asked %+v", q)
	}
	if _, err := reach.ApplyProposal(t.Context(), change); !refusedAs(err, auth.CodeForbidden) {
		t.Fatalf("a change before its allow: %v", err)
	}
	p.hold("toolu_2", map[string]any{"action": "change", "id": made.ID, "name": change.Name, "why": change.Why, "schedule": change.Schedule, "timeZone": change.TimeZone})
	p.allow("toolu_2")
	changed, err := reach.ApplyProposal(t.Context(), change)
	if err != nil {
		t.Fatalf("the change: %v", err)
	}
	q := p.questions()
	if o, _ := q[len(q)-1].Resource.Fields["origin"].(map[string]any); o["tool_use_id"] != "toolu_2" || o["confirmed_by"] != alice || o["kind"] != v1.OriginProposal {
		t.Fatalf("the change's origin is %+v", o)
	}
	got := p.get("alice", made.Name)
	if changed.ID != made.ID || got.Metadata.DisplayName != change.Name || got.Spec.Schedule != "0 8 * * 1" || got.Spec.Session.Message != `Count last week's \{{sign-ups}}.` ||
		got.Status.Origin == nil || got.Status.Origin.ToolUseID != "toolu_1" {
		t.Fatalf("after the change: %+v, origin %+v", got.Spec, got.Status.Origin)
	}

	// A pause runs on no confirmation; a resume waits for one.
	pause := runner.Proposal{ToolUseID: "toolu_3", Action: runner.TriggerPause, ID: made.ID, Why: "The person is away this month."}
	p.questions()
	if paused, err := reach.ApplyProposal(t.Context(), pause); err != nil || !paused.Suspended || paused.NextFireAt != nil {
		t.Fatalf("the pause answered %+v, %v", paused, err)
	}
	if o, _ := p.questions()[0].Resource.Fields["origin"].(map[string]any); o["tool_use_id"] != "toolu_3" || o["confirmed_by"] != nil {
		t.Fatalf("the pause's origin is %+v", o)
	}
	resume := runner.Proposal{ToolUseID: "toolu_4", Action: runner.TriggerResume, ID: made.ID}
	if _, err := reach.ApplyProposal(t.Context(), resume); !refusedAs(err, auth.CodeForbidden) {
		t.Fatalf("a resume before its allow: %v", err)
	}
	p.hold("toolu_4", map[string]any{"action": "resume", "id": made.ID})
	p.allow("toolu_4")
	if resumed, err := reach.ApplyProposal(t.Context(), resume); err != nil || resumed.Suspended || resumed.NextFireAt == nil {
		t.Fatalf("the resume answered %+v, %v", resumed, err)
	}

	// None reaches another agent's trigger, another owner's, or a wake.
	p.apply("alice", "writer", "Write.")
	other := p.do(http.MethodPut, "/v1/triggers/drafts", "alice", triggerOf("writer", "drafts", "schedule: '0 3 * * *', session: {message: y}"))
	var drafts v1.Trigger
	other.decode(t, &drafts)
	p.apply("bob", "reviewer", "Review.")
	bobs := p.do(http.MethodPut, "/v1/triggers/bobs", "bob", triggerOf("reviewer", "bobs", "schedule: '0 3 * * *', session: {message: y}"))
	var bob v1.Trigger
	bobs.decode(t, &bob)
	wake := p.applyTrigger("alice", "wake-like", "schedule: '0 4 * * *', session: {message: z}")
	stored, err := p.objects.Trigger(t.Context(), wake.Status.ID)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.DecodeTrigger(stored.Doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.Status.Origin = &v1.TriggerOrigin{Kind: v1.OriginWake, SessionID: p.sess.ID, ToolUseID: "toolu_w"}
	if stored.Doc, err = session.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	if err := p.objects.PutTrigger(t.Context(), stored); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{drafts.Status.ID, bob.Status.ID, wake.Status.ID, "trg_01JZZZZZZZZZZZZZZZZZZZZZZZ"} {
		for _, action := range []string{runner.TriggerChange, runner.TriggerPause, runner.TriggerResume, runner.TriggerDelete} {
			call := runner.Proposal{ToolUseID: "toolu_x", Action: action, ID: id, Name: "Renamed", Why: "Because."}
			if err := reach.CheckProposal(t.Context(), call); !refusedAs(err, auth.CodeNotFound) {
				t.Errorf("a %s of %s: %v", action, id, err)
			}
		}
	}

	// A delete waits for its allow, and removes the trigger.
	del := runner.Proposal{ToolUseID: "toolu_5", Action: runner.TriggerDelete, ID: made.ID}
	p.questions()
	if err := reach.CheckProposal(t.Context(), del); err != nil {
		t.Fatalf("the delete's check: %v", err)
	}
	if q := p.questions(); len(q) != 1 || q[0].Action != authorizer.ActionTriggerDelete || q[0].Resource.Fields["origin"] == nil {
		t.Fatalf("the delete's check asked %+v", q)
	}
	if _, err := reach.ApplyProposal(t.Context(), del); !refusedAs(err, auth.CodeForbidden) {
		t.Fatalf("a delete before its allow: %v", err)
	}
	p.hold("toolu_5", map[string]any{"action": "delete", "id": made.ID})
	p.allow("toolu_5")
	if _, err := reach.ApplyProposal(t.Context(), del); err != nil {
		t.Fatalf("the delete: %v", err)
	}
	if _, err := p.objects.Trigger(t.Context(), made.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the deleted trigger: %v", err)
	}
}

// TestAnEventProposalIsARoutineOrAnEventTrigger: an offer of a whole
// product's arrivals of mail is applied as the routine the SDK reads as
// one that starts from an email; an offer of a repository's pushes, which
// no routine expresses, under the event- name with the filter as its on,
// the event after the ask, and the same origin; and a routine is not
// changed to a filter no routine expresses.
func TestAnEventProposalIsARoutineOrAnEventTrigger(t *testing.T) {
	p := newProposing(t)
	reach := p.api.SessionTriggers(p.sess.ID)
	mail := runner.Proposal{ToolUseID: "toolu_m", Name: "Invoices", Ask: "Save the invoice.", Why: "When an email arrives, I save its invoice.", On: &v1.TriggerOn{Product: "mail", Verbs: []string{"received"}}}
	p.hold("toolu_m", map[string]any{"action": "propose", "name": mail.Name, "ask": mail.Ask, "why": mail.Why, "on": mail.On})
	p.allow("toolu_m")
	made, err := reach.ApplyProposal(t.Context(), mail)
	if err != nil {
		t.Fatal(err)
	}
	raw := p.do(http.MethodGet, "/v1/triggers/"+made.Name, "alice", "")
	var tr agents.Trigger
	raw.decode(t, &tr)
	r, ok := agents.RoutineOf(&tr, "reviewer")
	if !ok || r.Starts != routine.FromEmail || r.Instruction != mail.Ask || made.Name != routine.Name(p.sess.ID+"/toolu_m") {
		t.Fatalf("the mail offer reads as %+v (%v): %s", r, ok, raw.body)
	}

	push := runner.Proposal{ToolUseID: "toolu_p", Name: "Pushes to main", Ask: "Review what was pushed.", Why: "When someone pushes to main, I review it.",
		On: &v1.TriggerOn{Product: "github", Verbs: []string{"push"}, Resources: []string{"acme/app"}}}
	p.hold("toolu_p", map[string]any{"action": "propose", "name": push.Name, "ask": push.Ask, "why": push.Why, "on": push.On})
	p.allow("toolu_p")
	ev, err := reach.ApplyProposal(t.Context(), push)
	if err != nil {
		t.Fatal(err)
	}
	got := p.get("alice", ev.Name)
	if !strings.HasPrefix(ev.Name, "event-") || routine.IsName(ev.Name) || got.Spec.On == nil || got.Spec.On.Product != "github" || got.Spec.Schedule != "" ||
		!strings.HasPrefix(got.Spec.Session.Message, "Review what was pushed.\n\nThe event that started this run:") || got.Metadata.Labels["applied-by"] != "reviewer" ||
		got.Status.Origin == nil || got.Status.Origin.ConfirmedBy != alice || got.Spec.SkipIfActive == nil || *got.Spec.SkipIfActive {
		t.Fatalf("the push offer is %s: %+v", ev.Name, got)
	}
	raw = p.do(http.MethodGet, "/v1/triggers/"+ev.Name, "alice", "")
	raw.decode(t, &tr)
	if _, ok := agents.RoutineOf(&tr, "reviewer"); ok {
		t.Fatalf("the push offer reads as a routine: %s", raw.body)
	}
	// The SDK reads it back as the offer on events it wrote.
	watch, ok := routine.ReadEvent(routine.Trigger{Name: got.Metadata.Name, DisplayName: got.Metadata.DisplayName, Labels: got.Metadata.Labels,
		Product: got.Spec.On.Product, Message: got.Spec.Session.Message, Title: got.Spec.Session.Title}, "reviewer")
	if !ok || watch.Title != push.Name || watch.Ask != push.Ask || ev.Name != routine.EventName(routine.OfferKey(p.sess.ID, "toolu_p")) {
		t.Fatalf("the SDK reads the push offer as %+v (%v)", watch, ok)
	}

	// The mail routine cannot become the pushes, which no routine runs on.
	change := runner.Proposal{ToolUseID: "toolu_c", Action: runner.TriggerChange, ID: made.ID, Why: "Pushes instead.", On: push.On}
	if err := reach.CheckProposal(t.Context(), change); !refusedAs(err, CodeInvalidRequest) {
		t.Fatalf("a routine changed to pushes: %v", err)
	}
	// The event trigger's ask changes and keeps the event after it.
	change = runner.Proposal{ToolUseID: "toolu_e", Action: runner.TriggerChange, ID: ev.ID, Why: "Shorter.", Ask: "Summarize the push."}
	p.hold("toolu_e", map[string]any{"action": "change", "id": ev.ID, "why": change.Why, "ask": change.Ask})
	p.allow("toolu_e")
	if _, err := reach.ApplyProposal(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", ev.Name); !strings.HasPrefix(got.Spec.Session.Message, "Summarize the push.\n\nThe event that started this run:") || got.Spec.On == nil {
		t.Fatalf("after the change: %+v", got.Spec)
	}
}

// TestAPauseRecordsWhyAndByWhom: the trigger tool's pause writes
// status.paused with its why, the person it was applied as, the call and
// the time; a change while paused keeps it; a resume clears it; and a
// person's suspend at the route records them with no why and no call.
func TestAPauseRecordsWhyAndByWhom(t *testing.T) {
	p := newProposing(t)
	reach := p.api.SessionTriggers(p.sess.ID)
	p.allow("toolu_1")
	made, err := reach.ApplyProposal(t.Context(), weekly)
	if err != nil {
		t.Fatal(err)
	}
	pausedAt := p.clock.Now()
	if _, err := reach.ApplyProposal(t.Context(), runner.Proposal{ToolUseID: "toolu_p", Action: runner.TriggerPause, ID: made.ID, Why: " The person is away this month. "}); err != nil {
		t.Fatal(err)
	}
	want := v1.TriggerPause{Why: "The person is away this month.", By: alice, SessionID: p.sess.ID, ToolUseID: "toolu_p", At: pausedAt.UTC()}
	if got := p.get("alice", made.Name).Status.Paused; got == nil || *got != want {
		t.Fatalf("the pause is %+v, want %+v", got, want)
	}
	// A change while paused keeps the pause as it was.
	p.clock.Advance(time.Hour)
	change := runner.Proposal{ToolUseID: "toolu_c", Action: runner.TriggerChange, ID: made.ID, Why: "Shorter.", Name: "Sign-ups"}
	p.hold("toolu_c", map[string]any{"action": "change", "id": made.ID, "why": change.Why, "name": change.Name})
	p.allow("toolu_c")
	if _, err := reach.ApplyProposal(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", made.Name); got.Status.Paused == nil || *got.Status.Paused != want || !got.Spec.Suspend {
		t.Fatalf("after a change, the pause is %+v", got.Status.Paused)
	}
	// A resume clears it.
	resume := runner.Proposal{ToolUseID: "toolu_r", Action: runner.TriggerResume, ID: made.ID}
	p.hold("toolu_r", map[string]any{"action": "resume", "id": made.ID})
	p.allow("toolu_r")
	if _, err := reach.ApplyProposal(t.Context(), resume); err != nil {
		t.Fatal(err)
	}
	if got := p.get("alice", made.Name).Status.Paused; got != nil {
		t.Fatalf("a resumed trigger keeps the pause %+v", got)
	}
	// A person's suspend at the route.
	tr := p.applyTrigger("alice", "nightly", "schedule: '0 2 * * *', session: {message: x}, suspend: true")
	if got := tr.Status.Paused; got == nil || got.By != alice || got.Why != "" || got.SessionID != "" || got.ToolUseID != "" || got.At.IsZero() {
		t.Fatalf("a suspend at the route is recorded as %+v", got)
	}
	again := p.applyTrigger("alice", "nightly", "schedule: '0 3 * * *', session: {message: x}, suspend: true")
	if again.Status.Paused == nil || !again.Status.Paused.At.Equal(tr.Status.Paused.At) {
		t.Fatalf("a second apply while suspended moved the pause to %+v", again.Status.Paused)
	}
	if run := p.applyTrigger("alice", "nightly", "schedule: '0 3 * * *', session: {message: x}"); run.Status.Paused != nil {
		t.Fatalf("a trigger applied running keeps the pause %+v", run.Status.Paused)
	}
}

// TestTheListNamesAWakesTime: the tool's list answers a wake with its
// time and the session it continues, by its title.
func TestTheListNamesAWakesTime(t *testing.T) {
	w := newWaking(t)
	at := w.clock.Now().Add(time.Hour)
	set := w.set("toolu_w", at, 1)
	list, err := w.api.SessionTriggers(w.sess.ID).ListTriggers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if l.ID != set.ID {
			continue
		}
		if l.At == nil || !l.At.Equal(at) || l.Target != w.sess.ID || l.TargetTitle != w.header().Title || l.Origin == nil || l.Origin.Kind != v1.OriginWake {
			t.Fatalf("the wake is listed as %+v", l)
		}
		return
	}
	t.Fatalf("the list %+v holds no wake", list)
}
