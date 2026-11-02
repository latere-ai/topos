// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// refusalLog builds a log of the events in order, each at the next
// sequence.
type refusalLog struct {
	t   *testing.T
	evs []Event
}

func (l *refusalLog) add(typ Type, p any) *refusalLog {
	l.t.Helper()
	e := netEvent(l.t, uint64(len(l.evs)+1), typ, p)
	l.evs = append(l.evs, e)
	return l
}

func (l *refusalLog) on(thread string, typ Type, p any) *refusalLog {
	l.t.Helper()
	l.add(typ, p)
	l.evs[len(l.evs)-1].Subagent = thread
	return l
}

func blocked(id, by, rule string) AgentToolUse {
	return AgentToolUse{ToolUseID: id, Name: "bash", Verdict: "block", DecidedBy: by, DecidedByRule: rule}
}

func ran(id, by string, features ...string) AgentToolUse {
	return AgentToolUse{ToolUseID: id, Name: "bash", Verdict: "allow", DecidedBy: by, Risk: &Risk{Features: features}}
}

func waited(id string) AgentToolUse {
	return AgentToolUse{ToolUseID: id, Name: "bash", Verdict: "ask", DecidedBy: DecidedConfirm}
}

// TestRefusalsAreCountedFromTheLog (spec 101): a block by the risk
// score's block_at, the network or a decision service, and a person's
// deny of a call or of an approval, are refusals; a plan-mode block is
// not; a call that changes something and runs ends a run, a read neither
// counts nor ends one; and the counts reset when the fallback ends.
func TestRefusalsAreCountedFromTheLog(t *testing.T) {
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	l := &refusalLog{t: t}
	l.add(TypeUserMessage, UserMessage{Sender: ada}).
		add(TypeAgentToolUse, blocked("toolu_1", DecidedScore, "block_at")).
		add(TypeAgentToolUse, blocked("toolu_2", DecidedPlan, "")).
		add(TypeAgentToolUse, blocked("toolu_3", DecidedScore, "ask_at")).
		add(TypeAgentToolUse, ran("toolu_4", DecidedReadOnly, "effect:read")).
		add(TypeAgentToolUse, ran("toolu_5", DecidedScore, "effect:read"))
	if r := RefusalsOf(l.evs); r.Consecutive != 1 || r.Total != 1 || r.Fallback {
		t.Fatalf("a block_at, a plan block, another band and two reads: %+v", r)
	}
	l.add(TypeAgentToolUse, blocked("toolu_6", DecidedNetwork, "")).
		add(TypeAgentToolUse, blocked("toolu_7", DecidedLearned, "learned/1"))
	if r := RefusalsOf(l.evs); r.Consecutive != 3 || r.Total != 3 {
		t.Fatalf("network and learned blocks: %+v", r)
	}
	if limit, ok := RefusalsOf(l.evs).Reached(DenialLimits{Consecutive: 3, Total: 20}); !ok || limit != LimitConsecutive {
		t.Fatalf("3 in a row reach %q %v", limit, ok)
	}
	if _, ok := RefusalsOf(l.evs).Reached(DenialLimits{}); ok {
		t.Fatal("limits of 0 are off")
	}
	// A change that runs ends the run; the total stays.
	l.add(TypeAgentToolUse, ran("toolu_8", DecidedSandbox, "effect:write"))
	if r := RefusalsOf(l.evs); r.Consecutive != 0 || r.Total != 3 {
		t.Fatalf("after a change ran: %+v", r)
	}
	if limit, ok := RefusalsOf(l.evs).Reached(DenialLimits{Consecutive: 3, Total: 3}); !ok || limit != LimitTotal {
		t.Fatalf("the total reaches %q %v", limit, ok)
	}
	// An ask a person denies is a refusal, one they allow ends a run.
	l.add(TypeAgentToolUse, waited("toolu_9")).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ToolUseID: "toolu_9", Decision: DecisionDeny}).
		add(TypeAgentToolUse, waited("toolu_10")).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ToolUseID: "toolu_10", Decision: DecisionAllow}).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ToolUseID: "toolu_none", Decision: DecisionDeny})
	if r := RefusalsOf(l.evs); r.Consecutive != 0 || r.Total != 4 {
		t.Fatalf("a deny and an allow: %+v", r)
	}
	// An approval a person's confirmation denied is a refusal; one a
	// message denied is not.
	l.add(TypeApprovalRequested, ApprovalRequested{ApprovalID: "apr_1", ToolUseID: "toolu_8", Verdict: "ask"}).
		add(TypeApprovalRequested, ApprovalRequested{ApprovalID: "apr_2", ToolUseID: "toolu_8", Verdict: "ask"}).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ApprovalID: "apr_1", Decision: DecisionDeny}).
		add(TypeApprovalDecided, ApprovalDecided{ApprovalID: "apr_1", Decision: DecisionDeny, By: ada}).
		add(TypeApprovalDecided, ApprovalDecided{ApprovalID: "apr_2", Decision: DecisionDeny, By: ada})
	if r := RefusalsOf(l.evs); r.Consecutive != 1 || r.Total != 5 {
		t.Fatalf("approvals: %+v", r)
	}
	// The fallback: from a denials_reached until the person's next message,
	// which resets both counts.
	l.add(TypeDenialsReached, DenialsReached{Consecutive: 1, Total: 5, Limit: LimitTotal, Effect: DenialsAsk})
	r := RefusalsOf(l.evs)
	if !r.Fallback || r.Stopped != 0 {
		t.Fatalf("after denials_reached: %+v", r)
	}
	if _, ok := r.Reached(DenialLimits{Consecutive: 1, Total: 1}); ok {
		t.Fatal("the fallback reaches no limit again")
	}
	l.add(TypeUserMessage, UserMessage{Sender: Sender{Subject: "trigger:trg_1", Kind: SenderTrigger}})
	if r := RefusalsOf(l.evs); !r.Fallback {
		t.Fatal("a trigger's message ended the fallback")
	}
	l.add(TypeUserMessage, UserMessage{Sender: ada})
	if r := RefusalsOf(l.evs); r.Fallback || r.Consecutive != 0 || r.Total != 0 {
		t.Fatalf("after the person's message: %+v", r)
	}
}

// TestAStoppedTurnHandsTheNextTurnTheFallback (spec 101): after a
// denials_reached that stopped an unattended turn, the person's next
// message begins a turn in the fallback, and the message after it ends
// the fallback.
func TestAStoppedTurnHandsTheNextTurnTheFallback(t *testing.T) {
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	l := &refusalLog{t: t}
	l.add(TypeAgentToolUse, blocked("toolu_1", DecidedNetwork, "")).
		add(TypeDenialsReached, DenialsReached{Consecutive: 1, Total: 1, Limit: LimitConsecutive, Effect: DenialsStop})
	if r := RefusalsOf(l.evs); !r.Fallback || r.Stopped != 2 {
		t.Fatalf("after the stop: %+v", r)
	}
	l.add(TypeUserMessage, UserMessage{Sender: ada})
	if r := RefusalsOf(l.evs); !r.Fallback || r.Stopped != 0 || r.Total != 1 {
		t.Fatalf("the person's next turn: %+v", r)
	}
	l.add(TypeUserMessage, UserMessage{Sender: ada})
	if r := RefusalsOf(l.evs); r.Fallback || r.Total != 0 {
		t.Fatalf("the message after it: %+v", r)
	}
}

// TestRefusedInCountsOneThreadsRun (spec 101): the refusals of a
// subagent's run are its blocked calls, its calls a person denied and its
// approvals a person denied, after the event that drove the run; another
// thread's and an earlier run's are not.
func TestRefusedInCountsOneThreadsRun(t *testing.T) {
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	l := &refusalLog{t: t}
	l.on("evt_s", TypeAgentToolUse, blocked("toolu_0", DecidedScore, "block_at")).
		on("evt_s", TypeSubagentMessage, SubagentMessage{To: "evt_s"}).
		on("evt_s", TypeAgentToolUse, blocked("toolu_1", DecidedLearned, "")).
		on("", TypeAgentToolUse, blocked("toolu_2", DecidedNetwork, "")).
		on("evt_s", TypeAgentToolUse, waited("toolu_3")).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ToolUseID: "toolu_3", Decision: DecisionDeny}).
		on("evt_s", TypeApprovalRequested, ApprovalRequested{ApprovalID: "apr_1", Verdict: "ask"}).
		add(TypeUserToolConfirmation, UserToolConfirmation{Sender: ada, ApprovalID: "apr_1", Decision: DecisionDeny}).
		on("evt_s", TypeApprovalDecided, ApprovalDecided{ApprovalID: "apr_1", Decision: DecisionDeny}).
		on("evt_s", TypeAgentToolUse, blocked("toolu_4", DecidedPlan, ""))
	if n := RefusedIn(l.evs, "evt_s", 2); n != 3 {
		t.Fatalf("refused in the run: %d, want 3", n)
	}
	if n := RefusedIn(l.evs, "evt_t", 0); n != 0 {
		t.Fatalf("another thread: %d", n)
	}
}

// TestAScopeJoinsTheWiderOfTwo (spec 101): the scope a call records is
// the service's reading where it is wider or Topos says nothing, Topos's
// claim where the service says nothing or a narrower one, and no word
// outside the lists.
func TestAScopeJoinsTheWiderOfTwo(t *testing.T) {
	service := CallScope{Undo: UndoRecoverable, Crosses: &Crosses{Remote: new(true), Public: new(false)}, TargetClass: TargetOwn, TargetKind: "main", Contact: ContactRepeat}
	topos := CallScope{Undo: UndoIrreversible, Crosses: &Crosses{Public: new(true), Organization: new(false)}, TargetClass: "release", TargetKind: "app", Contact: ContactFirst, Source: ScopeFromTopos}
	want := CallScope{
		Undo: UndoIrreversible, Crosses: &Crosses{Remote: new(true), Public: new(true), Organization: new(false)},
		TargetClass: TargetOwn, TargetKind: "app", Contact: ContactFirst, Source: ScopeFromService,
	}
	if got := service.Join(topos); !reflect.DeepEqual(got, want) {
		t.Fatalf("joined %+v, want %+v", got, want)
	}
	if got := (CallScope{}).Join(CallScope{Source: ScopeFromTopos}); got.Crosses != nil || got.Source != ScopeFromService {
		t.Fatalf("an empty join %+v", got)
	}
	if Widest(Undos, "unknown", "nope") != "" || Widest(TargetClasses, TargetPrimary, TargetScratch) != TargetPrimary {
		t.Fatal("widest")
	}
}

// TestHowThePersonAnsweredIsChecked (spec 101): an answer_ms outside a
// day and a surface outside the list are refused; the rest pass.
func TestHowThePersonAnsweredIsChecked(t *testing.T) {
	for _, c := range []struct {
		c  UserToolConfirmation
		ok bool
	}{
		{UserToolConfirmation{}, true},
		{UserToolConfirmation{AnswerMS: new(int64(0)), Surface: SurfaceBulk}, true},
		{UserToolConfirmation{AnswerMS: new(int64(MaxAnswerMS))}, true},
		{UserToolConfirmation{AnswerMS: new(int64(MaxAnswerMS + 1))}, false},
		{UserToolConfirmation{AnswerMS: new(int64(-1))}, false},
		{UserToolConfirmation{Surface: "chat"}, false},
	} {
		if err := c.c.CheckAnswer(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.c, err)
		}
	}
}

// TestAnAllowsDenialsOnlyLower (spec 101): an authorizer's limits apply
// where lower, and a limit of zero, off, takes the other's.
func TestAnAllowsDenialsOnlyLower(t *testing.T) {
	base := DenialLimits{Consecutive: 3, Total: 20}
	for _, c := range []struct {
		o    *DenialLimits
		want DenialLimits
	}{
		{nil, base},
		{&DenialLimits{Consecutive: 2, Total: 30}, DenialLimits{Consecutive: 2, Total: 20}},
		{&DenialLimits{Consecutive: 0, Total: 5}, DenialLimits{Consecutive: 3, Total: 5}},
	} {
		if got := base.Lower(c.o); got != c.want {
			t.Errorf("lowered by %+v: %+v, want %+v", c.o, got, c.want)
		}
	}
	if got := (DenialLimits{}).Lower(&DenialLimits{Consecutive: 4, Total: 9}); got != (DenialLimits{Consecutive: 4, Total: 9}) {
		t.Errorf("off lowered: %+v", got)
	}
}

// TestTheHeaderKeepsTheLatestBoundary (spec 101): an authorizer's change
// that names a boundary replaces the session's, one that names none keeps
// it, and one that moves only the boundary leaves the network as it was.
func TestTheHeaderKeepsTheLatestBoundary(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	t0 := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	s := New(AgentRef{ID: "agt_1", Version: 1}, Sender{Subject: "usr_ada", Kind: SenderPerson}, RunnerHosted, Machine{Kind: MachineCella}, t0)
	s.Boundary = &Boundary{Hosts: []string{"example.com"}}
	if err := st.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []NetworkChanged{
		{Source: NetworkFromAuthorizer, Boundary: &Boundary{Hosts: []string{"*.example.org"}}},
		{Source: NetworkFromAuthorizer, Added: []string{"a.example.com"}},
		{Source: NetworkFromPerson, Boundary: &Boundary{Hosts: []string{"ignored.example"}}},
	} {
		got, err := st.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		e, err := NewEvent(TypeNetworkChanged, c, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.SessionID, e.Seq = s.ID, got.LastSeq+1
		if _, err := st.Append(ctx, s.ID, got.LastSeq, []Event{e}); err != nil {
			t.Fatal(err)
		}
		if c.Boundary != nil && c.Source == NetworkFromAuthorizer && !c.Moves() {
			if got, _ := st.Get(ctx, s.ID); got.Network != nil {
				t.Fatalf("a boundary alone moved the network to %+v", got.Network)
			}
		}
	}
	got, err := st.Get(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Boundary == nil || !reflect.DeepEqual(got.Boundary.Hosts, []string{"*.example.org"}) {
		t.Fatalf("boundary %+v", got.Boundary)
	}
	if got.Network == nil || !reflect.DeepEqual(got.Network.Hosts, []string{"a.example.com"}) {
		t.Fatalf("network %+v", got.Network)
	}
}
