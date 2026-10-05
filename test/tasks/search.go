// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"context"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/harness/search"
)

// cannedSearch is the suite's search service (spec 042): every search
// is answered with the task's results, at most the count asked, and
// costs nothing, so a run's spend is its model requests'.
type cannedSearch struct {
	hits []search.Hit
}

func (c cannedSearch) Search(_ context.Context, r search.Request) (search.Result, error) {
	n := min(len(c.hits), r.MaxResults)
	return search.Result{Hits: append([]search.Hit{}, c.hits[:n]...)}, nil
}

// readHits reads a task's search results, a YAML list of {title, url,
// snippet}, with ${SERVE_URL} expanded to serveURL; each needs a title
// and an http or https URL once expanded, and the list holds at most
// search.MaxResults.
func readHits(path, serveURL string) ([]search.Hit, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if serveURL == "" {
		serveURL = "http://127.0.0.1"
	}
	var written []struct {
		Title   string `yaml:"title"`
		URL     string `yaml:"url"`
		Snippet string `yaml:"snippet"`
	}
	if err := yaml.UnmarshalWithOptions(expand(raw, "", serveURL), &written, yaml.Strict()); err != nil {
		return nil, err
	}
	if len(written) > search.MaxResults {
		return nil, fmt.Errorf("%d results, past the %d a search answers", len(written), search.MaxResults)
	}
	hits := make([]search.Hit, len(written))
	for i, h := range written {
		if h.Title == "" || search.CheckURL(h.URL) != nil {
			return nil, fmt.Errorf("result %d needs a title and an http or https URL", i)
		}
		hits[i] = search.Hit{Title: h.Title, URL: h.URL, Snippet: h.Snippet}
	}
	return hits, nil
}
