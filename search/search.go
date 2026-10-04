// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package search is the client of the search service web_search sends
// its queries to (spec 042): one POST of a query and a result count to
// the URL an installation configures, with the credential its function
// answers at that search, and an answer of results a model can cite and
// the cost the service charged. The contract names no provider: any
// service that answers it works.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/otel"
)

// The bounds of spec 042. The tool's schema and its description are
// rendered from them or held to them by a test.
const (
	// MaxQueryLength is the most characters a query holds.
	MaxQueryLength = 400
	// DefaultResults is the count asked when a call names none.
	DefaultResults = 5
	// MaxResults is the most results a call asks for.
	MaxResults = 10
	// MaxTitleLength and MaxSnippetLength are the most characters of a
	// result's title and snippet the client keeps; a longer one is cut
	// and marked.
	MaxTitleLength   = 300
	MaxSnippetLength = 1000
	// Timeout bounds one search, from the request to the end of the body.
	Timeout = 30 * time.Second
	// MaxResponseBody is the most bytes of an answer the client reads.
	MaxResponseBody = 1 << 20
)

// cutMark ends a title or a snippet the client cut.
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

// Searcher runs one search. *Client is the one a runner holds.
type Searcher interface {
	Search(ctx context.Context, r Request) (Result, error)
}

// Credential answers the bearer of one search, "" for none.
type Credential func(ctx context.Context) (string, error)

// Client is the search service at one URL.
type Client struct {
	url  string
	cred Credential
	http *http.Client
}

// New builds the client of the service at u, an absolute http or https
// URL. A nil credential sends none, and a nil client is the traced one,
// so a search joins the turn's trace.
func New(u string, cred Credential, c *http.Client) (*Client, error) {
	if err := CheckURL(u); err != nil {
		return nil, err
	}
	if c == nil {
		c = otel.HTTPClient()
	}
	return &Client{url: u, cred: cred, http: c}, nil
}

// CheckURL reports whether u can be a search service's URL: absolute,
// http or https, with a host.
func CheckURL(u string) error {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return fmt.Errorf("search: %q is not an absolute http or https URL", u)
	}
	return nil
}

// Search sends r and reads the answer. r.MaxResults below one asks
// DefaultResults, so the service never applies a default of its own.
func (c *Client) Search(ctx context.Context, r Request) (Result, error) {
	if r.MaxResults < 1 {
		r.MaxResults = DefaultResults
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	body, err := json.Marshal(r)
	if err != nil {
		return Result{}, fmt.Errorf("search: encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("search: build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cred != nil {
		tok, err := c.cred(ctx)
		if err != nil {
			return Result{}, fmt.Errorf("%w: the credential: %w", ErrFailed, err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %w", ErrFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody+1))
	if err != nil {
		return Result{}, fmt.Errorf("%w: read the answer: %w", ErrFailed, err)
	}
	if len(raw) > MaxResponseBody {
		return Result{}, fmt.Errorf("%w: the answer passed %d bytes", ErrFailed, MaxResponseBody)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return read(raw, r.MaxResults)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		if ref := refusal(resp, raw); ref != nil {
			return Result{}, ref
		}
	}
	return Result{}, fmt.Errorf("%w: the service answered %d", ErrFailed, resp.StatusCode)
}

// read decodes a 200's body into at most limit results.
func read(raw []byte, limit int) (Result, error) {
	var doc struct {
		Results *[]struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
		CostUSDMicro *int64 `json:"cost_usd_micro"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{}, fmt.Errorf("%w: the answer does not decode: %w", ErrFailed, err)
	}
	if doc.Results == nil {
		return Result{}, fmt.Errorf("%w: the answer has no results member", ErrFailed)
	}
	if doc.CostUSDMicro != nil && *doc.CostUSDMicro < 0 {
		return Result{}, fmt.Errorf("%w: the answer's cost is negative", ErrFailed)
	}
	out := Result{CostUSDMicro: doc.CostUSDMicro, Hits: []Hit{}}
	for _, h := range *doc.Results {
		if len(out.Hits) == limit {
			break
		}
		if CheckURL(h.URL) != nil {
			continue
		}
		out.Hits = append(out.Hits, Hit{
			Title:   cut(oneLine(h.Title), MaxTitleLength),
			URL:     h.URL,
			Snippet: cut(strings.ToValidUTF8(strings.TrimSpace(h.Snippet), "�"), MaxSnippetLength),
		})
	}
	return out, nil
}

// refusal reads a 4xx's error envelope, nil when the body is none.
func refusal(resp *http.Response, raw []byte) *Refused {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Error == nil || strings.TrimSpace(env.Error.Message) == "" {
		return nil
	}
	ref := &Refused{Status: resp.StatusCode, Code: env.Error.Code, Message: strings.TrimSpace(env.Error.Message)}
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		ref.RetryAfter = time.Duration(s) * time.Second
	}
	return ref
}

// oneLine is s as one line of valid UTF-8, its runs of white space one
// space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ToValidUTF8(s, "�")), " ")
}

// cut keeps at most n characters of s, ending a cut one with cutMark.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:n-utf8.RuneCountInString(cutMark)]), " ") + cutMark
}
