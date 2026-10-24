// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import "time"

// The bounds of a trigger's run record and of the read that answers it
// (spec 088), which the server, the OpenAPI text and the tests all read.
const (
	// RunsKept is how long a run is kept: a month's forecast reads four
	// weeks back at most, and the mean of five runs of a weekly schedule
	// needs five weeks.
	RunsKept = 6 * 7 * 24 * time.Hour
	// RunsSinceDefault is how far back the read answers runs when it is
	// not told: the four weeks a rate of runs is read over.
	RunsSinceDefault = 28 * 24 * time.Hour
	// RunsAhead is how far ahead the read answers fire times at most: the
	// rest of a month and the next, so a client sees past the month's end.
	RunsAhead = 62 * 24 * time.Hour
	// MaxRunsListed is the most runs the read answers for one trigger, the
	// newest kept: a run every 20 minutes for RunsKept. A trigger that
	// fires more often is too frequent to be read run by run.
	MaxRunsListed = 3000
	// MaxRunFires is the most fire times the read answers for one
	// trigger, the soonest kept.
	MaxRunFires = 3000
)

// RunsName is the path segment of the read of an owner's runs,
// /triggers/runs (spec 088), which no trigger takes as its name.
const RunsName = "runs"
