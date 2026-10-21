// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"
)

// ClientCount is how many of something came from one OAuth client, ""
// for a token that named none (spec 085).
type ClientCount struct {
	Client string `json:"client"`
	Count  int    `json:"count"`
}

// SenderActivity is one sender's counts over a window (spec 085): the
// messages they sent and the sessions they started, each by the client
// that sent it, and the turns that answered them and ended normally. It
// holds counts only, never a word of what was said.
type SenderActivity struct {
	Sender   string        `json:"sender"`
	Messages []ClientCount `json:"messages"`
	Sessions []ClientCount `json:"sessions"`
	Answers  int           `json:"answers"`
}

// ActivityOptions is the window an activity count reads: events and
// sessions from From up to, not including, To. Sender, when set, keeps
// that sender alone; After keeps the senders after it in subject order,
// and Limit, when positive, keeps that many of them.
type ActivityOptions struct {
	From, To time.Time
	Sender   string
	After    string
	Limit    int
}

// Activity is the optional interface of a store that counts a window's
// activity without reading every log (spec 085). It answers what
// CountActivity answers for the same store, senders in subject order.
type Activity interface {
	Activity(ctx context.Context, o ActivityOptions) ([]SenderActivity, error)
}

// activityPage is the page CountActivity reads the store's list and logs
// in.
const activityPage = 500

// CountActivity counts a window's activity by reading every session and
// its log: the count of a store that keeps no index of time, and the
// reference the store suite holds an Activity to. The rules are spec
// 085's, one function per rule: PersonMessage, StartedByPerson and
// FinishedAnswer.
func CountActivity(ctx context.Context, st Store, o ActivityOptions) ([]SenderActivity, error) {
	if a, ok := st.(Activity); ok {
		return a.Activity(ctx, o)
	}
	tally := newActivityTally(o)
	var cursor string
	for {
		page, next, err := st.List(ctx, ListOptions{Limit: activityPage, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, s := range page {
			if StartedByPerson(s) && within(s.CreatedAt, o) {
				tally.session(s.Initiator)
			}
			if s.Rollout != "" {
				continue
			}
			// A session deleted between the list and its read has no
			// log left to count.
			if err := tally.log(ctx, st, s.ID); err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return tally.result(), nil
}

// PersonMessage reports whether e is a message a person sent on a
// session's own thread and still on the record: a user.message of a
// sender of kind person with no trigger's firing, in thread "", not
// redacted. It answers the message's payload when it is one.
func PersonMessage(e Event) (UserMessage, bool) {
	var m UserMessage
	if e.Type != TypeUserMessage || e.Thread != "" || e.Redacted() || e.Decode(&m) != nil {
		return UserMessage{}, false
	}
	return m, m.Sender.Kind == SenderPerson && m.FiringID == ""
}

// StartedByPerson reports whether a person started s: its initiator is
// of kind person, no trigger started it and it is no run of a rollout.
func StartedByPerson(s Session) bool {
	return s.Initiator.Kind == SenderPerson && s.TriggerID == "" && s.Rollout == ""
}

// FinishedAnswer reports whether e ends a turn normally on a session's
// own thread: a session.status a turn wrote (its turn is set) that is
// idle at end_turn, or ended as completed for an agent that ends on idle.
// A session ended through the API carries no turn and finishes nothing.
func FinishedAnswer(e Event) bool {
	var p SessionStatus
	if e.Type != TypeSessionStatus || e.Thread != "" || e.Turn == 0 || e.Redacted() || e.Decode(&p) != nil {
		return false
	}
	return (p.Status == StatusIdle && p.StopReason == StopEndTurn) || (p.Status == StatusEnded && p.StopReason == StopCompleted)
}

// within reports whether at is in o's window.
func within(at time.Time, o ActivityOptions) bool {
	return !at.Before(o.From) && at.Before(o.To)
}

// activityTally gathers one window's counts by sender.
type activityTally struct {
	o       ActivityOptions
	senders map[string]*senderTally
}

type senderTally struct {
	messages, sessions map[string]int
	answers            int
}

func newActivityTally(o ActivityOptions) *activityTally {
	return &activityTally{o: o, senders: map[string]*senderTally{}}
}

// of is the tally of subject, or nil for one the options leave out.
func (t *activityTally) of(subject string) *senderTally {
	if subject == "" || (t.o.Sender != "" && subject != t.o.Sender) || (t.o.After != "" && subject <= t.o.After) {
		return nil
	}
	s := t.senders[subject]
	if s == nil {
		s = &senderTally{messages: map[string]int{}, sessions: map[string]int{}}
		t.senders[subject] = s
	}
	return s
}

func (t *activityTally) session(by Sender) {
	if s := t.of(by.Subject); s != nil {
		s.sessions[by.Client]++
	}
}

// log counts one session's messages and answers in the window. An answer
// is counted for the sender of the last message on the session's own
// thread before it, when that message is a person's.
func (t *activityTally) log(ctx context.Context, st Store, id string) error {
	var last *Event
	var from uint64
	for {
		events, err := st.Events(ctx, id, from, activityPage)
		if err != nil {
			return err
		}
		for i := range events {
			e := events[i]
			if e.Type == TypeUserMessage && e.Thread == "" {
				last = &e
				if m, ok := PersonMessage(e); ok && within(e.Time, t.o) {
					if s := t.of(m.Sender.Subject); s != nil {
						s.messages[m.Sender.Client]++
					}
				}
				continue
			}
			if last == nil || !FinishedAnswer(e) || !within(e.Time, t.o) {
				continue
			}
			if m, ok := PersonMessage(*last); ok {
				if s := t.of(m.Sender.Subject); s != nil {
					s.answers++
				}
			}
		}
		if len(events) < activityPage {
			return nil
		}
		from = events[len(events)-1].Seq + 1
	}
}

// result is the tally in subject order, cut at the options' limit.
func (t *activityTally) result() []SenderActivity {
	subjects := slices.Sorted(maps.Keys(t.senders))
	if t.o.Limit > 0 && len(subjects) > t.o.Limit {
		subjects = subjects[:t.o.Limit]
	}
	out := make([]SenderActivity, 0, len(subjects))
	for _, subject := range subjects {
		s := t.senders[subject]
		out = append(out, SenderActivity{Sender: subject, Messages: ClientCounts(s.messages), Sessions: ClientCounts(s.sessions), Answers: s.answers})
	}
	return out
}

// ClientCounts is a count per client in client order, never nil: the
// form every Activity answers its counts in.
func ClientCounts(by map[string]int) []ClientCount {
	out := make([]ClientCount, 0, len(by))
	for client, n := range by {
		out = append(out, ClientCount{Client: client, Count: n})
	}
	slices.SortFunc(out, func(a, b ClientCount) int { return cmp.Compare(a.Client, b.Client) })
	return out
}
