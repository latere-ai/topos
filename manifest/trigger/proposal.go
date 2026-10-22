// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

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
