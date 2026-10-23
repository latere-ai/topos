// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import "time"

// The bounds of the trigger tool's calls (spec 086), which its schema, its
// description and the check before a call is held all read.
const (
	// MaxPurposeLength is the most characters of a proposal's why: one or
	// two sentences a person reads under the trigger's name.
	MaxPurposeLength = 200
	// MaxListed is the most triggers a list answers, newest first: enough
	// to find one the agent already proposed, and a person with more
	// manages them in a client.
	MaxListed = 50
)

// The window of a trigger's allowance and pace (spec 087).
const (
	// SpendWindow is the rolling window a trigger's spend is counted over.
	SpendWindow = 7 * 24 * time.Hour
	// SpendWindowDays is SpendWindow in days, as status.spend states it.
	SpendWindowDays = 7
	// PaceLooks is how many of the window's latest ended looks the cost
	// of a look is the mean of: recent enough to follow a look made
	// shorter, and more than one, so one long look does not set the pace.
	PaceLooks = 5
)
