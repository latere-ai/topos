// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"slices"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

// MaxTimes is the most times a trigger's spec.times may list (spec 090).
const MaxTimes = 100

// Times are the times s lists, in UTC and the soonest first, nil for a
// trigger that lists none. The spec keeps them as written, in its order
// and with its offsets, so a list read back and sent again applies with
// the same digest.
func Times(s v1.TriggerSpec) []time.Time {
	if len(s.Times) == 0 {
		return nil
	}
	out := make([]time.Time, len(s.Times))
	for i, t := range s.Times {
		out[i] = t.UTC()
	}
	slices.SortFunc(out, time.Time.Compare)
	return out
}

// NextTime is the soonest time s lists after at, nil when it lists none
// after it.
func NextTime(s v1.TriggerSpec, at time.Time) *time.Time {
	for _, t := range Times(s) {
		if t.After(at) {
			return &t
		}
	}
	return nil
}

// DueTime is the listed time a firing of s at at is the firing of: the
// latest listed time at or before at, which is at itself unless at is
// when one that waits is tried again; at for a time before every one.
func DueTime(s v1.TriggerSpec, at time.Time) time.Time {
	due := at
	for _, t := range Times(s) {
		if t.After(at) {
			break
		}
		due = t
	}
	return due
}

// LastTime reports whether at is the latest time s lists.
func LastTime(s v1.TriggerSpec, at time.Time) bool {
	ts := Times(s)
	return len(ts) > 0 && ts[len(ts)-1].Equal(at)
}
