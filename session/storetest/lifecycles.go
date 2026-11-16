// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/topos/session"
)

// testStatusLifecycle drives the append of session.status events, the one
// writer of a session's status, through session.StatusTable on the store
// under test: Exhaustive asks for every status from every status, and Race
// appends two statuses at once to a session in each. An append that loses
// the race names a sequence the session has moved past and is refused.
func testStatusLifecycle(t *testing.T, st session.Store) {
	ctx := t.Context()
	reasons := map[session.Status]session.StopReason{
		session.StatusIdle: session.StopEndTurn, session.StatusEnded: session.StopCompleted,
	}
	var seq atomic.Int64
	step := func(id string, to session.Status) error {
		s, err := st.Get(ctx, id)
		if err != nil {
			return err
		}
		if s.Status == to {
			// Repeating a status is no step of the table, though a store
			// takes a runner's second claim of a running session.
			return errors.Join(errors.New("the session already has the status"), statemachine.ErrIllegalTransition)
		}
		e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: to, StopReason: reasons[to]}, t0.Add(time.Duration(seq.Add(1))*time.Second))
		if err != nil {
			return err
		}
		batch := []session.Event{e}
		session.Stamp(id, s.LastSeq, batch)
		_, err = st.Append(ctx, id, s.LastSeq, batch)
		if errors.Is(err, session.ErrSequenceConflict) {
			return errors.Join(err, statemachine.ErrIllegalTransition)
		}
		return err
	}
	d := statemachinetest.Driver[session.Status]{
		Enter: func(t *testing.T, state session.Status) string {
			s := create(t, st)
			switch state {
			case session.StatusRunning:
				noErr(t, step(s.ID, session.StatusRunning))
			case session.StatusEnded:
				noErr(t, step(s.ID, session.StatusEnded))
			case session.StatusIdle:
			}
			return s.ID
		},
		Move: func(id string, to session.Status) error { return step(id, to) },
		State: func(t *testing.T, id string) session.Status {
			s, err := st.Get(ctx, id)
			noErr(t, err)
			return s.Status
		},
	}
	statemachinetest.Exhaustive(t, session.StatusTable, d)
	statemachinetest.Race(t, session.StatusTable, d)
}

func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// testMessageLifecycle drives the events that settle a waiting message, a
// messages.read and a user.withdraw, through session.MessageTable: the log is
// the store's, WaitingMessages folds it, and an append that loses a race
// names a sequence the session has moved past and is refused.
func testMessageLifecycle(t *testing.T, st session.Store) {
	ctx := t.Context()
	var seq atomic.Int64
	messages := map[string]string{} // session id to the id of its waiting message
	appendOne := func(id string, last uint64, typ session.Type, payload any) error {
		e, err := session.NewEvent(typ, payload, t0.Add(time.Duration(seq.Add(1))*time.Second))
		if err != nil {
			return err
		}
		batch := []session.Event{e}
		session.Stamp(id, last, batch)
		_, err = st.Append(ctx, id, last, batch)
		if errors.Is(err, session.ErrSequenceConflict) {
			return errors.Join(err, statemachine.ErrIllegalTransition)
		}
		return err
	}
	read := func(id string) (uint64, session.MessageState) {
		s, err := st.Get(ctx, id)
		noErr(t, err)
		evs, err := st.Events(ctx, id, 1, 0)
		noErr(t, err)
		for _, w := range session.WaitingMessages(evs) {
			if w.Event.ID == messages[id] {
				return s.LastSeq, w.State
			}
		}
		t.Fatalf("session %s has no waiting message", id)
		return 0, session.MessageWaiting
	}
	d := statemachinetest.Driver[session.MessageState]{
		Enter: func(t *testing.T, state session.MessageState) string {
			s := create(t, st)
			last := appendAll(t, st, s.ID, 0, Status(t, session.StatusRunning, "", t0.Add(time.Second)))
			msg := Message(t, "hello", t0.Add(2*time.Second))
			messages[s.ID] = msg.ID
			last = appendAll(t, st, s.ID, last, msg)
			switch state {
			case session.MessageRead:
				noErr(t, appendOne(s.ID, last, session.TypeMessagesRead, session.MessagesRead{EventIDs: []string{msg.ID}}))
			case session.MessageWithdrawn:
				noErr(t, appendOne(s.ID, last, session.TypeUserWithdraw, session.UserWithdraw{Sender: session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, EventID: msg.ID}))
			case session.MessageWaiting:
			}
			return s.ID
		},
		Move: func(id string, to session.MessageState) error {
			last, from := read(id)
			if from != session.MessageWaiting {
				return errors.Join(errors.New("the message is settled"), statemachine.ErrIllegalTransition)
			}
			switch to {
			case session.MessageRead:
				return appendOne(id, last, session.TypeMessagesRead, session.MessagesRead{EventIDs: []string{messages[id]}})
			case session.MessageWithdrawn:
				return appendOne(id, last, session.TypeUserWithdraw, session.UserWithdraw{Sender: session.Sender{Subject: "usr_1", Kind: session.SenderPerson}, EventID: messages[id]})
			case session.MessageWaiting:
			}
			return errors.Join(errors.New("no writer returns a message to waiting"), statemachine.ErrIllegalTransition)
		},
		State: func(t *testing.T, id string) session.MessageState {
			_, state := read(id)
			return state
		},
	}
	statemachinetest.Exhaustive(t, session.MessageTable, d)
	statemachinetest.Race(t, session.MessageTable, d)
}
