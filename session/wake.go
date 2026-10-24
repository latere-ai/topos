// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"slices"
	"time"
)

// ToolWake is the name of the tool that sets and cancels a session's next
// wake (spec 086): a one-shot trigger that sends the note the agent wrote
// back into the same session at the time it chose.
const ToolWake = "wake"

// WakeSet is the payload of session.wake_set: the runner applied the
// session's wake, the trigger TriggerID, at At, with the title a person
// reads and the note the agent wrote, for the call ToolUseID. Count is
// the wake's place in the chain of wakes since the session's initiator
// last wrote, answered or confirmed.
type WakeSet struct {
	TriggerID string    `json:"trigger_id"`
	At        time.Time `json:"at"`
	Title     string    `json:"title"`
	Note      string    `json:"note"`
	ToolUseID string    `json:"tool_use_id"`
	Count     int       `json:"count"`
}

// The reasons a wake is cleared.
const (
	// WakeCanceled is a wake the agent's cancel, or a person's or a
	// client's delete of its trigger, removed.
	WakeCanceled = "canceled"
	// WakeEnded is a wake its session's end, deletion or archive removed.
	WakeEnded = "ended"
	// WakeSkipped is a wake that fired past its maxAge, after a stop of
	// the server, and sent nothing.
	WakeSkipped = "skipped"
	// WakeRefused is a wake whose firing the authorizer refused, so it
	// sent nothing; the cleared event's Refusal is the firing's reason.
	WakeRefused = "refused"
)

// WakeCleared is the payload of session.wake_cleared: the wake TriggerID
// is gone for Reason, by the agent's cancel ToolUseID or by Sender, a
// person's or a client's delete of its trigger. A refused wake names the
// firing's reason as Refusal.
type WakeCleared struct {
	TriggerID string  `json:"trigger_id"`
	Reason    string  `json:"reason"`
	ToolUseID string  `json:"tool_use_id,omitempty"`
	Sender    *Sender `json:"sender,omitempty"`
	Refusal   string  `json:"refusal,omitempty"`
}

// MessageWake is what a wake's message says of the wake that sent it: its
// trigger, when it was set and when it was due, and its place in the
// chain.
type MessageWake struct {
	TriggerID string    `json:"trigger_id"`
	SetAt     time.Time `json:"set_at"`
	DueAt     time.Time `json:"due_at"`
	Count     int       `json:"count"`
}

// Wake is a session's pending wake as its header folds it.
type Wake struct {
	TriggerID string    `json:"trigger_id"`
	At        time.Time `json:"at"`
	Title     string    `json:"title"`
	Count     int       `json:"count"`
}

// foldWake is the pending wake after e, from cur: a session.wake_set sets
// it, and a session.wake_cleared of it or the message its firing sent
// clears it.
func foldWake(cur *Wake, e Event) *Wake {
	if e.Redacted() {
		return cur
	}
	switch e.Type {
	case TypeWakeSet:
		var p WakeSet
		if e.Decode(&p) == nil {
			return &Wake{TriggerID: p.TriggerID, At: p.At, Title: p.Title, Count: p.Count}
		}
	case TypeWakeCleared:
		var p WakeCleared
		if e.Decode(&p) == nil && cur != nil && p.TriggerID == cur.TriggerID {
			return nil
		}
	case TypeUserMessage:
		var p UserMessage
		if e.Decode(&p) == nil && p.Wake != nil && cur != nil && p.Wake.TriggerID == cur.TriggerID {
			return nil
		}
	}
	return cur
}

// PendingWake is the wake a log leaves pending, nil for none: the latest
// set that no clear and no message of its firing followed.
func PendingWake(evs []Event) *Wake {
	var w *Wake
	for _, e := range evs {
		w = foldWake(w, e)
	}
	return w
}

// LatestWakeSet is the latest session.wake_set of the trigger triggerID
// in a log, and its event's time, which a wake's message reads when it
// was set and its place in the chain from; false for none.
func LatestWakeSet(evs []Event, triggerID string) (WakeSet, time.Time, bool) {
	for _, e := range slices.Backward(evs) {
		if e.Type != TypeWakeSet || e.Redacted() {
			continue
		}
		var p WakeSet
		if e.Decode(&p) == nil && p.TriggerID == triggerID {
			return p, e.Time, true
		}
	}
	return WakeSet{}, time.Time{}, false
}

// WakesInARow counts the wakes' messages of a log since the latest
// message, answer or confirmation from initiator, the session's
// initiator: how many times in a row the agent woke itself with no word
// from the person.
func WakesInARow(evs []Event, initiator string) int {
	n := 0
	for _, e := range slices.Backward(evs) {
		if e.Redacted() {
			continue
		}
		var from Sender
		switch e.Type {
		case TypeUserMessage:
			var p UserMessage
			if e.Decode(&p) != nil {
				continue
			}
			if p.Wake != nil {
				n++
				continue
			}
			from = p.Sender
		case TypeUserAnswer:
			var p UserAnswer
			if e.Decode(&p) != nil {
				continue
			}
			from = p.Sender
		case TypeUserToolConfirmation:
			var p UserToolConfirmation
			if e.Decode(&p) != nil {
				continue
			}
			from = p.Sender
		default:
			continue
		}
		if from.Subject == initiator {
			return n
		}
	}
	return n
}
