// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// setStatus appends a session.status of status and reason to session
// id's log, as its runner would.
func (f *fixture) setStatus(id string, status session.Status, reason session.StopReason) {
	f.t.Helper()
	s, err := f.sessions.Get(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: status, StopReason: reason}, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	batch := []session.Event{e}
	session.Stamp(id, s.LastSeq, batch)
	if _, err := f.sessions.Append(f.t.Context(), id, s.LastSeq, batch); err != nil {
		f.t.Fatal(err)
	}
}

// summary reads the summary as token with the query q.
func (f *fixture) summary(token, q string) session.Summary {
	f.t.Helper()
	a := f.do(http.MethodGet, "/v1/sessions/summary"+q, token, "")
	if a.status != http.StatusOK {
		f.t.Fatalf("summary %q: %d %s", q, a.status, a.body)
	}
	var out session.Summary
	a.decode(f.t, &out)
	return out
}

// counts is a summary written out.
func counts(running, waiting, idle, ended, agents int) session.Summary {
	return session.Summary{Sessions: session.Counts{Running: running, WaitingForApproval: waiting, Idle: idle, Ended: ended}, Agents: agents}
}

// TestSummarizeSessions: the summary counts the sessions the caller's
// list would answer, by status with the idle sessions waiting for a
// person apart, and the distinct agents among them; it takes the list's
// agent, runner and archived filters, refuses what the list refuses, and
// answers zeros for an agent name the caller holds none of. A status
// filter does not narrow it.
func TestSummarizeSessions(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	f.apply("alice", "writer", "Write.")
	f.apply("alice", "idle", "Wait.")
	f.apply("bob", "helper", "Help.")
	running := f.create("alice", "reviewer")
	f.setStatus(running.ID, session.StatusRunning, "")
	waiting := f.create("alice", "reviewer")
	f.setStatus(waiting.ID, session.StatusIdle, session.StopToolConfirmation)
	idle := f.create("alice", "writer")
	f.setStatus(idle.ID, session.StatusIdle, session.StopEndTurn)
	f.create("alice", "writer")
	f.ended("alice", "writer")
	filed := f.ended("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+filed.ID+"/archive", "alice", ""); a.status != http.StatusOK {
		t.Fatalf("archive: %d %s", a.status, a.body)
	}
	theirs := f.create("bob", "helper")
	f.setStatus(theirs.ID, session.StatusRunning, "")

	for q, want := range map[string]session.Summary{
		"":                           counts(1, 1, 2, 1, 2),
		"?archived=false":            counts(1, 1, 2, 1, 2),
		"?archived=true":             counts(0, 0, 0, 1, 1),
		"?archived=any":              counts(1, 1, 2, 2, 2),
		"?agent=reviewer":            counts(1, 1, 0, 0, 1),
		"?agent=writer&archived=any": counts(0, 0, 2, 1, 1),
		"?agent=idle":                counts(0, 0, 0, 0, 0),
		"?agent=nobody":              counts(0, 0, 0, 0, 0),
		"?runner=hosted":             counts(1, 1, 2, 1, 2),
		"?runner=external":           counts(0, 0, 0, 0, 0),
		"?status=running":            counts(1, 1, 2, 1, 2),
		"?status=gone":               counts(1, 1, 2, 1, 2),
	} {
		if got := f.summary("alice", q); got != want {
			t.Errorf("alice's summary %q: %+v, want %+v", q, got, want)
		}
	}
	if got, want := f.summary("bob", ""), counts(1, 0, 0, 0, 1); got != want {
		t.Errorf("bob's summary: %+v, want %+v", got, want)
	}
	// A summary counts what the list holds, the sessions of its context's
	// agents: the admin's context holds none (spec 036).
	if got, want := f.summary("root", ""), counts(0, 0, 0, 0, 0); got != want {
		t.Errorf("the admin's summary: %+v, want %+v", got, want)
	}
	for _, q := range []string{"?runner=elsewhere", "?archived=maybe"} {
		if a := f.do(http.MethodGet, "/v1/sessions/summary"+q, "alice", ""); a.code() != CodeInvalidRequest {
			t.Errorf("%s: %d %s", q, a.status, a.body)
		}
	}
	if a := f.do(http.MethodGet, "/v1/sessions/summary", "", ""); a.status != http.StatusUnauthorized {
		t.Errorf("without a bearer: %d %s", a.status, a.body)
	}
	// The body is the shape the document states.
	a := f.do(http.MethodGet, "/v1/sessions/summary", "alice", "")
	if want := `{"sessions":{"running":1,"waiting_for_approval":1,"idle":2,"ended":1},"agents":2}`; string(a.body) != want+"\n" && string(a.body) != want {
		t.Errorf("the body %s, want %s", a.body, want)
	}
}

// TestASummaryIsScopedAsTheList: the summary asks session.list alone,
// with the fields the list asks with, applies the owners the decision
// narrows to, counts every session the list pages through under the
// same filters, and is refused where the list is.
func TestASummaryIsScopedAsTheList(t *testing.T) {
	f := newFixture(t)
	reviewer := f.apply("alice", "reviewer", "Review.").Status.ID
	f.apply("bob", "helper", "Help.")
	for range 3 {
		f.create("alice", "reviewer")
	}
	f.ended("alice", "reviewer")
	f.create("bob", "helper")
	// An authorizer that lets bob start a session of alice's agent, which
	// the summary of alice's context counts; bob's own agent's is not.
	f.authz.answer = func(authz.Request) (authz.Decision, error) { return authz.Decision{Allow: true}, nil }
	bobs := f.create("bob", reviewer)
	f.setStatus(bobs.ID, session.StatusIdle, session.StopToolConfirmation)

	var asked []authz.Request
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		asked = append(asked, req)
		// The decision narrows alice's list to bob's sessions.
		return authz.Decision{Allow: true, Filter: &authz.Filter{Owners: []string{authz.Subject(issuer, "bob")}}}, nil
	}
	if got, want := f.summary("alice", "?runner=hosted"), counts(0, 1, 0, 0, 1); got != want {
		t.Fatalf("the narrowed summary: %+v, want %+v", got, want)
	}
	if len(asked) != 1 || asked[0].Action != authorizer.ActionSessionList || asked[0].Resource.Kind != authorizer.KindSession ||
		asked[0].Resource.String("status") != "" || asked[0].Resource.String("runner") != session.RunnerHosted || asked[0].Resource.String("agent_owner") != alice {
		t.Fatalf("the summary asked %+v", asked)
	}
	f.listIDs("alice", "?runner=hosted")
	if len(asked) != 2 || !slices.Equal([]string{asked[0].Action, asked[0].Resource.String("status"), asked[0].Resource.String("runner")},
		[]string{asked[1].Action, asked[1].Resource.String("status"), asked[1].Resource.String("runner")}) {
		t.Fatalf("the list asked %+v, the summary %+v", asked[1], asked[0])
	}

	// Under the owner policy, the summary counts what the list pages
	// through, page by page.
	f.authz.answer = nil
	for _, q := range []string{"", "?archived=any", "?archived=true", "?agent=reviewer"} {
		sep := "?"
		if q != "" {
			sep = "&"
		}
		var listed int
		for cursor := ""; ; {
			a := f.do(http.MethodGet, "/v1/sessions"+q+sep+"limit=1"+cursor, "alice", "")
			var page struct {
				Items      []session.Session `json:"items"`
				NextCursor string            `json:"next_cursor"`
			}
			a.decode(t, &page)
			listed += len(page.Items)
			if page.NextCursor == "" {
				break
			}
			cursor = "&cursor=" + page.NextCursor
		}
		got := f.summary("alice", q).Sessions
		if total := got.Running + got.WaitingForApproval + got.Idle + got.Ended; total != listed {
			t.Errorf("%q: the summary counts %d sessions, the list pages through %d", q, total, listed)
		}
	}

	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		return authz.Decision{Reason: "not_enabled"}, nil
	}
	list, summary := f.do(http.MethodGet, "/v1/sessions", "alice", ""), f.do(http.MethodGet, "/v1/sessions/summary", "alice", "")
	if list.status != http.StatusForbidden || summary.status != list.status || summary.code() != list.code() {
		t.Fatalf("a denied summary: %d %s, the list %d %s", summary.status, summary.body, list.status, list.body)
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		return authz.Decision{}, errors.New("the authorizer is down")
	}
	if a := f.do(http.MethodGet, "/v1/sessions/summary", "alice", ""); a.status < http.StatusInternalServerError {
		t.Fatalf("an authorizer that does not answer: %d %s", a.status, a.body)
	}
}

// listOnly is a store with no counts of its own: the session store's
// methods without Summarize.
type listOnly struct{ session.Store }

// failing is a store whose list fails after its first page.
type failing struct {
	session.Store
	pages int
}

func (s *failing) List(ctx context.Context, o session.ListOptions) ([]session.Session, string, error) {
	if s.pages++; s.pages > 1 {
		return nil, "", errors.New("the store is gone")
	}
	return s.Store.List(ctx, o)
}

// TestASummaryPagesAStoreWithoutCounts: a store that offers no count is
// summarized through its list, every page of it, with the list's filters
// and none of its status, and a failing page fails the summary.
func TestASummaryPagesAStoreWithoutCounts(t *testing.T) {
	st := session.NewMemoryStore()
	if _, ok := st.(session.Summarizer); !ok {
		t.Fatal("the memory store does not count, so this test proves nothing")
	}
	plain := listOnly{st}
	ctx := t.Context()
	agents := []session.AgentRef{
		{ID: session.NewID(session.PrefixAgent), Name: "one", Version: 1},
		{ID: session.NewID(session.PrefixAgent), Name: "two", Version: 1},
	}
	statuses := []struct {
		status session.Status
		reason session.StopReason
	}{
		{session.StatusRunning, ""}, {session.StatusIdle, session.StopToolConfirmation}, {session.StatusIdle, session.StopEndTurn}, {session.StatusEnded, session.StopCompleted},
	}
	n := 2*summarizePage + 3
	for i := range n {
		s := session.New(agents[i%2], session.Sender{Subject: fmt.Sprintf("usr_%d", i%3), Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineHost}, time.Now())
		s.Status, s.StopReason = statuses[i%4].status, statuses[i%4].reason
		if err := st.Create(ctx, s, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []session.ListOptions{
		{},
		{Owners: []string{"usr_0", "usr_2"}},
		{AgentID: agents[1].ID, Status: session.StatusRunning, Limit: 1},
		{Runner: session.RunnerExternal},
	} {
		want, err := st.(session.Summarizer).Summarize(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		got, err := summarize(ctx, plain, o)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%+v: paged %+v, counted %+v", o, got, want)
		}
	}
	if got, err := summarize(ctx, plain, session.ListOptions{}); err != nil || got.Sessions.Running+got.Sessions.WaitingForApproval+got.Sessions.Idle+got.Sessions.Ended != n || got.Agents != 2 {
		t.Fatalf("every session: %+v, %v", got, err)
	}
	if _, err := summarize(ctx, &failing{Store: plain}, session.ListOptions{}); err == nil {
		t.Fatal("a failing page summarized")
	}

	// The route reaches a store without counts the same way.
	f := newFixture(t, func(o *Options) { o.Sessions = listOnly{session.NewMemoryStore()} })
	f.apply("alice", "reviewer", "Review.")
	f.create("alice", "reviewer")
	if got, want := f.summary("alice", ""), counts(0, 0, 1, 0, 1); got != want {
		t.Fatalf("the route over a store without counts: %+v, want %+v", got, want)
	}
	gone := newFixture(t, func(o *Options) { o.Sessions = &failing{Store: listOnly{session.NewMemoryStore()}, pages: 1} })
	gone.apply("alice", "reviewer", "Review.")
	if a := gone.do(http.MethodGet, "/v1/sessions/summary", "alice", ""); a.status != http.StatusInternalServerError {
		t.Fatalf("the route over a failing store: %d %s", a.status, a.body)
	}
}
