// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts_test

import (
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/prompts"
)

// TestReadParsedDescriptionHoldsTheBounds (spec 080): the description of
// a read that reads a PDF through a parsing service states the default
// selection and the pages a parse reads with the values of their
// constants.
func TestReadParsedDescriptionHoldsTheBounds(t *testing.T) {
	d := prompts.Text(prompts.ToolReadParsed)
	for _, want := range []string{
		fmt.Sprintf("Default `%s`.", parsing.DefaultSelection),
		fmt.Sprintf("Only the first %d pages of a longer PDF are read.", parsing.MaxPages),
		"`<!-- page N -->`", "which `pages` to ask for next",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the description does not state %q", want)
		}
	}
}
