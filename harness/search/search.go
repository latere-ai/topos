// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package search is the contract of the search service web_search sends
// its queries to (spec 047): the request, the results a model can cite,
// a refusal and a failure, the bounds of a call, and the Searcher a
// runner hands the tool. It dials nothing (spec 001): the HTTP client of
// the contract is toposd's, and an embedder may hand the tool any
// Searcher. The contract names no provider.
package search

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// The bounds of spec 047. The tool's schema and its description are
// rendered from them or held to them by a test.
const (
	// MaxQueryLength is the most characters a query holds.
	MaxQueryLength = 400
	// DefaultResults is the count asked when a call names none.
	DefaultResults = 5
	// MaxResults is the most results a call asks for.
	MaxResults = 10
	// MaxTitleLength and MaxSnippetLength are the most characters of a
	// result's title and snippet that are kept; a longer one is cut and
	// marked.
	MaxTitleLength   = 300
	MaxSnippetLength = 1000
	// Timeout bounds one search, from the request to the end of the body.
	Timeout = 30 * time.Second
	// MaxResponseBody is the most bytes of an answer that are read.
	MaxResponseBody = 1 << 20
)

// cutMark ends a title or a snippet that was cut.
const cutMark = "..."

// Request is one search.
type Request struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

// Hit is one result.
type Hit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

// Result is a service's answer: the results in its order, at most the
// count asked, each with an http or https URL, and what the search was
// charged, nil when the service reports nothing.
type Result struct {
	Hits         []Hit
	CostUSDMicro *int64
}

// Refused is a search the service refused with the error envelope: its
// code, its sentence for the person, and when it may be tried again
// after a 429 that names it.
type Refused struct {
	Status     int
	Code       string
	Message    string
	RetryAfter time.Duration
}

func (r *Refused) Error() string {
	return fmt.Sprintf("search: the service refused with %d %s: %s", r.Status, r.Code, r.Message)
}

// ErrFailed is a search the service did not answer as the contract says:
// a 5xx, a refusal without the envelope, a body that does not decode or
// is past MaxResponseBody, or a transport error. A timeout wraps
// context.DeadlineExceeded as well.
var ErrFailed = errors.New("search: the search failed")

// Searcher runs one search.
type Searcher interface {
	Search(ctx context.Context, r Request) (Result, error)
}

// CheckURL reports whether u is an absolute http or https URL with a
// host: a search service's address, or a result's.
func CheckURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return fmt.Errorf("search: %q is not an absolute http or https URL", u)
	}
	return nil
}

// Clean is h as a model reads it, and false for a result to drop: one
// whose URL is not http or https. The title is one line of valid UTF-8
// and the snippet valid UTF-8, each cut at its bound.
func Clean(h Hit) (Hit, bool) {
	if CheckURL(h.URL) != nil {
		return Hit{}, false
	}
	return Hit{
		Title:   cut(strings.Join(strings.Fields(strings.ToValidUTF8(h.Title, "�")), " "), MaxTitleLength),
		URL:     h.URL,
		Snippet: cut(strings.TrimSpace(strings.ToValidUTF8(h.Snippet, "�")), MaxSnippetLength),
	}, true
}

// cut keeps at most n characters of s, ending a cut one with cutMark.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:n-utf8.RuneCountInString(cutMark)]), " ") + cutMark
}
