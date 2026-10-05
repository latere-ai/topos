// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/harness/search"
)

// service is a search service that records what it was sent and answers
// with status, the header Retry-After when set, and body.
type service struct {
	auth, retry string
	sent        search.Request
	status      int
	body        string
	calls       int
}

func (s *service) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls++
		s.auth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("the request is %s with %q", r.Method, r.Header.Get("Content-Type"))
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(raw, &s.sent); err != nil {
			t.Errorf("the body %s: %v", raw, err)
		}
		if !strings.Contains(string(raw), `"max_results"`) {
			t.Errorf("the body names no max_results: %s", raw)
		}
		if s.retry != "" {
			w.Header().Set("Retry-After", s.retry)
		}
		w.WriteHeader(s.status)
		_, _ = io.WriteString(w, s.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestTheClient: the client sends the query and the count, the default
// included, with the bearer its function answers at that search, and
// reads results, the cost, a refusal with its Retry-After, failures and
// a body past the bound as the contract says.
func TestTheClient(t *testing.T) {
	long := strings.Repeat("ab ", search.MaxSnippetLength)
	s := &service{status: http.StatusOK, body: `{"results":[` +
		`{"title":"Go 1.25\n is   released","url":"https://go.dev/blog/go1.25","snippet":"  Go 1.25 is now available.  "},` +
		`{"title":"not a page","url":"ftp://example.com/x","snippet":"dropped"},` +
		`{"title":"Long","url":"http://example.com/long","snippet":"` + long + `"},` +
		`{"title":"Third","url":"https://example.com/3"}],"cost_usd_micro":10000,"more":true}`}
	srv := s.start(t)
	n := 0
	c, err := New(srv.URL, func(context.Context) (string, error) { n++; return "key-" + string(rune('0'+n)), nil }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Search(t.Context(), search.Request{Query: "go release", MaxResults: 2})
	if err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer key-1" || s.sent != (search.Request{Query: "go release", MaxResults: 2}) {
		t.Fatalf("sent %+v with %q", s.sent, s.auth)
	}
	if len(res.Hits) != 2 || res.Hits[0] != (search.Hit{Title: "Go 1.25 is released", URL: "https://go.dev/blog/go1.25", Snippet: "Go 1.25 is now available."}) {
		t.Fatalf("hits %+v", res.Hits)
	}
	if got := res.Hits[1].Snippet; len([]rune(got)) != search.MaxSnippetLength || !strings.HasSuffix(got, "...") {
		t.Fatalf("a long snippet is %d characters: %q", len([]rune(got)), got[len(got)-10:])
	}
	if res.CostUSDMicro == nil || *res.CostUSDMicro != 10000 {
		t.Fatalf("cost %v", res.CostUSDMicro)
	}
	// A second search asks the credential again, and no count is the
	// default.
	if _, err := c.Search(t.Context(), search.Request{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer key-2" || s.sent.MaxResults != search.DefaultResults {
		t.Fatalf("the second search sent %+v with %q", s.sent, s.auth)
	}

	for name, tc := range map[string]struct {
		status       int
		retry, body  string
		code, msg    string
		retryAfter   time.Duration
		failed       bool
		noCost, none bool
	}{
		"a refusal":                 {status: 403, body: `{"error":{"code":"search_needs_credit","message":"Searching needs credit.","details":{"detail":"x"}}}`, code: "search_needs_credit", msg: "Searching needs credit."},
		"a rate with Retry-After":   {status: 429, retry: "7", body: `{"error":{"code":"rate_limited","message":"Too many."}}`, code: "rate_limited", msg: "Too many.", retryAfter: 7 * time.Second},
		"a 4xx without an envelope": {status: 404, body: `404 page not found`, failed: true},
		"a 4xx with no message":     {status: 400, body: `{"error":{"code":"x"}}`, failed: true},
		"a 5xx":                     {status: 503, body: `{"error":{"code":"unavailable","message":"Down."}}`, failed: true},
		"a body that does not read": {status: 200, body: `{"results":`, failed: true},
		"no results member":         {status: 200, body: `{}`, failed: true},
		"a negative cost":           {status: 200, body: `{"results":[],"cost_usd_micro":-1}`, failed: true},
		"no results and no cost":    {status: 200, body: `{"results":[]}`, noCost: true, none: true},
		"a body past the bound":     {status: 200, body: `{"results":[],"pad":"` + strings.Repeat("x", search.MaxResponseBody) + `"}`, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &service{status: tc.status, retry: tc.retry, body: tc.body}
			srv := s.start(t)
			c, err := New(srv.URL, nil, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			res, err := c.Search(t.Context(), search.Request{Query: "q", MaxResults: 3})
			if s.auth != "" {
				t.Errorf("no credential sent %q", s.auth)
			}
			var ref *search.Refused
			switch {
			case tc.failed:
				if !errors.Is(err, search.ErrFailed) {
					t.Fatalf("got %v, want a failure", err)
				}
			case tc.code != "":
				if !errors.As(err, &ref) || ref.Code != tc.code || ref.Message != tc.msg || ref.RetryAfter != tc.retryAfter || ref.Status != tc.status {
					t.Fatalf("got %v (%+v)", err, ref)
				}
			default:
				if err != nil || res.Hits == nil || len(res.Hits) != 0 || (tc.noCost && res.CostUSDMicro != nil) {
					t.Fatalf("got %+v, %v", res, err)
				}
			}
		})
	}
}

// TestTheClientsFailures: a credential that cannot be had, a service
// that does not answer and one past the timeout are failures, the last
// one a deadline.
func TestTheClientsFailures(t *testing.T) {
	if _, err := New("ftp://example.com", nil, nil); err == nil {
		t.Fatal("an ftp URL was accepted")
	}
	if _, err := New("/search", nil, nil); err == nil {
		t.Fatal("a relative URL was accepted")
	}
	s := &service{status: http.StatusOK, body: `{"results":[]}`}
	srv := s.start(t)
	c, err := New(srv.URL, func(context.Context) (string, error) { return "", errors.New("no key") }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(t.Context(), search.Request{Query: "q"}); !errors.Is(err, search.ErrFailed) || s.calls != 0 {
		t.Fatalf("a credential that failed: %v after %d calls", err, s.calls)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	c, err = New(gone.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Search(t.Context(), search.Request{Query: "q"}); !errors.Is(err, search.ErrFailed) {
		t.Fatalf("a closed service: %v", err)
	}
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	c, err = New(slow.URL, nil, slow.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Search(ctx, search.Request{Query: "q"}); !errors.Is(err, search.ErrFailed) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a slow service: %v", err)
	}
}
