// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"slices"
)

// MessagesRead is the payload of messages.read: the waiting messages the
// request after it is the first to read, in sequence order (spec 098).
type MessagesRead struct {
	EventIDs []string `json:"event_ids"`
}

// UserWithdraw is the payload of user.withdraw: a person takes back a
// waiting message they sent, before any request read it (spec 098).
type UserWithdraw struct {
	Sender  Sender `json:"sender"`
	EventID string `json:"event_id"`
}

// DetailSendNow is the detail of the idle interrupted status of a turn a
// send_now message cut (spec 098).
const DetailSendNow = "send_now"

// MessageState is where a waiting message stands (spec 098).
type MessageState string

// The states of a waiting message.
const (
	MessageWaiting   MessageState = "waiting"
	MessageRead      MessageState = "read"
	MessageWithdrawn MessageState = "withdrawn"
)

// Waiting is a waiting message: a user.message of the session's own
// thread appended while that thread's turn ran (spec 098).
type Waiting struct {
	Event   Event
	Message UserMessage
	State   MessageState
	// ReadAt is the sequence of the messages.read that read the message,
	// where the fold renders it; zero for an unread message and for one
	// a request written before spec 098 read where it stands.
	ReadAt uint64
	// Arrived is the sequence of the running status of the turn the
	// message arrived in, and Closed that of the status that closed it,
	// or of the running status of a later turn when no status closed it,
	// zero while the turn runs.
	Arrived, Closed uint64
	// CutForIt is set when the turn the message arrived in ended
	// interrupted with DetailSendNow and the message asked to be read now.
	CutForIt bool
}

// Person reports whether a person sent the message.
func (w Waiting) Person() bool { return w.Message.Sender.Kind == SenderPerson }

// ReadInItsTurn reports whether the message was read by a request of the
// turn it arrived in, which the fold says to the model.
func (w Waiting) ReadInItsTurn() bool {
	return w.State == MessageRead && w.ReadAt != 0 && (w.Closed == 0 || w.ReadAt < w.Closed)
}

// Starts reports whether the message, unread, starts a turn of its own:
// a person's and a trigger's do; any other sender's is read by the next
// turn whatever starts it.
func (w Waiting) Starts() bool {
	return w.Message.Sender.Kind == SenderPerson || w.Message.Sender.Kind == SenderTrigger
}

// WaitingMessages lists the waiting messages of a log in sequence order,
// each with its state. A message is read once a messages.read names it
// before any user.withdraw of it, or once a request of the session's own
// thread written before spec 098, which names nothing it read, folded the
// log past it; it is withdrawn once a user.withdraw of it comes before
// any messages.read that names it.
func WaitingMessages(events []Event) []Waiting {
	evs := slices.Clone(events)
	slices.SortStableFunc(evs, func(a, b Event) int { return cmp.Compare(a.Seq, b.Seq) })

	var out []Waiting
	at := map[string]int{}
	var running uint64
	open := false
	var opened []int
	for _, e := range evs {
		switch e.Type {
		case TypeSessionStatus:
			var p SessionStatus
			if e.Redacted() || e.Decode(&p) != nil {
				continue
			}
			// A running status while a turn is open starts the turn of a
			// runner that took over one nobody closed, which closes it.
			if open {
				cut := p.Status == StatusIdle && p.StopReason == StopInterrupted && p.Detail == DetailSendNow
				for _, i := range opened {
					out[i].Closed = e.Seq
					out[i].CutForIt = cut && out[i].Message.SendNow
				}
			}
			opened = opened[:0]
			open = p.Status == StatusRunning
			if open {
				running = e.Seq
			}
		case TypeUserMessage:
			if !open || e.Subagent != "" {
				continue
			}
			w := Waiting{Event: e, State: MessageWaiting, Arrived: running}
			if !e.Redacted() {
				if err := e.Decode(&w.Message); err != nil {
					continue
				}
			}
			at[e.ID] = len(out)
			opened = append(opened, len(out))
			out = append(out, w)
		case TypeUserWithdraw:
			var p UserWithdraw
			if e.Redacted() || e.Decode(&p) != nil {
				continue
			}
			if i, ok := at[p.EventID]; ok && out[i].State == MessageWaiting {
				out[i].State = MessageWithdrawn
			}
		case TypeMessagesRead:
			var p MessagesRead
			if e.Redacted() || e.Decode(&p) != nil {
				continue
			}
			for _, id := range p.EventIDs {
				if i, ok := at[id]; ok && out[i].State == MessageWaiting {
					out[i].State, out[i].ReadAt = MessageRead, e.Seq
				}
			}
		case TypeModelRequest:
			var p ModelRequest
			if e.Subagent != "" || e.Redacted() || e.Decode(&p) != nil || p.NamesReads || p.Purpose != "" {
				continue
			}
			for i := range out {
				if out[i].State == MessageWaiting && out[i].Event.Seq <= p.FoldSeq {
					out[i].State = MessageRead
				}
			}
		}
	}
	return out
}

// Unread returns the waiting messages of a log no request read and nobody
// withdrew.
func Unread(events []Event) []Waiting {
	var out []Waiting
	for _, w := range WaitingMessages(events) {
		if w.State == MessageWaiting {
			out = append(out, w)
		}
	}
	return out
}

// PendingWaiting reports whether a log holds an unread waiting message
// that starts a turn of its own, while the session waits for no person's
// answer: an ask, a question or a client's call holds such a message
// until the turn the answer starts reads it (spec 098).
func PendingWaiting(events []Event) bool {
	starts := false
	for _, w := range Unread(events) {
		if w.Starts() {
			starts = true
			break
		}
	}
	return starts && len(Awaiting(events)) == 0
}
