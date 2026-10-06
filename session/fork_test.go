// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/session/storetest"
)

// forkLog is a log from statuses, each a session.status of the
// session's own thread but those of a thread, sequenced from 1.
func forkLog(t *testing.T, statuses ...forkStatus) []session.Event {
	t.Helper()
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	evs := make([]session.Event, len(statuses))
	for i, s := range statuses {
		evs[i] = storetest.Status(t, s.status, s.reason, at.Add(time.Duration(i)*time.Second))
		evs[i].Thread = s.thread
	}
	session.Stamp(session.NewID(session.PrefixSession), 0, evs)
	return evs
}

type forkStatus struct {
	status session.Status
	reason session.StopReason
	thread string
}

var (
	running     = forkStatus{status: session.StatusRunning}
	idle        = forkStatus{status: session.StatusIdle, reason: session.StopEndTurn}
	interrupted = forkStatus{status: session.StatusIdle, reason: session.StopInterrupted}
	failedTurn  = forkStatus{status: session.StatusIdle, reason: session.StopError}
	completed   = forkStatus{status: session.StatusEnded, reason: session.StopCompleted}
	canceled    = forkStatus{status: session.StatusEnded, reason: session.StopCanceled}
	expired     = forkStatus{status: session.StatusEnded, reason: session.StopExpired}
	failed      = forkStatus{status: session.StatusEnded, reason: session.StopFailed}
)

func limited(r session.StopReason) forkStatus {
	return forkStatus{status: session.StatusIdle, reason: r}
}

func onThread(s forkStatus) forkStatus {
	s.thread = "thr_reviewer"
	return s
}

// TestForkPointOfEachEnd holds spec 017's table of ends: the default
// fork point of each log, and whether the end itself is a fork point.
func TestForkPointOfEachEnd(t *testing.T) {
	for _, c := range []struct {
		name string
		log  []forkStatus
		// want is the default fork point, 0 for none.
		want uint64
		// end is whether the log's last event is a fork point.
		end bool
	}{
		{"an end that closed its turn", []forkStatus{running, completed}, 2, true},
		{"an end that closed a later turn", []forkStatus{running, idle, running, completed}, 4, true},
		{"an end that closed a turn whose thread went idle", []forkStatus{running, onThread(running), onThread(idle), completed}, 4, true},
		{"completed by the end route", []forkStatus{running, idle, completed}, 2, false},
		{"canceled by the end route", []forkStatus{running, idle, canceled}, 2, false},
		{"expired by the reaper", []forkStatus{running, idle, expired}, 2, false},
		{"failed mid-turn", []forkStatus{running, failed}, 0, false},
		{"canceled mid-turn", []forkStatus{running, canceled}, 0, false},
		{"expired mid-turn", []forkStatus{running, expired}, 0, false},
		{"failed in a later turn", []forkStatus{running, idle, running, failed}, 2, false},
		{"interrupted, then canceled", []forkStatus{running, interrupted, canceled}, 2, false},
		{"an error, then expired", []forkStatus{running, failedTurn, expired}, 2, false},
		{"on its budget, then expired", []forkStatus{running, limited(session.StopBudget), expired}, 2, false},
		{"on its turn limit, then expired", []forkStatus{running, limited(session.StopTurnLimit), expired}, 2, false},
		{"on its output limit, then completed", []forkStatus{running, limited(session.StopOutputLimit), completed}, 2, false},
		{"a thread's end", []forkStatus{running, onThread(running), onThread(completed)}, 0, false},
		{"still running", []forkStatus{running}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs := forkLog(t, c.log...)
			got, err := session.ForkPoint(evs, nil)
			switch {
			case c.want == 0 && !errors.Is(err, session.ErrInvalidForkPoint):
				t.Fatalf("the default fork point is %d, %v; want invalid_fork_point", got, err)
			case c.want != 0 && (err != nil || got != c.want):
				t.Fatalf("the default fork point is %d, %v; want %d", got, err, c.want)
			}
			last := uint64(len(evs))
			got, err = session.ForkPoint(evs, &last)
			if c.end != (err == nil) || err != nil && !errors.Is(err, session.ErrInvalidForkPoint) || err == nil && got != last {
				t.Fatalf("a fork at the end, seq %d: %d, %v; a fork point: %v", last, got, err, c.end)
			}
		})
	}
}

// TestAForkRestatesTheEndOfItsTurn: a fork at an end that closed its
// turn copies the end as idle end_turn, its id, time, turn, checkpoint
// and fields a later schema adds kept, and waits for its next message;
// every other event is copied verbatim and the parent's log is left as
// it was.
func TestAForkRestatesTheEndOfItsTurn(t *testing.T) {
	ctx := t.Context()
	st := session.NewMemoryStore()
	parent := storetest.NewSession()
	parent.ID = session.NewID(session.PrefixSession)
	parent.EndOnIdle = true
	if err := st.Create(ctx, parent, nil); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	end, err := session.NewEvent(session.TypeSessionStatus, json.RawMessage(
		`{"status":"ended","stop_reason":"completed","checkpoint":{"ref":"refs/topos/checkpoints/x/1","commit":"0000000000000000000000000000000000000001"},"later":{"kept":true}}`), at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{
		storetest.Message(t, "Review main.go.", at),
		storetest.Status(t, session.StatusRunning, "", at.Add(time.Second)),
		end,
	}
	for i := range evs {
		evs[i].Turn = 1
	}
	session.Stamp(parent.ID, 0, evs)
	if _, err := st.Append(ctx, parent.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	logged, err := st.Events(ctx, parent.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := session.ForkPoint(logged, nil)
	if err != nil || seq != 3 {
		t.Fatalf("the fork point is %d, %v; want the end at 3", seq, err)
	}
	copied := slices.Clone(logged[:seq])
	child := storetest.NewSession()
	child.ID = session.NewID(session.PrefixSession)
	child, err = session.Fork(ctx, st, child, nil, parent, copied)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn || child.LastSeq != seq || child.Turn != 1 || child.Parent.Seq != seq {
		t.Fatalf("the fork is %s %s at %d, turn %d, parent %+v", child.Status, child.StopReason, child.LastSeq, child.Turn, child.Parent)
	}
	for i := range copied {
		if !session.SameEvent(copied[i], logged[i]) {
			t.Fatalf("the fork changed the parent's event %d", i+1)
		}
	}
	got, err := st.Events(ctx, child.ID, 1, 0)
	if err != nil || len(got) != int(seq) {
		t.Fatalf("the fork holds %d events, %v", len(got), err)
	}
	for i, e := range got[:seq-1] {
		want := logged[i]
		want.SessionID = child.ID
		if !session.SameEvent(e, want) {
			t.Fatalf("event %d: %+v, want the parent's %+v", i+1, e, logged[i])
		}
	}
	last := got[seq-1]
	if last.ID != end.ID || !last.Time.Equal(end.Time) || last.Turn != 1 || last.Type != session.TypeSessionStatus || last.Thread != "" {
		t.Fatalf("the restated end: %+v, want the parent's %+v", last, logged[seq-1])
	}
	var p struct {
		session.SessionStatus
		Later json.RawMessage `json:"later"`
	}
	if err := json.Unmarshal(last.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Status != session.StatusIdle || p.StopReason != session.StopEndTurn || p.Checkpoint == nil || p.Checkpoint.Commit != "0000000000000000000000000000000000000001" || string(p.Later) != `{"kept":true}` {
		t.Fatalf("the restated end reads %s", last.Payload)
	}
	parentNow, err := st.Get(ctx, parent.ID)
	if err != nil || parentNow.Status != session.StatusEnded || parentNow.StopReason != session.StopCompleted {
		t.Fatalf("the parent is %+v, %v", parentNow, err)
	}
}

// edited is a log of two turns of a person's messages, each answered,
// with a model change the person made between the first turn's end and
// the second message, a message steered into the second turn, a
// trigger's message, a service's, and a subagent thread's: the events
// ForkBefore reads a fork point from.
func edited(t *testing.T) []session.Event {
	t.Helper()
	at := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	message := func(text, kind, thread string) session.Event {
		e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{
			Sender: session.Sender{Subject: "usr_1", Kind: kind}, Content: []lux.Block{{Type: ir.BlockText, Text: text}},
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Thread = thread
		return e
	}
	changed, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{
		By: session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, Old: session.ModelRef{Name: "small"}, New: session.ModelRef{Name: "large"},
	}, at)
	if err != nil {
		t.Fatal(err)
	}
	cost := int64(700)
	request, err := session.NewEvent(session.TypeModelRequest, session.ModelRequest{Model: "small", CostUSDMicro: &cost}, at)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{
		message("Plan it for three people.", session.SenderPerson, ""), // 1
		storetest.Status(t, session.StatusRunning, "", at),             // 2
		request, // 3
		storetest.Status(t, session.StatusIdle, session.StopEndTurn, at), // 4
		changed, // 5
		message("And the budget?", session.SenderPerson, ""),               // 6
		storetest.Status(t, session.StatusRunning, "", at),                 // 7
		message("In euros.", session.SenderPerson, ""),                     // 8
		message("Look up prices.", session.SenderPerson, "thr_researcher"), // 9
		storetest.Status(t, session.StatusIdle, session.StopEndTurn, at),   // 10
		message("Weekly report.", session.SenderTrigger, ""),               // 11
		storetest.Status(t, session.StatusRunning, "", at),                 // 12
		storetest.Status(t, session.StatusIdle, session.StopEndTurn, at),   // 13
		message("A service's note.", session.SenderService, ""),            // 14
	}
	session.Stamp(session.NewID(session.PrefixSession), 0, evs)
	return evs
}

// TestForkBeforeAMessage: a person's message that opened a turn is a
// fork point, the copy ending just before it with whatever lies between
// the turn's end and the message, a model change included; the opening
// message copies nothing.
func TestForkBeforeAMessage(t *testing.T) {
	evs := edited(t)
	for _, c := range []struct {
		before, want uint64
	}{{1, 0}, {6, 5}} {
		got, err := session.ForkBefore(evs, c.before)
		if err != nil || got != c.want {
			t.Errorf("ForkBefore(%d) = %d, %v; want %d", c.before, got, err, c.want)
		}
	}
}

// TestBeforeSeqMustOpenATurn: a message steered into a running turn, a
// thread's message, a trigger's, a service's, a redacted one, an event
// that is no message, and a sequence outside the log are no fork point.
func TestBeforeSeqMustOpenATurn(t *testing.T) {
	evs := edited(t)
	redacted := slices.Clone(evs)
	tomb, _, err := session.Tombstone(redacted[5], uint64(len(redacted)), session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	redacted[5] = tomb
	for _, c := range []struct {
		name   string
		log    []session.Event
		before uint64
	}{
		{"none", evs, 0},
		{"past the log", evs, uint64(len(evs)) + 1},
		{"a status", evs, 4},
		{"a model change", evs, 5},
		{"a message steered into a turn", evs, 8},
		{"a thread's message", evs, 9},
		{"a trigger's message", evs, 11},
		{"a service's message", evs, 14},
		{"a redacted message", redacted, 6},
	} {
		if got, err := session.ForkBefore(c.log, c.before); !errors.Is(err, session.ErrInvalidForkPoint) {
			t.Errorf("%s, before_seq %d: %d, %v; want invalid_fork_point", c.name, c.before, got, err)
		}
	}
}

// TestRootOfAFork: a fork's root is its parent's id when the parent is
// no fork, and its parent's root when it is, so a fork of a fork stays
// in the first session's tree, unless it names itself its root and
// starts a tree of its own; its carried spend is the copy's, and a
// message it starts with lands in the same append after the copy.
func TestRootOfAFork(t *testing.T) {
	ctx := t.Context()
	st := session.NewMemoryStore()
	evs := edited(t)
	top := storetest.NewSession()
	if err := st.Create(ctx, top, nil); err != nil {
		t.Fatal(err)
	}
	session.Stamp(top.ID, 0, evs)
	if _, err := st.Append(ctx, top.ID, 0, evs); err != nil {
		t.Fatal(err)
	}
	if top.Root != "" || top.TreeRoot() != top.ID {
		t.Fatalf("a session no fork made has root %q, tree %q", top.Root, top.TreeRoot())
	}
	replacement, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{
		Sender: session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, Content: []lux.Block{{Type: ir.BlockText, Text: "And the budget for four?"}},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	child, err := session.Fork(ctx, st, storetest.NewSession(), nil, top, evs[:5], replacement)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case child.Root != top.ID || child.Parent.SessionID != top.ID || child.Parent.Seq != 5:
		t.Fatalf("the fork's root %q, parent %+v", child.Root, child.Parent)
	case child.LastSeq != 6 || child.Budget.CarriedCostUSDMicro != 700 || child.Budget.SpentCostUSDMicro != 700:
		t.Fatalf("the fork is at %d, carried %d, spent %d", child.LastSeq, child.Budget.CarriedCostUSDMicro, child.Budget.SpentCostUSDMicro)
	case child.Model == nil || child.Model.Name != "large" || child.Status != session.StatusIdle || child.StopReason != session.StopEndTurn:
		t.Fatalf("the fork stands on %+v, %s %s", child.Model, child.Status, child.StopReason)
	}
	got, err := st.Events(ctx, child.ID, 6, 0)
	if err != nil || len(got) != 1 || got[0].ID != replacement.ID || got[0].SessionID != child.ID {
		t.Fatalf("the fork's message: %+v, %v", got, err)
	}
	// A fork that names itself its root starts a tree of its own, its
	// parent kept as its lineage.
	own := storetest.NewSession()
	own.Root = own.ID
	own, err = session.Fork(ctx, st, own, nil, child, evs[:5])
	if err != nil {
		t.Fatal(err)
	}
	if own.Root != own.ID || own.TreeRoot() != own.ID || own.Parent.SessionID != child.ID || own.Parent.Seq != 5 {
		t.Fatalf("a fork that started a tree: root %q, parent %+v", own.Root, own.Parent)
	}
	stray := storetest.NewSession()
	stray.Root = session.NewID(session.PrefixSession)
	if stray, err = session.Fork(ctx, st, stray, nil, child, nil); err != nil || stray.Root != top.ID {
		t.Fatalf("a fork that names another session its root joins %q, %v; want its parent's tree", stray.Root, err)
	}
	grandchild, err := session.Fork(ctx, st, storetest.NewSession(), nil, child, nil)
	if err != nil {
		t.Fatal(err)
	}
	if grandchild.Root != top.ID || grandchild.Parent.SessionID != child.ID || grandchild.Parent.Seq != 0 || grandchild.LastSeq != 0 ||
		grandchild.Status != session.StatusIdle || grandchild.StopReason != "" || grandchild.Turn != 0 || grandchild.Budget.CarriedCostUSDMicro != 0 {
		t.Fatalf("a fork of the fork that copies nothing: %+v", grandchild)
	}
}
