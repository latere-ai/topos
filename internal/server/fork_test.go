// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
)

// appendTo appends events to a session's log after its last one, each
// on turn, and answers the last sequence.
func (f *fixture) appendTo(id string, turn int, payloads ...any) uint64 {
	f.t.Helper()
	ctx := f.t.Context()
	s, err := f.sessions.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	var batch []session.Event
	for _, p := range payloads {
		var typ session.Type
		switch p.(type) {
		case session.SessionStatus:
			typ = session.TypeSessionStatus
		case session.AgentMessage:
			typ = session.TypeAgentMessage
		case session.ModelRequest:
			typ = session.TypeModelRequest
		case session.SessionResumed:
			typ = session.TypeSessionResumed
		case session.UserMessage:
			typ = session.TypeUserMessage
		default:
			f.t.Fatalf("no event type for %T", p)
		}
		e, err := session.NewEvent(typ, p, time.Now())
		if err != nil {
			f.t.Fatal(err)
		}
		e.Turn = turn
		batch = append(batch, e)
	}
	last := session.Stamp(id, s.LastSeq, batch)
	if _, err := f.sessions.Append(ctx, id, s.LastSeq, batch); err != nil {
		f.t.Fatal(err)
	}
	return last
}

// turn appends one finished turn of the session's own thread: the agent
// answered text at cost and the turn went idle with a checkpoint. It
// answers the turn's boundary, the sequence of its idle status.
func (f *fixture) turn(id string, n int, text string, cost int64) uint64 {
	f.t.Helper()
	return f.appendTo(id, n,
		session.SessionStatus{Status: session.StatusRunning},
		session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: text}}}, StopReason: ir.StopEndTurn},
		session.ModelRequest{Model: "claude-haiku-4-5", CostUSDMicro: &cost},
		session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn,
			Checkpoint: &session.CheckpointRef{Ref: "refs/topos/checkpoints/" + id + "/" + strconv.Itoa(n), Commit: fmt.Sprintf("%040d", n)}},
	)
}

// endedTurn appends one turn of the session's own thread that the
// session's end closed, as a runner writes it for a session created
// with end_on_idle: the agent answered text at cost and the session
// ended completed with the turn's checkpoint. It answers the end's
// sequence.
func (f *fixture) endedTurn(id string, n int, text string, cost int64) uint64 {
	f.t.Helper()
	return f.appendTo(id, n,
		session.SessionStatus{Status: session.StatusRunning},
		session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: text}}}, StopReason: ir.StopEndTurn},
		session.ModelRequest{Model: "claude-haiku-4-5", CostUSDMicro: &cost},
		session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted,
			Checkpoint: &session.CheckpointRef{Ref: "refs/topos/checkpoints/" + id + "/" + strconv.Itoa(n), Commit: fmt.Sprintf("%040d", n)}},
	)
}

// questions keeps every authorizer question of one action.
type questions struct {
	mu   sync.Mutex
	reqs []authz.Request
}

// keep records the questions of action through f's authorizer, which
// still answers them as before.
func (q *questions) keep(f *fixture, action string) {
	next := f.authz.next
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == action {
			q.mu.Lock()
			q.reqs = append(q.reqs, req)
			q.mu.Unlock()
		}
		return next.Authorize(context.Background(), req)
	}
}

func (q *questions) last(t *testing.T) authz.Request {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.reqs) == 0 {
		t.Fatal("the action was never asked")
	}
	return q.reqs[len(q.reqs)-1]
}

// TestForkContinuesAnEndedSession: an expired session forks at its last
// turn boundary into a new session of the same agent version, with the
// parent link, the log to the fork point copied, the forker as initiator,
// a lifetime from now, the copied spend and its own budget; the fork asks
// session.read and then session.fork with the create's fields and the
// parent's.
func TestForkContinuesAnEndedSession(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	clock := now
	f := newFixture(t, func(o *Options) { o.Now = func() time.Time { return clock } })
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "One finding.", 1200)
	raised := int64(999_999_999)
	f.appendTo(parent.ID, 1, session.SessionResumed{By: parent.Initiator, Reason: "budget_raised", MaxCostUSDMicro: &raised})
	f.appendTo(parent.ID, 2, session.UserMessage{Sender: parent.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "And main_test.go?"}}})
	boundary := f.turn(parent.ID, 2, "Two findings.", 800)
	f.appendTo(parent.ID, 2, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopExpired})

	var fork questions
	fork.keep(f, authorizer.ActionSessionFork)
	clock = now.Add(48 * time.Hour)
	f.authz.take()
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", "")
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	if got := f.authz.take(); !slices.Equal(got, []string{authorizer.ActionSessionRead, authorizer.ActionSessionFork}) {
		t.Fatalf("the fork asked %v", got)
	}
	var child session.Session
	a.decode(t, &child)
	switch {
	case child.ID == parent.ID || child.Parent == nil || *child.Parent != (session.Parent{SessionID: parent.ID, Seq: boundary}):
		t.Fatalf("parent %+v of %s", child.Parent, child.ID)
	case child.Agent != parent.Agent:
		t.Fatalf("agent %+v, want the parent's %+v", child.Agent, parent.Agent)
	case child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn || child.LastSeq != boundary || child.Turn != 2:
		t.Fatalf("the fork is %s %s at %d, turn %d", child.Status, child.StopReason, child.LastSeq, child.Turn)
	case child.Initiator.Subject != alice || child.Title != continuedTitle(parent.Title) || !slices.Equal(child.Resources, parent.Resources):
		t.Fatalf("initiator %+v, title %q, resources %+v", child.Initiator, child.Title, child.Resources)
	case !child.CreatedAt.Equal(clock) || !child.ExpiresAt.After(clock) || !child.ExpiresAt.After(parent.ExpiresAt):
		t.Fatalf("created %v, expires %v; a fork's lifetime runs from now", child.CreatedAt, child.ExpiresAt)
	case child.Budget.SpentCostUSDMicro != 2000:
		t.Fatalf("spent %d, want the copied 2000", child.Budget.SpentCostUSDMicro)
	case child.Budget.MaxCostUSDMicro != nil && *child.Budget.MaxCostUSDMicro == raised:
		t.Fatal("the copied session.resumed set the fork's budget")
	}
	q := fork.last(t)
	if q.Resource.ID != parent.ID || q.Resource.String("parent") != parent.ID || q.Resource.String("session_id") != child.ID ||
		q.Resource.String("owner") != alice || q.Resource.String("initiator") != alice || q.Resource.String("agent_owner") != alice {
		t.Fatalf("session.fork asked about %s with %v", q.Resource.ID, q.Resource.Fields)
	}
	if seq := fmt.Sprint(q.Resource.Fields["seq"]); seq != strconv.FormatUint(boundary, 10) {
		t.Fatalf("session.fork's seq is %v, want %d", q.Resource.Fields["seq"], boundary)
	}
	if _, ok := q.Resource.Fields["permissions"]; !ok {
		t.Fatal("session.fork carries no permissions for the initiator cap")
	}

	pe, err := f.sessions.Events(t.Context(), parent.ID, 1, int(boundary))
	if err != nil {
		t.Fatal(err)
	}
	ce, err := f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ce) != len(pe) {
		t.Fatalf("the fork holds %d events, the parent %d to the fork point", len(ce), len(pe))
	}
	for i := range pe {
		want := pe[i]
		want.SessionID = child.ID
		if !session.SameEvent(ce[i], want) {
			t.Fatalf("event %d: %+v, want the parent's %+v", i+1, ce[i], pe[i])
		}
	}
	// The agent's bundle is the fork's own blob, so a runner rebuilds the
	// agent from the fork alone.
	if a := f.do(http.MethodGet, "/v1/sessions/"+child.ID+"/blobs/"+string(child.Agent.Bundle), "alice", ""); a.status != http.StatusOK {
		t.Fatalf("the fork's bundle: %d %s", a.status, a.body)
	}

	// An explicit fork point at the first turn's boundary.
	first := uint64(5)
	a = f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", `{"at_seq":`+strconv.FormatUint(first, 10)+`}`)
	if a.status != http.StatusCreated {
		t.Fatalf("fork at %d: %d %s", first, a.status, a.body)
	}
	var early session.Session
	a.decode(t, &early)
	if early.Parent.Seq != first || early.LastSeq != first || early.Budget.SpentCostUSDMicro != 1200 || early.Turn != 1 {
		t.Fatalf("a fork at %d: %+v", first, early)
	}
}

// TestAForksFirstTurnSeesTheHistory: the fork's log folds as its
// parent's did at the fork point, so the next message reaches a model
// with the whole conversation before it.
func TestAForksFirstTurnSeesTheHistory(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	boundary := f.turn(parent.ID, 1, "main.go looks fine.", 100)
	f.appendTo(parent.ID, 1, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopExpired})
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", "{}")
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var child session.Session
	a.decode(t, &child)
	pe, err := f.sessions.Events(t.Context(), parent.ID, 1, int(boundary))
	if err != nil {
		t.Fatal(err)
	}
	ce, err := f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	fold := func(evs []session.Event) string {
		tr, err := session.Fold(evs, "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if fold(ce) != fold(pe) {
		t.Fatalf("the fork folds to\n%s\nthe parent at the fork point to\n%s", fold(ce), fold(pe))
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+child.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Now main_test.go."}]}}`); a.status != http.StatusOK {
		t.Fatalf("send to the fork: %d %s", a.status, a.body)
	}
	ce, err = f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := session.Fold(ce, "")
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, m := range tr.Messages {
		for _, b := range m.Blocks {
			said = append(said, b.Text)
		}
	}
	if !slices.Contains(said, "Review main.go.") || !slices.Contains(said, "main.go looks fine.") || said[len(said)-1] != "Now main_test.go." {
		t.Fatalf("the fork's next turn reads %q", said)
	}
}

// TestForkContinuesASessionThatEndedWithItsTurn: a session created with
// end_on_idle, whose turn's end ended it, forks at that end: the fork is
// idle end_turn at the end's sequence with the end's checkpoint, carries
// no end_on_idle, and its next turn reaches a model with the whole
// conversation before it.
func TestForkContinuesASessionThatEndedWithItsTurn(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","message":"Review main.go.","end_on_idle":true}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var parent session.Session
	a.decode(t, &parent)
	end := f.endedTurn(parent.ID, 1, "main.go looks fine.", 300)
	if p, err := f.sessions.Get(t.Context(), parent.ID); err != nil || p.Status != session.StatusEnded || p.StopReason != session.StopCompleted {
		t.Fatalf("the parent is %+v, %v", p, err)
	}

	a = f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", "")
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var child session.Session
	a.decode(t, &child)
	switch {
	case child.Parent == nil || *child.Parent != (session.Parent{SessionID: parent.ID, Seq: end}):
		t.Fatalf("parent %+v, want the end at %d", child.Parent, end)
	case child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn || child.LastSeq != end || child.Turn != 1:
		t.Fatalf("the fork is %s %s at %d, turn %d", child.Status, child.StopReason, child.LastSeq, child.Turn)
	case child.EndOnIdle:
		t.Fatal("the fork carries end_on_idle")
	case child.Budget.SpentCostUSDMicro != 300:
		t.Fatalf("spent %d, want the copied 300", child.Budget.SpentCostUSDMicro)
	}
	if b := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", `{"at_seq":`+strconv.FormatUint(end, 10)+`}`); b.status != http.StatusCreated {
		t.Fatalf("a fork at the end, %d: %d %s", end, b.status, b.body)
	}

	pe, err := f.sessions.Events(t.Context(), parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	ce, err := f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ce) != len(pe) {
		t.Fatalf("the fork holds %d events, the parent %d", len(ce), len(pe))
	}
	var st session.SessionStatus
	if err := ce[end-1].Decode(&st); err != nil || ce[end-1].ID != pe[end-1].ID || st.Status != session.StatusIdle || st.StopReason != session.StopEndTurn ||
		st.Checkpoint == nil || st.Checkpoint.Commit != fmt.Sprintf("%040d", 1) {
		t.Fatalf("the fork's copy of the end: %s, %v", ce[end-1].Payload, err)
	}
	fold := func(evs []session.Event) string {
		tr, err := session.Fold(evs, "")
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(tr)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if fold(ce) != fold(pe) {
		t.Fatalf("the fork folds to\n%s\nthe parent at the fork point to\n%s", fold(ce), fold(pe))
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+child.ID+"/events", "alice", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Now main_test.go."}]}}`); a.status != http.StatusOK {
		t.Fatalf("send to the fork: %d %s", a.status, a.body)
	}
	ce, err = f.sessions.Events(t.Context(), child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := session.Fold(ce, "")
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	for _, m := range tr.Messages {
		for _, b := range m.Blocks {
			said = append(said, b.Text)
		}
	}
	if !slices.Equal(said, []string{"Review main.go.", "main.go looks fine.", "Now main_test.go."}) {
		t.Fatalf("the fork's next turn reads %q", said)
	}
}

// TestForkPointMustBeATurnBoundary: a sequence that is not a session's
// own idle status, one past the log, a session that never finished a
// turn, the end route's end of an idle session, and an end failed,
// canceled or expired straight after running are invalid_fork_point,
// and nothing is created.
func TestForkPointMustBeATurnBoundary(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	fresh := f.create("alice", "reviewer")
	if a := f.do(http.MethodPost, "/v1/sessions/"+fresh.ID+"/fork", "alice", ""); a.status != http.StatusUnprocessableEntity || a.code() != CodeInvalidForkPoint {
		t.Fatalf("a session with no turn: %d %s", a.status, a.body)
	}
	s := f.create("alice", "reviewer")
	f.turn(s.ID, 1, "Done.", 1)
	threadIdle, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	threadIdle.Thread = "thr_reviewer"
	cur, err := f.sessions.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{threadIdle}
	last := session.Stamp(s.ID, cur.LastSeq, batch)
	if _, err := f.sessions.Append(t.Context(), s.ID, cur.LastSeq, batch); err != nil {
		t.Fatal(err)
	}
	for _, at := range []uint64{0, 1, 2, last, last + 1} {
		a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", fmt.Sprintf(`{"at_seq":%d}`, at))
		if a.status != http.StatusUnprocessableEntity || a.code() != CodeInvalidForkPoint {
			t.Errorf("at_seq %d: %d %s", at, a.status, a.body)
		}
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+s.ID+"/fork", "alice", `{"at":5}`); a.code() != CodeInvalidRequest {
		t.Fatalf("an unknown member: %d %s", a.status, a.body)
	}

	// The end route's end closes no turn: the idle before it is the
	// boundary, and the end is none.
	closed := f.create("alice", "reviewer")
	f.turn(closed.ID, 1, "Done.", 1)
	if a := f.do(http.MethodPost, "/v1/sessions/"+closed.ID+"/end", "alice", `{"reason":"completed"}`); a.status != http.StatusOK {
		t.Fatalf("end: %d %s", a.status, a.body)
	}
	ended, err := f.sessions.Get(t.Context(), closed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+closed.ID+"/fork", "alice", fmt.Sprintf(`{"at_seq":%d}`, ended.LastSeq)); a.code() != CodeInvalidForkPoint {
		t.Errorf("at the end route's end, %d: %d %s", ended.LastSeq, a.status, a.body)
	}
	sessions := 3

	// An end straight after running closes a turn that did not finish,
	// but for completed, the end of a finished turn.
	for _, reason := range []session.StopReason{session.StopFailed, session.StopCanceled, session.StopExpired} {
		cut := f.create("alice", "reviewer")
		sessions++
		f.appendTo(cut.ID, 1, session.SessionStatus{Status: session.StatusRunning}, session.SessionStatus{Status: session.StatusEnded, StopReason: reason})
		if a := f.do(http.MethodPost, "/v1/sessions/"+cut.ID+"/fork", "alice", ""); a.status != http.StatusUnprocessableEntity || a.code() != CodeInvalidForkPoint {
			t.Errorf("ended %s straight after running: %d %s", reason, a.status, a.body)
		}
	}
	page, _, err := f.sessions.List(t.Context(), session.ListOptions{})
	if err != nil || len(page) != sessions {
		t.Fatalf("a refused fork created a session: %d, want %d, %v", len(page), sessions, err)
	}
}

// TestForkNeverMerges: what the parent appends after a fork never
// reaches the fork, and the fork's own events never reach the parent.
func TestForkNeverMerges(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	boundary := f.turn(parent.ID, 1, "Done.", 1)
	a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", "")
	if a.status != http.StatusCreated {
		t.Fatalf("fork: %d %s", a.status, a.body)
	}
	var child session.Session
	a.decode(t, &child)
	f.turn(parent.ID, 2, "The parent went on.", 1)
	f.turn(child.ID, 2, "The fork went on.", 1)
	pe, err := f.sessions.Events(t.Context(), parent.ID, boundary+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	ce, err := f.sessions.Events(t.Context(), child.ID, boundary+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pe {
		if slices.ContainsFunc(ce, func(c session.Event) bool { return c.ID == p.ID }) {
			t.Fatalf("the parent's %s %s reached the fork", p.Type, p.ID)
		}
	}
}

// TestAForkIsRefused: a caller who may not read the session hears
// not_found; a denied session.fork is forbidden; a session of an archived
// agent is conflict; none of them creates a session.
func TestAForkIsRefused(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	parent := f.create("alice", "reviewer")
	f.turn(parent.ID, 1, "Done.", 1)
	count := func() int {
		page, _, err := f.sessions.List(t.Context(), session.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return len(page)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "bob", ""); a.status != http.StatusNotFound {
		t.Fatalf("bob forks alice's session: %d %s", a.status, a.body)
	}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if req.Action == authorizer.ActionSessionFork {
			return authz.Decision{Reason: "plan_suspended"}, nil
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ""); a.status != http.StatusForbidden {
		t.Fatalf("a denied fork: %d %s", a.status, a.body)
	}
	f.authz.answer = nil
	if a := f.do(http.MethodPost, "/v1/agents/reviewer/archive", "alice", `{"permanent":true}`); a.status != http.StatusOK {
		t.Fatalf("archive the agent: %d %s", a.status, a.body)
	}
	if a := f.do(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "alice", ""); a.code() != CodeConflict {
		t.Fatalf("a fork of an archived agent's session: %d %s", a.status, a.body)
	}
	if n := count(); n != 1 {
		t.Fatalf("refused forks left %d sessions", n)
	}
}

func TestContinuedTitle(t *testing.T) {
	for _, c := range []struct{ title, want string }{
		{"", ""},
		{"Review main.go", "Review main.go (continued)"},
		{"Review main.go (continued)", "Review main.go (continued 2)"},
		{"Review main.go (continued 2)", "Review main.go (continued 3)"},
		{"Review main.go (continued 41)", "Review main.go (continued 42)"},
		// Only the mark a fork adds counts; other parentheses stay.
		{"Notes (draft)", "Notes (draft) (continued)"},
		{"(continued)", "(continued) (continued)"},
	} {
		if got := continuedTitle(c.title); got != c.want {
			t.Errorf("continuedTitle(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

// TestAForkIsTitledAsAContinuation forks a titled session and then the
// fork, and reads each title back: a list of sessions tells the three
// apart by title alone.
func TestAForkIsTitledAsAContinuation(t *testing.T) {
	f := newFixture(t)
	f.apply("alice", "reviewer", "Review.")
	a := f.do(http.MethodPost, "/v1/sessions", "alice", `{"agent":"reviewer","title":"Review main.go","message":"Review main.go."}`)
	if a.status != http.StatusCreated {
		t.Fatalf("create: %d %s", a.status, a.body)
	}
	var parent session.Session
	a.decode(t, &parent)
	f.turn(parent.ID, 1, "One finding.", 1200)
	want := []string{"Review main.go (continued)", "Review main.go (continued 2)"}
	from := parent.ID
	for i, title := range want {
		r := f.do(http.MethodPost, "/v1/sessions/"+from+"/fork", "alice", "")
		if r.status != http.StatusCreated {
			t.Fatalf("fork %d: %d %s", i+1, r.status, r.body)
		}
		var child session.Session
		r.decode(t, &child)
		if child.Title != title {
			t.Fatalf("fork %d is titled %q, want %q", i+1, child.Title, title)
		}
		from = child.ID
	}
}
