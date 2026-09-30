// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"testing"
	"time"
)

func mustSchedule(t *testing.T, s string) Schedule {
	t.Helper()
	sc, err := ParseSchedule(s)
	if err != nil {
		t.Fatalf("%q: %v", s, err)
	}
	return sc
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestScheduleNext(t *testing.T) {
	utc := time.UTC
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct{ schedule, after, want string }{
		{"@hourly", "2026-09-30T08:00:00Z", "2026-09-30T09:00:00Z"},
		{"@hourly", "2026-09-30T08:59:59Z", "2026-09-30T09:00:00Z"},
		{"@daily", "2026-09-30T08:00:00Z", "2026-10-01T00:00:00Z"},
		{"@weekly", "2026-09-30T08:00:00Z", "2026-10-04T00:00:00Z"},
		{"*/15 * * * *", "2026-09-30T08:07:00Z", "2026-09-30T08:15:00Z"},
		{"5-7 9 * * *", "2026-09-30T09:06:00Z", "2026-09-30T09:07:00Z"},
		{"50/5 * * * *", "2026-09-30T09:52:00Z", "2026-09-30T09:55:00Z"},
		{"0 0 1,15 * *", "2026-09-30T08:00:00Z", "2026-10-01T00:00:00Z"},
		{"0 0 * 2 *", "2026-09-30T08:00:00Z", "2027-02-01T00:00:00Z"},
		// Day of month and day of week both restricted: either matches.
		{"0 0 13 * 5", "2026-09-30T08:00:00Z", "2026-10-02T00:00:00Z"},
		// 7 is Sunday, as 0 is.
		{"0 12 * * 7", "2026-09-30T08:00:00Z", "2026-10-04T12:00:00Z"},
		{"0 0 29 2 *", "2026-09-30T08:00:00Z", "2028-02-29T00:00:00Z"},
	}
	for _, c := range cases {
		if got := mustSchedule(t, c.schedule).Next(at(c.after), utc); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s: %s, want %s", c.schedule, c.after, got, c.want)
		}
	}
	if got := mustSchedule(t, "0 0 30 2 *").Next(at("2026-09-30T08:00:00Z"), utc); !got.IsZero() {
		t.Errorf("the 30th of February fires at %s", got)
	}
	for _, bad := range []string{"", "* * *", "0 25 * * *", "*/0 * * * *", "5-2 * * * *", "x * * * *", "0 0 32 * *", "0 0 * 13 *", "0 0 * * 8", "@yearly", "1-x * * * *", "1/x * * * *"} {
		if _, err := ParseSchedule(bad); err == nil {
			t.Errorf("%q parses", bad)
		}
	}
}

// TestDaylightSavingRules: in a zone that changes to daylight saving
// time, a local time the change skips fires at the next valid minute,
// and a local time the change back repeats fires once.
func TestDaylightSavingRules(t *testing.T) {
	berlin := zone(t, "Europe/Berlin")
	local := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, berlin) }
	daily := mustSchedule(t, "30 2 * * *")

	// On 2026-03-29 the clock goes from 02:00 to 03:00: 02:30 does not
	// exist, and the firing is at 03:00, the next valid minute.
	got := daily.Next(local(2026, 3, 28, 12, 0), berlin)
	if want := time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("the skipped 02:30 fires at %s, want %s (03:00 local)", got.In(berlin), want.In(berlin))
	}
	if next := daily.Next(got, berlin); !next.Equal(time.Date(2026, 3, 30, 0, 30, 0, 0, time.UTC)) {
		t.Fatalf("the day after the skipped 02:30 fires at %s", next.In(berlin))
	}

	// On 2026-10-25 the clock goes from 03:00 back to 02:00: 02:30 occurs
	// twice and fires at its first occurrence only.
	first := daily.Next(local(2026, 10, 24, 12, 0), berlin)
	if want := time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC); !first.Equal(want) {
		t.Fatalf("the repeated 02:30 first fires at %s, want %s", first, want)
	}
	if next := daily.Next(first, berlin); !next.Equal(time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC)) {
		t.Fatalf("the repeated 02:30 fires again at %s; want the next day's", next.In(berlin))
	}

	// Every minute through the repeated hour: each local minute fires
	// once, so the hour after the change back fires nothing until 03:00.
	minutely := mustSchedule(t, "* * * * *")
	late := time.Date(2026, 10, 25, 0, 59, 0, 0, time.UTC) // 02:59 summer time
	if next := minutely.Next(late, berlin); !next.Equal(time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("after 02:59 summer time the next minute is %s, want 03:00 winter time", next.In(berlin))
	}
	seen := map[string]int{}
	for at := local(2026, 10, 25, 1, 0); at.Before(time.Date(2026, 10, 25, 2, 30, 0, 0, time.UTC)); at = minutely.Next(at, berlin) {
		seen[at.In(berlin).Format("15:04")]++
	}
	for minute, n := range seen {
		if n != 1 {
			t.Fatalf("%s fired %d times", minute, n)
		}
	}
	// And the hourly schedule through the skipped hour: 02:00 does not
	// exist and fires at 03:00, and 03:00 itself is the same minute.
	hourly := mustSchedule(t, "0 * * * *")
	if next := hourly.Next(local(2026, 3, 29, 1, 30), berlin); !next.Equal(time.Date(2026, 3, 29, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("the skipped 02:00 fires at %s", next.In(berlin))
	}
}
