// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package storetest

import (
	"reflect"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

// testActivity holds a store to spec 085's counts of a day: a person's
// messages on a session's own thread by client, the sessions a person
// started by client, and the turns that answered a person and ended
// normally; a trigger's message, a subagent's, a redacted one, a
// rollout's session, a trigger's session, a turn that stopped on an error
// and an end through the API are none of them, and nothing outside the
// day counts. A store with its own Activity answers what CountActivity
// reads from every log.
func testActivity(t *testing.T, st session.Store) {
	day := session.ActivityOptions{From: t0.Truncate(24 * time.Hour), To: t0.Truncate(24 * time.Hour).Add(24 * time.Hour)}
	person := func(subject, client string) session.Sender {
		return session.Sender{Subject: subject, Kind: session.SenderPerson, Client: client}
	}
	message := func(by session.Sender, at time.Time, turn int, thread, firing string) session.Event {
		e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{
			Sender: by, Content: []lux.Block{{Type: ir.BlockText, Text: "hello"}}, FiringID: firing,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn, e.Thread = turn, thread
		return e
	}
	status := func(s session.Status, reason session.StopReason, at time.Time, turn int) session.Event {
		e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: s, StopReason: reason}, at)
		if err != nil {
			t.Fatal(err)
		}
		e.Turn = turn
		return e
	}
	start := func(by session.Sender, created time.Time, edit func(*session.Session), events ...session.Event) session.Session {
		s := NewSession()
		s.Initiator, s.CreatedAt, s.UpdatedAt = by, created, created
		if edit != nil {
			edit(&s)
		}
		if err := st.Create(t.Context(), s, nil); err != nil {
			t.Fatal(err)
		}
		appendAll(t, st, s.ID, 0, events...)
		return s
	}
	ada, adaDesk, bo := person("usr_1", "web"), person("usr_1", "desk"), person("usr_2", "")
	trigger := session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}

	// Ada's conversation on the web, continued from the desktop: two
	// messages, an answer that finished and one that failed; then a
	// trigger's message and its answer, a subagent's message, and a
	// message the next day.
	start(ada, t0, nil,
		message(ada, t0, 1, "", ""),
		status(session.StatusRunning, "", t0, 1),
		status(session.StatusIdle, session.StopEndTurn, t0.Add(time.Minute), 1),
		message(adaDesk, t0.Add(2*time.Minute), 2, "", ""),
		status(session.StatusIdle, session.StopError, t0.Add(3*time.Minute), 2),
		message(trigger, t0.Add(4*time.Minute), 3, "", "fir_1"),
		status(session.StatusIdle, session.StopEndTurn, t0.Add(5*time.Minute), 3),
		message(ada, t0.Add(6*time.Minute), 4, "thr_1", ""),
		message(ada, t0.Add(20*time.Hour), 5, "", ""),
	)
	// Ada's agent that ends on idle finishes its turn as completed.
	start(ada, t0, nil,
		message(ada, t0, 1, "", ""),
		status(session.StatusEnded, session.StopCompleted, t0.Add(time.Minute), 1),
	)
	// Bo's conversation from the day before, with a token that named no
	// client: one answer, and then an end through the API.
	start(bo, t0.Add(-24*time.Hour), nil,
		message(bo, t0, 1, "", ""),
		status(session.StatusIdle, session.StopEndTurn, t0.Add(time.Minute), 1),
		status(session.StatusEnded, session.StopCompleted, t0.Add(2*time.Minute), 0),
	)
	// A rollout's run and a trigger's session are no person's.
	start(ada, t0, func(s *session.Session) { s.Rollout = "cel_1.smoke" },
		message(ada, t0, 1, "", ""),
		status(session.StatusIdle, session.StopEndTurn, t0.Add(time.Minute), 1),
	)
	start(ada, t0, func(s *session.Session) { s.TriggerID = "trg_1" },
		message(trigger, t0, 1, "", "fir_2"),
		status(session.StatusIdle, session.StopEndTurn, t0.Add(time.Minute), 1),
	)
	// A message later redacted, and the answer to it, count for no one.
	cy := person("usr_3", "web")
	redacted := message(cy, t0, 1, "", "")
	s := start(cy, t0.Add(-24*time.Hour), nil, redacted, status(session.StatusIdle, session.StopEndTurn, t0.Add(time.Minute), 1))
	if err := st.Redact(t.Context(), s.ID, redacted.ID, cy, "test"); err != nil {
		t.Fatal(err)
	}

	adaDay := session.SenderActivity{Sender: "usr_1",
		Messages: []session.ClientCount{{Client: "desk", Count: 1}, {Client: "web", Count: 2}},
		Sessions: []session.ClientCount{{Client: "web", Count: 2}}, Answers: 2}
	boDay := session.SenderActivity{Sender: "usr_2",
		Messages: []session.ClientCount{{Client: "", Count: 1}}, Sessions: []session.ClientCount{}, Answers: 1}
	for _, c := range []struct {
		name string
		edit func(*session.ActivityOptions)
		want []session.SenderActivity
	}{
		{"the day", nil, []session.SenderActivity{adaDay, boDay}},
		{"one sender", func(o *session.ActivityOptions) { o.Sender = "usr_2" }, []session.SenderActivity{boDay}},
		{"after a sender", func(o *session.ActivityOptions) { o.After = "usr_1" }, []session.SenderActivity{boDay}},
		{"a page of one", func(o *session.ActivityOptions) { o.Limit = 1 }, []session.SenderActivity{adaDay}},
		{"another day", func(o *session.ActivityOptions) {
			o.From, o.To = o.From.Add(-48*time.Hour), o.From.Add(-24*time.Hour)
		}, []session.SenderActivity{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := day
			if c.edit != nil {
				c.edit(&o)
			}
			got, err := session.CountActivity(t.Context(), st, o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("activity\n got %+v\nwant %+v", got, c.want)
			}
			if _, ok := st.(session.Activity); !ok {
				return
			}
			// The store's own count is the reference's.
			ref, err := session.CountActivity(t.Context(), plain{st}, o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, ref) {
				t.Fatalf("the store's count\n got %+v\nwant what every log reads, %+v", got, ref)
			}
		})
	}
}

// plain is a store seen through session.Store alone, so CountActivity
// reads its logs rather than its own count.
type plain struct{ session.Store }
