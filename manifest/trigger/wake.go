// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"fmt"
	"time"
)

// The bounds of a session's wake (spec 086), which the wake tool's
// schema, its description, the server's check of a trigger a wake applies
// and the API's description all read, so none is written twice.
const (
	// MinWakeDelay is how soon a wake may be: at the floor, ten wakes in a
	// row take more than an hour and a half before the person is asked.
	MinWakeDelay = 10 * time.Minute
	// MaxWakeAhead is how far ahead a wake may be: a look further off is
	// standing work, which the agent proposes and the person approves.
	MaxWakeAhead = 168 * time.Hour
	// MaxWakesWithoutPerson is how many wakes in a row may fire with no
	// message, answer or confirmation from the session's initiator; the
	// next set waits for that person.
	MaxWakesWithoutPerson = 10
	// WakeMaxAge is how late a wake may fire, after a stop of the server;
	// a later firing is skipped_late.
	WakeMaxAge = 24 * time.Hour
	// MaxWakeTitle bounds a wake's title, the line a person reads, in
	// characters.
	MaxWakeTitle = 80
	// MaxWakeNote bounds a wake's note, what the agent tells itself, in
	// characters.
	MaxWakeNote = 4000
)

// The codes a wake's time outside its bounds is refused with.
const (
	CodeTooSoon = "too_soon"
	CodeTooLate = "too_late"
)

// WakeError is a wake's time outside its bounds: Code is CodeTooSoon with
// the earliest time a wake may be as Bound, or CodeTooLate with the latest.
type WakeError struct {
	Code  string
	Bound time.Time
}

func (e *WakeError) Error() string {
	if e.Code == CodeTooSoon {
		return fmt.Sprintf("%s: a wake is at %s at the earliest", e.Code, e.Bound.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s: a wake is at %s at the latest", e.Code, e.Bound.UTC().Format(time.RFC3339))
}

// WakeWindow is the earliest and the latest time a wake set at now may
// be, in a session whose life ends at expires: MinWakeDelay from now, and
// MaxWakeAhead from now or the session's end, whichever is sooner. A zero
// expires bounds nothing.
func WakeWindow(now, expires time.Time) (earliest, latest time.Time) {
	earliest, latest = now.Add(MinWakeDelay), now.Add(MaxWakeAhead)
	if !expires.IsZero() && expires.Before(latest) {
		latest = expires
	}
	return earliest.UTC(), latest.UTC()
}

// CheckWake refuses a wake at at, set at now in a session whose life ends
// at expires, outside WakeWindow, as a *WakeError naming the bound. A
// session that ends before the earliest time takes no wake: every time is
// too late.
func CheckWake(at, now, expires time.Time) error {
	earliest, latest := WakeWindow(now, expires)
	switch {
	case latest.Before(earliest) || at.After(latest):
		return &WakeError{Code: CodeTooLate, Bound: latest}
	case at.Before(earliest):
		return &WakeError{Code: CodeTooSoon, Bound: earliest}
	}
	return nil
}
