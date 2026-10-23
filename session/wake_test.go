// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"
	"time"
)

// wakeEvents numbers evs from seq 1 on, as a log holds them.
func wakeEvents(t *testing.T, payloads ...any) []Event {
	t.Helper()
	at := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	var out []Event
	for i, p := range payloads {
		var typ Type
		switch p.(type) {
		case WakeSet:
			typ = TypeWakeSet
		case WakeCleared:
			typ = TypeWakeCleared
		case UserMessage:
			typ = TypeUserMessage
		case UserAnswer:
			typ = TypeUserAnswer
		case UserToolConfirmation:
			typ = TypeUserToolConfirmation
		default:
			t.Fatalf("no type for %T", p)
		}
		e, err := NewEvent(typ, p, at.Add(time.Duration(i)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = uint64(i + 1)
		out = append(out, e)
	}
	return out
}

// TestTheSessionFoldsItsWake: a wake_set sets the header's wake, the
// latest replacing the one before; a clear of another trigger leaves it,
// one of it clears it, and so does the message its firing sent; a fork's
// copied wake is its parent's and not the fork's.
func TestTheSessionFoldsItsWake(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	evs := wakeEvents(t,
		WakeSet{TriggerID: "trg_1", At: at, Title: "Look", Count: 1},
		WakeSet{TriggerID: "trg_1", At: at.Add(time.Hour), Title: "Look later", Count: 1},
		WakeCleared{TriggerID: "trg_2", Reason: WakeCanceled},
		UserMessage{Sender: ada},
	)
	var s Session
	ApplyBatch(&s, evs)
	if s.Wake == nil || s.Wake.TriggerID != "trg_1" || !s.Wake.At.Equal(at.Add(time.Hour)) || s.Wake.Title != "Look later" || s.Wake.Count != 1 {
		t.Fatalf("the header's wake is %+v", s.Wake)
	}
	if w := PendingWake(evs); w == nil || *w != *s.Wake {
		t.Fatalf("the pending wake is %+v", w)
	}
	fired := wakeEvents(t, UserMessage{Sender: Sender{Subject: "trigger:trg_1", Kind: SenderTrigger}, Wake: &MessageWake{TriggerID: "trg_1", Count: 1}})
	ApplyBatch(&s, fired)
	if s.Wake != nil {
		t.Fatalf("the fired wake is kept: %+v", s.Wake)
	}
	cleared := wakeEvents(t, WakeSet{TriggerID: "trg_3", At: at, Count: 1}, WakeCleared{TriggerID: "trg_3", Reason: WakeEnded})
	if w := PendingWake(cleared); w != nil {
		t.Fatalf("a cleared wake is pending: %+v", w)
	}
	fork := Session{Parent: &Parent{SessionID: "ses_p", Seq: 1}}
	ApplyBatch(&fork, evs[:1])
	if fork.Wake != nil {
		t.Fatalf("a fork holds its parent's wake %+v", fork.Wake)
	}
	if set, when, ok := LatestWakeSet(evs, "trg_1"); !ok || set.Title != "Look later" || !when.Equal(evs[1].Time) {
		t.Fatalf("the latest set is %+v at %s (%v)", set, when, ok)
	}
	if _, _, ok := LatestWakeSet(evs, "trg_9"); ok {
		t.Fatal("a set of another trigger was found")
	}
}

// TestWakesInARowCountFromTheInitiatorsWord: the count is the wakes'
// messages since the initiator's last message, answer or confirmation;
// another sender's word does not start it again.
func TestWakesInARowCountFromTheInitiatorsWord(t *testing.T) {
	ada := Sender{Subject: "usr_ada", Kind: SenderPerson}
	bob := Sender{Subject: "usr_bob", Kind: SenderPerson}
	wake := UserMessage{Sender: Sender{Subject: "trigger:trg_1", Kind: SenderTrigger}, Wake: &MessageWake{TriggerID: "trg_1"}}
	cases := []struct {
		name string
		evs  []any
		want int
	}{
		{"none", []any{UserMessage{Sender: ada}}, 0},
		{"two after a message", []any{UserMessage{Sender: ada}, wake, wake}, 2},
		{"another sender between", []any{UserMessage{Sender: ada}, wake, UserMessage{Sender: bob}, wake}, 2},
		{"an answer starts again", []any{wake, wake, UserAnswer{Sender: ada}, wake}, 1},
		{"a confirmation starts again", []any{wake, UserToolConfirmation{Sender: ada, ToolUseID: "toolu_1", Decision: DecisionAllow}}, 0},
		{"another's confirmation does not", []any{wake, UserToolConfirmation{Sender: bob, ToolUseID: "toolu_1", Decision: DecisionAllow}, wake}, 2},
		{"from the start", []any{wake, wake, wake}, 3},
	}
	for _, c := range cases {
		if got := WakesInARow(wakeEvents(t, c.evs...), ada.Subject); got != c.want {
			t.Errorf("%s: %d wakes in a row, want %d", c.name, got, c.want)
		}
	}
}
