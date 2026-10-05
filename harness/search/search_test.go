// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package search

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestClean: a result keeps an http or https URL, its title becomes one
// line, its title and snippet are cut at their bounds and marked, and a
// result with any other URL is dropped.
func TestClean(t *testing.T) {
	h, ok := Clean(Hit{Title: " Go 1.25\n is   released ", URL: "https://go.dev/blog", Snippet: "  a\xffb  "})
	if !ok || h != (Hit{Title: "Go 1.25 is released", URL: "https://go.dev/blog", Snippet: "a�b"}) {
		t.Fatalf("%+v %v", h, ok)
	}
	long, ok := Clean(Hit{Title: strings.Repeat("t", MaxTitleLength+1), URL: "http://x.example", Snippet: strings.Repeat("ab ", MaxSnippetLength)})
	if !ok || len([]rune(long.Title)) != MaxTitleLength || !strings.HasSuffix(long.Title, cutMark) || len([]rune(long.Snippet)) != MaxSnippetLength || !strings.HasSuffix(long.Snippet, cutMark) {
		t.Fatalf("a long result: %d, %d", len([]rune(long.Title)), len([]rune(long.Snippet)))
	}
	for _, u := range []string{"ftp://x.example/a", "/relative", "https://", ""} {
		if _, ok := Clean(Hit{Title: "T", URL: u}); ok {
			t.Errorf("%q was kept", u)
		}
	}
	if got := cut("héllo wörld", 8); got != "héllo..." {
		t.Fatalf("cut %q", got)
	}
	if (&Refused{Status: 403, Code: "c", Message: "m"}).Error() == "" {
		t.Fatal("a refusal says nothing")
	}
}

// TestTheContractPageStatesTheBounds: docs/web-search.md, the contract a
// search service's author reads, states each bound with the value of its
// constant, so a bound that changes fails here until the page says it.
func TestTheContractPageStatesTheBounds(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "web-search.md"))
	if err != nil {
		t.Fatal(err)
	}
	page := strings.Join(strings.Fields(string(raw)), " ")
	thousands := func(n int) string {
		if n < 1000 {
			return strconv.Itoa(n)
		}
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	for _, want := range []string{
		fmt.Sprintf("`query` is 1 to %d characters", MaxQueryLength),
		fmt.Sprintf("`max_results` is 1 to %d, and always present: Topos sends %d", MaxResults, DefaultResults),
		fmt.Sprintf("cuts a title past %s characters and a snippet past %s.", thousands(MaxTitleLength), thousands(MaxSnippetLength)),
		fmt.Sprintf("larger than %d MiB, or no answer within %d seconds", MaxResponseBody>>20, int(Timeout.Seconds())),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/web-search.md does not state %q", want)
		}
	}
}
