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
