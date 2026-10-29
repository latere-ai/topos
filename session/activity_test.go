// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"
)

// TestSenderClientIsOptional: a sender with no client renders none, so an
// event a token without a client wrote reads as before spec 085, and an
// event written before it reads with an empty client.
func TestSenderClientIsOptional(t *testing.T) {
	b, err := Marshal(Sender{Subject: "usr_1", Kind: SenderPerson})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "client") {
		t.Fatalf("a sender with no client renders %s", b)
	}
	b, err = Marshal(Sender{Subject: "usr_1", Kind: SenderPerson, Client: "web"})
	if err != nil || string(b) != `{"subject":"usr_1","kind":"person","client":"web"}` {
		t.Fatalf("a sender with a client renders %s, %v", b, err)
	}
	var m UserMessage
	old := Event{Type: TypeUserMessage, Payload: []byte(`{"sender":{"subject":"usr_1","kind":"person"},"content":[]}`)}
	if err := old.Decode(&m); err != nil || m.Sender.Client != "" {
		t.Fatalf("an older message reads %+v, %v", m.Sender, err)
	}
}

// TestTheActivityRules: a person's message on the session's own thread
// with no firing counts, a trigger's, a subagent's and a redacted one do
// not; a turn's normal end finishes an answer, an error, an API end and
// a subagent's end do not; a session a person started counts, a
// trigger's and a rollout's do not.
func TestTheActivityRules(t *testing.T) {
	msg := func(kind, thread, firing string) Event {
		e, err := NewEvent(TypeUserMessage, UserMessage{Sender: Sender{Subject: "usr_1", Kind: kind}, FiringID: firing}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Subagent = thread
		return e
	}
	if _, ok := PersonMessage(msg(SenderPerson, "", "")); !ok {
		t.Error("a person's message does not count")
	}
	for name, e := range map[string]Event{
		"a trigger's":    msg(SenderTrigger, "", "fir_1"),
		"a firing's":     msg(SenderPerson, "", "fir_1"),
		"a subagent's":   msg(SenderPerson, "thr_1", ""),
		"a redacted one": {Type: TypeUserMessage, Payload: tombstone},
	} {
		if _, ok := PersonMessage(e); ok {
			t.Errorf("%s message counts", name)
		}
	}
	status := func(s Status, r StopReason, turn int, thread string) Event {
		e, err := NewEvent(TypeSessionStatus, SessionStatus{Status: s, StopReason: r}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn, e.Subagent = turn, thread
		return e
	}
	for _, c := range []struct {
		name string
		e    Event
		want bool
	}{
		{"an end of turn", status(StatusIdle, StopEndTurn, 1, ""), true},
		{"a completed end", status(StatusEnded, StopCompleted, 2, ""), true},
		{"an error", status(StatusIdle, StopError, 1, ""), false},
		{"a budget", status(StatusIdle, StopBudget, 1, ""), false},
		{"a question", status(StatusIdle, StopQuestion, 1, ""), false},
		{"an end through the API", status(StatusEnded, StopCompleted, 0, ""), false},
		{"a subagent's end", status(StatusIdle, StopEndTurn, 1, "thr_1"), false},
		{"a message", msg(SenderPerson, "", ""), false},
	} {
		if got := FinishedAnswer(c.e); got != c.want {
			t.Errorf("%s: FinishedAnswer = %v, want %v", c.name, got, c.want)
		}
	}
	person := Sender{Subject: "usr_1", Kind: SenderPerson}
	if !StartedByPerson(Session{Initiator: person}) || StartedByPerson(Session{Initiator: person, TriggerID: "trg_1"}) ||
		StartedByPerson(Session{Initiator: person, Rollout: "cel_1.smoke"}) || StartedByPerson(Session{Initiator: Sender{Kind: SenderTrigger}}) {
		t.Error("StartedByPerson does not follow the rules")
	}
}
