// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts_test

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"latere.ai/x/topos/harness/search"
	"latere.ai/x/topos/prompts"
)

// TestWebSearchDescriptionHoldsTheBounds: the web_search description
// states each bound of a call with the value of its constant and holds
// no other number, and it says what spec 042's table asks of it: search
// for an address the agent lacks, fetch one it has, cite the URLs, that
// a search may be charged, and that a refusal is for the person.
func TestWebSearchDescriptionHoldsTheBounds(t *testing.T) {
	d := prompts.Text(prompts.ToolWebSearch)
	for _, want := range []string{
		fmt.Sprintf("`query`: what to search for, at most %d characters", search.MaxQueryLength),
		fmt.Sprintf("`max_results`: how many results to return, 1 to %d. The default is %d.", search.MaxResults, search.DefaultResults),
		"fetch it with `web_fetch` instead", "read it in full", "Cite the URL", "may be charged", "Tell them what it says",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("the description does not state %q", want)
		}
	}
	bounds := []int{1, search.MaxQueryLength, search.MaxResults, search.DefaultResults}
	for _, m := range regexp.MustCompile(`[0-9]+`).FindAllString(d, -1) {
		n, err := strconv.Atoi(m)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(bounds, n) {
			t.Errorf("the description holds the number %d, which is no bound of a search", n)
		}
	}
}
