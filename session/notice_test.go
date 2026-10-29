// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"testing"
	"time"
)

// logged is one event of a test's log: its type, its thread and its
// payload.
type logged struct {
	typ    Type
	thread string
	p      any
}

// logOf numbers events of the given types and payloads from seq 1 on.
func logOf(t *testing.T, items ...logged) []Event {
	t.Helper()
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	out := make([]Event, 0, len(items))
	for i, it := range items {
		e, err := NewEvent(it.typ, it.p, at.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		e.Seq, e.Subagent = uint64(i+1), it.thread
		out = append(out, e)
	}
	return out
}

// TestTheSessionFoldsOutcomeAndNotice: the header keeps the outcome of the
// latest status that recorded one, and the latest notify's result of the
// session's own thread as its notice; a thread's result and a status with
// no outcome change neither.
func TestTheSessionFoldsOutcomeAndNotice(t *testing.T) {
	notice := json.RawMessage(`{"notice":{"title":"The fare dropped"}}`)
	evs := logOf(t,
		logged{TypeToolResult, "", ToolResult{ToolUseID: "toolu_1", Meta: notice}},
		logged{TypeSessionStatus, "", SessionStatus{Status: StatusIdle, StopReason: StopEndTurn, Outcome: OutcomeNotified}},
		logged{TypeToolResult, "thr_1", ToolResult{ToolUseID: "toolu_2", Meta: json.RawMessage(`{"notice":{"title":"A thread's"}}`)}},
		logged{TypeSessionStatus, "", SessionStatus{Status: StatusIdle, StopReason: StopEndTurn}},
	)
	var s Session
	ApplyBatch(&s, evs)
	if s.Outcome != OutcomeNotified || s.Notice == nil || s.Notice.Title != "The fare dropped" || s.Notice.Seq != 1 || !s.Notice.At.Equal(evs[0].Time) {
		t.Fatalf("the header's outcome %q and notice %+v", s.Outcome, s.Notice)
	}
	ApplyBatch(&s, logOf(t, logged{TypeSessionStatus, "", SessionStatus{Status: StatusIdle, StopReason: StopEndTurn, Outcome: OutcomeQuiet}}))
	if s.Outcome != OutcomeQuiet {
		t.Fatalf("the latest outcome is %q", s.Outcome)
	}
	if !Noticed(evs, 0) || Noticed(evs, 1) {
		t.Fatal("Noticed reads the turn's results wrong")
	}
}

// TestATurnsFirstInputNamesItsTrigger: TriggerTurn answers the trigger's
// message that is the first input after the last status that closed a
// turn, and nothing for a turn a person's message, a confirmation or an
// answer began.
func TestATurnsFirstInputNamesItsTrigger(t *testing.T) {
	trigger := UserMessage{Sender: Sender{Subject: "trigger:trg_1", Kind: SenderTrigger}, FiringID: "frg_1", Notify: NotifyJudged}
	person := UserMessage{Sender: Sender{Subject: "usr_ada", Kind: SenderPerson}}
	idle := SessionStatus{Status: StatusIdle, StopReason: StopEndTurn}
	running := SessionStatus{Status: StatusRunning}
	cases := []struct {
		name string
		evs  []Event
		want bool
	}{
		{"a first message of a trigger", logOf(t, logged{TypeUserMessage, "", trigger}, logged{TypeSessionStatus, "", running}), true},
		{"after a person's turn", logOf(t, logged{TypeUserMessage, "", person}, logged{TypeSessionStatus, "", running}, logged{TypeSessionStatus, "", idle}, logged{TypeUserMessage, "", trigger}, logged{TypeSessionStatus, "", running}), true},
		{"a person's message first", logOf(t, logged{TypeUserMessage, "", person}, logged{TypeUserMessage, "", trigger}, logged{TypeSessionStatus, "", running}), false},
		{"a confirmation first", logOf(t, logged{TypeSessionStatus, "", idle}, logged{TypeUserToolConfirmation, "", UserToolConfirmation{ToolUseID: "toolu_1", Decision: DecisionAllow}}, logged{TypeSessionStatus, "", running}), false},
		{"a thread's status is no turn's close", logOf(t, logged{TypeUserMessage, "", trigger}, logged{TypeSessionStatus, "thr_1", idle}, logged{TypeSessionStatus, "", running}), true},
		{"nothing", logOf(t, logged{TypeSessionStatus, "", running}), false},
	}
	for _, c := range cases {
		m, ok := TriggerTurn(c.evs, c.evs[len(c.evs)-1].Seq)
		if ok != c.want || ok && !m.Judged() {
			t.Errorf("%s: %+v, %v; want %v", c.name, m, ok, c.want)
		}
	}
}
