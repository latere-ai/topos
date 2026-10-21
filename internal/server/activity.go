// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"time"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/session"
)

// FirstActivityDay is the earliest day GET /sessions/activity answers:
// older than any log this core wrote, and a bound on what a mistyped year
// makes the stores read.
var FirstActivityDay = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// activityDayLayout is the form of the day parameter, a date in UTC.
const activityDayLayout = time.DateOnly

// activity is GET /sessions/activity (spec 085): for one day in UTC, per
// sender in subject order, the messages a person sent from each client,
// the sessions they started from each and the answers to them that
// finished. It answers counts only, so it asks session.activity once,
// about the collection, with the day and the one sender the request
// names, and reads no session's content.
func (c *call) activity() error {
	q := c.r.URL.Query()
	raw := q.Get("day")
	if raw == "" {
		return refuse(CodeInvalidRequest, "day is required: a date in UTC, as YYYY-MM-DD")
	}
	day, err := time.Parse(activityDayLayout, raw)
	if err != nil {
		return refuse(CodeInvalidRequest, "day is %q, not a date as YYYY-MM-DD", raw)
	}
	today := c.s.o.Now().UTC().Truncate(24 * time.Hour)
	if day.Before(FirstActivityDay) || day.After(today) {
		return refuse(CodeInvalidRequest, "day is %s, not between %s and today, %s", raw, FirstActivityDay.Format(activityDayLayout), today.Format(activityDayLayout))
	}
	limit, cursor, err := c.pageParams()
	if err != nil {
		return err
	}
	after, err := store.Uncursor(cursor)
	if err != nil {
		return refuse(CodeInvalidRequest, "the cursor was not issued by this server")
	}
	sender := q.Get("sender")
	fields := map[string]any{"day": raw}
	if sender != "" {
		fields["sender"] = sender
	}
	ctx := c.r.Context()
	if _, err := c.ask(ctx, authorizer.ActionSessionActivity, authz.NewResource(authorizer.KindSession, "", fields)); err != nil {
		return err
	}
	// One sender past the page says whether another page follows.
	found, err := session.CountActivity(ctx, c.s.o.Sessions, session.ActivityOptions{
		From: day, To: day.Add(24 * time.Hour), Sender: sender, After: after, Limit: limit + 1,
	})
	if err != nil {
		return err
	}
	var next string
	if len(found) > limit {
		found = found[:limit]
		next = store.Cursor(found[limit-1].Sender)
	}
	return c.replyPage(found, next)
}
