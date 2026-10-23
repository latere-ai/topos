// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"errors"
	"testing"
	"time"
)

// TestAWakeLivesInsideItsBounds: a wake is at least MinWakeDelay ahead,
// at most MaxWakeAhead ahead and no later than its session's end, and a
// refusal names its bound; a session ending before the earliest time
// takes none.
func TestAWakeLivesInsideItsBounds(t *testing.T) {
	now := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	week := now.Add(7 * 24 * time.Hour)
	cases := []struct {
		name        string
		at, expires time.Time
		code        string
		bound       time.Time
	}{
		{"the floor", now.Add(MinWakeDelay), week.Add(time.Hour), "", time.Time{}},
		{"a minute short of it", now.Add(MinWakeDelay - time.Minute), week, CodeTooSoon, now.Add(MinWakeDelay)},
		{"the ceiling", now.Add(MaxWakeAhead), time.Time{}, "", time.Time{}},
		{"past the ceiling", now.Add(MaxWakeAhead + time.Minute), time.Time{}, CodeTooLate, now.Add(MaxWakeAhead)},
		{"past the session's end", now.Add(48 * time.Hour), now.Add(24 * time.Hour), CodeTooLate, now.Add(24 * time.Hour)},
		{"a session about to end", now.Add(MinWakeDelay), now.Add(time.Minute), CodeTooLate, now.Add(time.Minute)},
	}
	for _, c := range cases {
		err := CheckWake(c.at, now, c.expires)
		if c.code == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		we, ok := errors.AsType[*WakeError](err)
		if !ok || we.Code != c.code || !we.Bound.Equal(c.bound) {
			t.Errorf("%s: %v, want %s at %s", c.name, err, c.code, c.bound)
		}
	}
	if e := (&WakeError{Code: CodeTooSoon, Bound: now}).Error(); e != "too_soon: a wake is at 2026-10-23T09:00:00Z at the earliest" {
		t.Errorf("too soon reads %q", e)
	}
	if e := (&WakeError{Code: CodeTooLate, Bound: now}).Error(); e != "too_late: a wake is at 2026-10-23T09:00:00Z at the latest" {
		t.Errorf("too late reads %q", e)
	}
}
