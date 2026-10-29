// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/topos/session"
)

// The three counts of a window's activity (spec 085), each grouped by the
// subject it is counted for. They apply session.PersonMessage,
// session.StartedByPerson and session.FinishedAnswer in SQL, over the rows
// the events_activity and sessions_created indexes (migrations 0015 and
// 0017) find by time; the
// store suite holds them to session.CountActivity. $1 and $2 are the
// window, $3 the one sender or "", $4 the subject the page starts after,
// compared byte by byte (COLLATE "C") as Go orders the page, so the cut
// at a page's end and the next page's start agree whatever the
// database's collation.
const (
	activityMessages = `
SELECT m.p->'sender'->>'subject', COALESCE(m.p->'sender'->>'client', ''), count(*)
  FROM (SELECT e.payload::jsonb AS p
          FROM events e JOIN sessions s ON s.id = e.session_id
         WHERE e.type = 'user.message' AND e.subagent = '' AND NOT e.redacted
           AND e.time >= $1 AND e.time < $2 AND s.rollout IS NULL) m
 WHERE m.p->'sender'->>'kind' = 'person' AND COALESCE(m.p->>'firing_id', '') = ''
   AND ($3 = '' OR m.p->'sender'->>'subject' = $3) AND (m.p->'sender'->>'subject') COLLATE "C" > $4
 GROUP BY 1, 2`

	activitySessions = `
SELECT s.b->'initiator'->>'subject', COALESCE(s.b->'initiator'->>'client', ''), count(*)
  FROM (SELECT body::jsonb AS b FROM sessions
         WHERE created_at >= $1 AND created_at < $2 AND trigger_id IS NULL AND rollout IS NULL) s
 WHERE s.b->'initiator'->>'kind' = 'person'
   AND ($3 = '' OR s.b->'initiator'->>'subject' = $3) AND (s.b->'initiator'->>'subject') COLLATE "C" > $4
 GROUP BY 1, 2`

	activityAnswers = `
SELECT last.p->'sender'->>'subject', count(*)
  FROM (SELECT st.session_id, st.seq, st.payload::jsonb AS p
          FROM events st JOIN sessions s ON s.id = st.session_id
         WHERE st.type = 'session.status' AND st.subagent = '' AND NOT st.redacted AND st.turn > 0
           AND st.time >= $1 AND st.time < $2 AND s.rollout IS NULL) ended
 CROSS JOIN LATERAL (
       SELECT u.payload::jsonb AS p
         FROM events u
        WHERE u.session_id = ended.session_id AND u.seq < ended.seq
          AND u.type = 'user.message' AND u.subagent = ''
        ORDER BY u.seq DESC LIMIT 1) last
 WHERE ((ended.p->>'status' = 'idle' AND ended.p->>'stop_reason' = 'end_turn')
        OR (ended.p->>'status' = 'ended' AND ended.p->>'stop_reason' = 'completed'))
   AND last.p->'sender'->>'kind' = 'person' AND COALESCE(last.p->>'firing_id', '') = ''
   AND ($3 = '' OR last.p->'sender'->>'subject' = $3) AND (last.p->'sender'->>'subject') COLLATE "C" > $4
 GROUP BY 1`
)

// Activity counts a window's activity by sender (spec 085), as
// session.CountActivity does from every log.
func (s *Store) Activity(ctx context.Context, o session.ActivityOptions) ([]session.SenderActivity, error) {
	type tally struct {
		messages, sessions map[string]int
		answers            int
	}
	senders := map[string]*tally{}
	of := func(subject string) *tally {
		t := senders[subject]
		if t == nil {
			t = &tally{messages: map[string]int{}, sessions: map[string]int{}}
			senders[subject] = t
		}
		return t
	}
	args := []any{o.From, o.To, o.Sender, o.After}
	byClient := func(query, what string, into func(t *tally) map[string]int) error {
		rows, err := s.pool.Query(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("postgres: count the %s of a day: %w", what, err)
		}
		var subject, client string
		var n int64
		_, err = pgx.ForEachRow(rows, []any{&subject, &client, &n}, func() error {
			into(of(subject))[client] += int(n)
			return nil
		})
		if err != nil {
			return fmt.Errorf("postgres: count the %s of a day: %w", what, err)
		}
		return nil
	}
	if err := byClient(activityMessages, "messages", func(t *tally) map[string]int { return t.messages }); err != nil {
		return nil, err
	}
	if err := byClient(activitySessions, "sessions", func(t *tally) map[string]int { return t.sessions }); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, activityAnswers, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: count the answers of a day: %w", err)
	}
	var subject string
	var n int64
	if _, err := pgx.ForEachRow(rows, []any{&subject, &n}, func() error {
		of(subject).answers += int(n)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("postgres: count the answers of a day: %w", err)
	}
	subjects := slices.Sorted(maps.Keys(senders))
	if o.Limit > 0 && len(subjects) > o.Limit {
		subjects = subjects[:o.Limit]
	}
	out := make([]session.SenderActivity, 0, len(subjects))
	for _, subject := range subjects {
		t := senders[subject]
		out = append(out, session.SenderActivity{Sender: subject, Messages: session.ClientCounts(t.messages), Sessions: session.ClientCounts(t.sessions), Answers: t.answers})
	}
	return out, nil
}
