// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package search

import (
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
