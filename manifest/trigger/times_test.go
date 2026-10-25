// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"slices"
	"testing"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

// TestTheTimesOfAList (spec 090): a list's times read soonest first in
// UTC whatever their order and offsets, the spec left as written; the
// next time after a moment is the soonest after it; the due time of a
// moment is the latest listed at or before it, or the moment itself
// before every one; and only the latest time is the last.
func TestTheTimesOfAList(t *testing.T) {
	base := time.Date(2026, 10, 26, 17, 40, 0, 0, time.UTC)
	a, b, c := base, base.Add(62*time.Hour+20*time.Minute), base.Add(8*24*time.Hour)
	spec := v1.TriggerSpec{Times: []time.Time{c, a.In(time.FixedZone("CET", 3600)), b}}
	if got := Times(spec); !slices.EqualFunc(got, []time.Time{a, b, c}, func(x, y time.Time) bool { return x.Equal(y) && x.Location() == time.UTC }) {
		t.Fatalf("the times read %v", got)
	}
	if !spec.Times[0].Equal(c) || spec.Times[1].Location() == time.UTC {
		t.Fatalf("reading the times changed the spec: %v", spec.Times)
	}
	if Times(v1.TriggerSpec{}) != nil || NextTime(v1.TriggerSpec{}, base) != nil || LastTime(v1.TriggerSpec{}, base) {
		t.Fatal("a trigger with no list has times")
	}
	for _, c := range []struct {
		at   time.Time
		next *time.Time
		due  time.Time
		last bool
	}{
		{a.Add(-time.Hour), &a, a.Add(-time.Hour), false},
		{a, &b, a, false},
		{a.Add(time.Hour), &b, a, false},
		{c, nil, c, true},
	} {
		if n := NextTime(spec, c.at); (n == nil) != (c.next == nil) || n != nil && !n.Equal(*c.next) {
			t.Errorf("the next time after %v is %v", c.at, n)
		}
		if d := DueTime(spec, c.at); !d.Equal(c.due) {
			t.Errorf("the due time of %v is %v", c.at, d)
		}
		if LastTime(spec, c.at) != c.last {
			t.Errorf("%v is the last: %v", c.at, !c.last)
		}
	}
}
