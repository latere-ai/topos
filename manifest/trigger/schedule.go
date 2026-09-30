// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed schedule: a five-field cron expression (minute,
// hour, day of month, month, day of week, where 0 and 7 are Sunday) or
// one of the macros @hourly, @daily and @weekly.
type Schedule struct {
	// fields holds, per field, the values that match as bits.
	fields [5]uint64
	// dom and dow record whether the day-of-month and day-of-week fields
	// are restricted: when both are, a day matches when either does, as
	// cron reads them.
	dom, dow bool
}

// cronFields are the bounds of the five cron fields.
var cronFields = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}

// macros are the schedules spec 022 names by a word.
var macros = map[string]string{
	"@hourly": "0 * * * *",
	"@daily":  "0 0 * * *",
	"@weekly": "0 0 * * 0",
}

// errSchedule is every schedule that does not parse.
var errSchedule = errors.New("not a five-field cron expression or @hourly, @daily, @weekly")

// ParseSchedule reads a schedule: five fields of *, numbers, ranges a-b
// and lists, each item with an optional /step, where a/step runs from a
// to the field's end; or a macro.
func ParseSchedule(s string) (Schedule, error) {
	if m, ok := macros[s]; ok {
		s = m
	}
	fields := strings.Fields(s)
	if len(fields) != len(cronFields) {
		return Schedule{}, errSchedule
	}
	var sc Schedule
	for i, f := range fields {
		lo, hi := cronFields[i][0], cronFields[i][1]
		for item := range strings.SplitSeq(f, ",") {
			bits, ok := cronItem(item, lo, hi)
			if !ok {
				return Schedule{}, errSchedule
			}
			sc.fields[i] |= bits
		}
	}
	// Sunday is 0 and 7.
	if sc.fields[4]&(1<<7) != 0 {
		sc.fields[4] |= 1
	}
	sc.dom, sc.dow = fields[2] != "*", fields[4] != "*"
	return sc, nil
}

// cronItem reads one item of a field and returns the values it matches.
func cronItem(item string, lo, hi int) (uint64, bool) {
	base, step, stepped := strings.Cut(item, "/")
	n := 1
	if stepped {
		var err error
		if n, err = strconv.Atoi(step); err != nil || n < 1 {
			return 0, false
		}
	}
	from, to := lo, hi
	if base != "*" {
		first, last, ranged := strings.Cut(base, "-")
		a, err := strconv.Atoi(first)
		if err != nil || a < lo || a > hi {
			return 0, false
		}
		from, to = a, a
		switch {
		case ranged:
			b, err := strconv.Atoi(last)
			if err != nil || b < a || b > hi {
				return 0, false
			}
			to = b
		case stepped:
			to = hi
		}
	}
	var bits uint64
	for v := from; v <= to; v += n {
		bits |= 1 << v
	}
	return bits, true
}

func (s Schedule) has(field, v int) bool { return s.fields[field]&(1<<v) != 0 }

// day reports whether a calendar day matches the day fields.
func (s Schedule) day(w time.Time) bool {
	dom, dow := s.has(2, w.Day()), s.has(4, int(w.Weekday()))
	if s.dom && s.dow {
		return dom || dow
	}
	return dom && dow
}

// horizon bounds the search for the next matching wall-clock minute: a
// schedule that matches no day within it, such as the 30th of February,
// never fires.
const horizon = 8 * 366 * 24 * time.Hour

// Next is the instant after which the schedule next fires in loc, read
// on loc's wall clock, and the zero time when it never fires. A wall
// time that does not exist, skipped by a change to daylight saving time,
// fires at the next valid minute, the first instant whose wall clock is
// past it; a wall time that occurs twice fires once, at its first
// occurrence.
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	w := wall(after.In(loc))
	for {
		var ok bool
		if w, ok = s.nextWall(w); !ok {
			return time.Time{}
		}
		// A wall time whose instant is not past after occurs again, or
		// lay in a skipped hour whose next valid minute has fired.
		if t := due(w, loc); t.After(after) {
			return t
		}
	}
}

// wall is t's wall clock in its location, truncated to the minute, as a
// time in UTC, which keeps the calendar with no transitions in it.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
}

// nextWall is the first wall-clock minute after w that the schedule
// matches.
func (s Schedule) nextWall(w time.Time) (time.Time, bool) {
	limit := w.Add(horizon)
	w = w.Add(time.Minute)
	for w.Before(limit) {
		switch {
		case !s.has(3, int(w.Month())):
			w = time.Date(w.Year(), w.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		case !s.day(w):
			w = time.Date(w.Year(), w.Month(), w.Day()+1, 0, 0, 0, 0, time.UTC)
		case !s.has(1, w.Hour()):
			w = time.Date(w.Year(), w.Month(), w.Day(), w.Hour()+1, 0, 0, 0, time.UTC)
		case !s.has(0, w.Minute()):
			w = w.Add(time.Minute)
		default:
			return w, true
		}
	}
	return time.Time{}, false
}

// due is the first instant whose wall clock in loc is at or past the
// wall time w: w's instant when it occurs, its first occurrence when it
// occurs twice, and the end of the skipped span when it does not occur.
func due(w time.Time, loc *time.Location) time.Time {
	guess := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), 0, 0, loc)
	var first time.Time
	// A transition lies within hours of w, so the offsets in effect half
	// a day either side are every offset w may be read in.
	for _, at := range []time.Time{guess.Add(-12 * time.Hour), guess, guess.Add(12 * time.Hour)} {
		_, offset := at.Zone()
		t := w.Add(-time.Duration(offset) * time.Second)
		if wall(t.In(loc)).Equal(w) && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	if !first.IsZero() {
		return first.In(loc)
	}
	// w was skipped: the zone in effect half a day before ends at the
	// transition that skipped it, where the wall clock resumes past w.
	_, end := guess.Add(-12 * time.Hour).ZoneBounds()
	return end.In(loc)
}
