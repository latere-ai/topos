// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// TestAFailoverAsksTheInitiatorForAnotherModel: a failover asks the
// authorizer session.update as the session's initiator in the session's
// context, with the routed name as model, the model the session stands on
// as current_model and current_model_via, and the model that failed as
// failed_model; it answers the model the allow names with the session's
// via, checked as a switch checks one, and appends nothing, since the
// harness records the change in its own batch.
func TestAFailoverAsksTheInitiatorForAnotherModel(t *testing.T) {
	f, names := checking(t)
	f.applyModel("quick", "name: "+quick)
	r := f.route(func(req authz.Request) string {
		switch {
		case req.Action == authorizer.ActionSessionCreate:
			return haiku
		case req.Action == authorizer.ActionSessionUpdate && req.Resource.String("failed_model") == haiku:
			return sonnet
		}
		return ""
	})
	s := f.create("alice", "quick")
	before := len(f.log(s.ID))
	*names = nil
	on := session.ModelRef{Name: haiku, Via: quick}
	next, err := f.api.Failover(t.Context(), s.ID, on, on, "", "")
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: quick}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	asked := r.last(t, authorizer.ActionSessionUpdate)
	if claims, _ := json.Marshal(asked.Claims); asked.Subject != alice || asked.Issuer != issuer || asked.Sub != "alice" || string(claims) != `{"org_id":""}` {
		t.Fatalf("the failover asked as %s %s %s, claims %s", asked.Subject, asked.Issuer, asked.Sub, claims)
	}
	want := `{"agent":"AGENT","current_model":"claude-haiku-4-5","current_model_via":"tier/quick","failed_model":"claude-haiku-4-5","id":"SESSION","kind":"session","model":"tier/quick","owner":"https://login.example|alice","runner":"hosted","session_id":"SESSION"}`
	if got := wire(t, asked, s.ID, "SESSION", s.Agent.ID, "AGENT"); got != want {
		t.Fatalf("session.update asked about\n%s, want\n%s", got, want)
	}
	if !slices.Equal(*names, []string{sonnet}) {
		t.Fatalf("the failover checked %v", *names)
	}
	if n := len(f.log(s.ID)); n != before {
		t.Fatalf("the failover appended %d events", n-before)
	}
}

// TestAFailoverOfARejectedRequestSaysWhy: a request the provider rejected
// asks the same session.update with failed_reason rejected and the
// gateway's code and detail as failed_detail, and moves the turn to the
// model the allow names; an allow that names none keeps it where it is.
func TestAFailoverOfARejectedRequestSaysWhy(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("quick", "name: "+quick)
	answer := sonnet
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionCreate {
			return haiku
		}
		return answer
	})
	s := f.create("alice", "quick")
	on := session.ModelRef{Name: haiku, Via: quick}
	const detail = "upstream_rejected: upstream status 404: no endpoints"
	next, err := f.api.Failover(t.Context(), s.ID, on, on, harness.FailedRejected, detail)
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: quick}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	asked := r.last(t, authorizer.ActionSessionUpdate)
	want := `{"agent":"AGENT","current_model":"claude-haiku-4-5","current_model_via":"tier/quick","failed_detail":"upstream_rejected: upstream status 404: no endpoints","failed_model":"claude-haiku-4-5","failed_reason":"rejected","id":"SESSION","kind":"session","model":"tier/quick","owner":"https://login.example|alice","runner":"hosted","session_id":"SESSION"}`
	if got := wire(t, asked, s.ID, "SESSION", s.Agent.ID, "AGENT"); got != want {
		t.Fatalf("session.update asked about\n%s, want\n%s", got, want)
	}
	answer = ""
	if next, err := f.api.Failover(t.Context(), s.ID, on, on, harness.FailedRejected, detail); err != nil || next != on {
		t.Fatalf("an allow that names no model: %+v, %v", next, err)
	}
}

// TestAFailoverOfACallWrittenAsTextSaysWhy: a model that wrote a tool call
// as text, again after a reminder, asks the same session.update with
// failed_reason tool_as_text and the tool in failed_detail (spec 062), and
// moves the turn to the model the allow names.
func TestAFailoverOfACallWrittenAsTextSaysWhy(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("quick", "name: "+quick)
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionCreate {
			return haiku
		}
		return sonnet
	})
	s := f.create("alice", "quick")
	on := session.ModelRef{Name: haiku, Via: quick}
	const detail = "a call of question was written as text, again after a reminder"
	next, err := f.api.Failover(t.Context(), s.ID, on, on, harness.FailedToolAsText, detail)
	if err != nil || next != (session.ModelRef{Name: sonnet, Via: quick}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	asked := r.last(t, authorizer.ActionSessionUpdate)
	want := `{"agent":"AGENT","current_model":"claude-haiku-4-5","current_model_via":"tier/quick","failed_detail":"a call of question was written as text, again after a reminder","failed_model":"claude-haiku-4-5","failed_reason":"tool_as_text","id":"SESSION","kind":"session","model":"tier/quick","owner":"https://login.example|alice","runner":"hosted","session_id":"SESSION"}`
	if got := wire(t, asked, s.ID, "SESSION", s.Agent.ID, "AGENT"); got != want {
		t.Fatalf("session.update asked about\n%s, want\n%s", got, want)
	}
}

// TestAFailoverPassesOnAModelNamedThatCouldNotBeConnected: a turn whose
// runner could not connect the model an earlier answer named asks again
// standing on the model the session runs, with the model named as
// failed_model and why it could not be connected as failed_detail, and is
// answered the model the next allow names; an allow that names the failed
// model again moves nothing.
func TestAFailoverPassesOnAModelNamedThatCouldNotBeConnected(t *testing.T) {
	const opus = "anthropic/claude-opus-4-5"
	f, _ := checking(t)
	f.applyModel("quick", "name: "+quick)
	answer := opus
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionCreate {
			return haiku
		}
		return answer
	})
	s := f.create("alice", "quick")
	on, named := session.ModelRef{Name: haiku, Via: quick}, session.ModelRef{Name: sonnet, Via: quick}
	const why = sonnet + " was named and could not be connected: models: model_unknown"
	next, err := f.api.Failover(t.Context(), s.ID, on, named, "", why)
	if err != nil || next != (session.ModelRef{Name: opus, Via: quick}) {
		t.Fatalf("the failover answered %+v, %v", next, err)
	}
	asked := r.last(t, authorizer.ActionSessionUpdate)
	if asked.Resource.String("current_model") != haiku || asked.Resource.String("current_model_via") != quick || asked.Resource.String("model") != quick ||
		asked.Resource.String("failed_model") != sonnet || asked.Resource.String("failed_detail") != why {
		t.Fatalf("session.update asked about %v", asked.Resource.Fields)
	}
	answer = sonnet
	if next, err := f.api.Failover(t.Context(), s.ID, on, named, "", why); err != nil || next != on {
		t.Fatalf("an allow of the failed model again: %+v, %v", next, err)
	}
}

// TestAFailoverThatNamesNoOtherModelMovesNothing: an allow that names no
// model or the failed one answers the model the session stands on; a turn
// that stands on a model the header does not name, a failed model of
// another routed name, and a session on a model named itself, ask
// nothing; a deny, and a model the installation does not run, are errors.
func TestAFailoverThatNamesNoOtherModelMovesNothing(t *testing.T) {
	f, _ := checking(t)
	f.applyModel("quick", "name: "+quick)
	answer := haiku
	r := f.route(func(req authz.Request) string {
		if req.Action == authorizer.ActionSessionCreate {
			return haiku
		}
		return answer
	})
	s := f.create("alice", "quick")
	standing := session.ModelRef{Name: haiku, Via: quick}
	for _, a := range []string{haiku, ""} {
		answer = a
		if next, err := f.api.Failover(t.Context(), s.ID, standing, standing, "", ""); err != nil || next != standing {
			t.Fatalf("an allow of %q answered %+v, %v", a, next, err)
		}
	}
	asks := len(r.asked)
	elsewhere := session.ModelRef{Name: sonnet, Via: quick}
	if next, err := f.api.Failover(t.Context(), s.ID, elsewhere, elsewhere, "", ""); err != nil || next != standing || len(r.asked) != asks {
		t.Fatalf("a turn on a model the session does not stand on: %+v, %v, %d questions", next, err, len(r.asked)-asks)
	}
	if next, err := f.api.Failover(t.Context(), s.ID, standing, session.ModelRef{Name: sonnet, Via: "tier/thorough"}, "", ""); err != nil || next != standing || len(r.asked) != asks {
		t.Fatalf("a failed model of another routed name: %+v, %v, %d questions", next, err, len(r.asked)-asks)
	}
	f.apply("alice", "reviewer", "Review.")
	plain := f.create("alice", "reviewer")
	if next, err := f.api.Failover(t.Context(), plain.ID, session.ModelRef{Name: haiku}, session.ModelRef{Name: haiku}, "", ""); err != nil || next.Via != "" || len(r.asked) != asks+1 {
		t.Fatalf("a session on a model named itself: %+v, %v", next, err)
	}

	answer = "no-such-model"
	if _, err := f.api.Failover(t.Context(), s.ID, standing, standing, "", ""); err == nil {
		t.Fatal("a model the installation does not run was answered")
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		return authz.Decision{Reason: "model_tier_quick_unavailable"}, nil
	}
	if _, err := f.api.Failover(t.Context(), s.ID, standing, standing, "", ""); err == nil {
		t.Fatal("a deny was answered")
	}
}

// TestAnOrganizationsSessionFailsOverInItsContext: the failover of a
// session of an organization's agent is asked in the organization's
// context, the org_id its initiator's create named.
func TestAnOrganizationsSessionFailsOverInItsContext(t *testing.T) {
	f := newOrgFixture(t)
	f.apply("carol@acme", "reviewer", "Review for acme.")
	a := f.do(http.MethodPost, "/v1/sessions", "carol@acme", `{"agent":"reviewer"}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var s session.Session
	a.decode(t, &s)
	on := session.ModelRef{Name: haiku, Via: quick}
	changed, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: s.Initiator, Old: session.ModelRef{Name: haiku}, New: on}, s.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.api.appendBatch(t.Context(), s.ID, []session.Event{changed}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.api.Failover(t.Context(), s.ID, on, on, "", strings.Repeat("é", models.MaxDetail)); err != nil {
		t.Fatal(err)
	}
	asked := f.last(authorizer.ActionSessionUpdate)
	if org, _ := asked.Claims["org_id"].(string); org != "acme" || asked.Subject != issuer+"|carol" || asked.Resource.String("failed_model") != haiku {
		t.Fatalf("the failover asked as %s in %v about %v", asked.Subject, asked.Claims, asked.Resource.Fields)
	}
	// The gateway's detail is passed on cut at models.MaxDetail bytes, on a
	// character's boundary.
	if d := asked.Resource.String("failed_detail"); len(d) != models.MaxDetail || !utf8.ValidString(d) {
		t.Fatalf("failed_detail is %d bytes, valid %v", len(d), utf8.ValidString(d))
	}
}
