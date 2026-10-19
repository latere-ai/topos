// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts_test

import (
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/prompts"
)

// TestHistoryDescriptionHoldsTheBounds (spec 084): the history
// description states a query's bounds with the harness's constants, the
// three reads by their inputs, and what spec 084 asks of it: use it for
// what a cleared result or a summary let go, and that a read costs a
// call.
func TestHistoryDescriptionHoldsTheBounds(t *testing.T) {
	d := prompts.Text(prompts.ToolHistory)
	for _, want := range []string{
		fmt.Sprintf("An answer holds at most %d matches, each with at most %d characters of its text.", harness.HistoryMatches, harness.HistoryMatchChars),
		"`{\"tool_use_id\": \"toolu_...\"}`", "`{\"from\": 120, \"to\": 180}`", "`{\"query\": \"ECONNREFUSED\"}`",
		"An old tool result may be cleared", "replaced by a summary", "A cleared result names its call's id",
		"Each read is a call", "the input that reads on",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the description does not state %q", want)
		}
	}
}
