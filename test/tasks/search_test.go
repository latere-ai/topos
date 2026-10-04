// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/search"
)

// TestTheCannedSearch: the suite's search service answers the task's
// results with ${SERVE_URL} expanded, at most the count asked, at no
// cost, and refuses a list longer than a search answers.
func TestTheCannedSearch(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"r.yaml": "- {title: A, url: '${SERVE_URL}/a', snippet: first}\n- {title: B, url: 'https://example.com/b'}\n"})
	hits, err := readHits(filepath.Join(dir, "r.yaml"), "http://127.0.0.1:9")
	if err != nil {
		t.Fatal(err)
	}
	res, err := cannedSearch{hits: hits}.Search(t.Context(), search.Request{Query: "q", MaxResults: 1})
	if err != nil || len(res.Hits) != 1 || res.Hits[0] != (search.Hit{Title: "A", URL: "http://127.0.0.1:9/a", Snippet: "first"}) || res.CostUSDMicro != nil {
		t.Fatalf("%+v %v", res, err)
	}
	var long strings.Builder
	for i := range search.MaxResults + 1 {
		fmt.Fprintf(&long, "- {title: T%d, url: 'https://example.com/%d'}\n", i, i)
	}
	writeFiles(t, dir, map[string]string{"long.yaml": long.String()})
	if _, err := readHits(filepath.Join(dir, "long.yaml"), ""); err == nil {
		t.Fatal("a list longer than a search answers was read")
	}
}
