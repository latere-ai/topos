// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package parser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/models"
)

// stubParse is one parse the stub service holds: the states its gets
// answer in turn, the last kept, its pages and usage, and how many
// document reads answer not ready first.
type stubParse struct {
	states   []string
	total    int
	failed   int
	usage    Usage
	pages    []Page
	notReady int
	errCode  string
}

// stub is the parsing service: it answers submits with the parse its
// next function makes, and records every request.
type stub struct {
	t        *testing.T
	mu       sync.Mutex
	next     func(body map[string]any) *stubParse
	parses   map[string]*stubParse
	requests []string
	submits  []map[string]any
	prefer   []string
	bearers  []string
	refuse   map[string]int // a path prefix to the status it is refused with
	srv      *httptest.Server
}

func newStub(t *testing.T) *stub {
	s := &stub{t: t, parses: map[string]*stubParse{}, refuse: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func envelope(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"The %s refusal."}}`, code, code)
}

func (s *stub) view(id string, p *stubParse) map[string]any {
	state := p.states[0]
	if len(p.states) > 1 {
		p.states = p.states[1:]
	}
	v := map[string]any{
		"id": id, "state": state,
		"progress": map[string]any{"pages_total": p.total, "pages_failed": p.failed},
		"usage":    p.usage,
	}
	if p.errCode != "" {
		v["error"] = map[string]any{"code": p.errCode, "detail": "the " + p.errCode + " detail"}
	}
	return v
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		line += "?" + r.URL.RawQuery
	}
	s.requests = append(s.requests, line)
	s.bearers = append(s.bearers, r.Header.Get("Authorization"))
	for prefix, status := range s.refuse {
		if strings.HasPrefix(line, prefix) {
			envelope(w, status, map[int]string{403: "forbidden", 404: "not_found", 413: "file_too_large"}[status])
			return
		}
	}
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/parsing/parses"), "/")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/parsing/parses":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Error(err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			s.t.Errorf("a submit's body: %v", err)
		}
		s.submits = append(s.submits, body)
		s.prefer = append(s.prefer, r.Header.Get("Prefer"))
		id := "prs_" + strconv.Itoa(len(s.submits))
		p := s.next(body)
		s.parses[id] = p
		v := s.view(id, p)
		status := http.StatusAccepted
		if st := v["state"]; st == StateSucceeded || st == StateFailed {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(v); err != nil {
			s.t.Error(err)
		}
	case r.Method == http.MethodGet && len(segs) >= 2:
		p, ok := s.parses[segs[1]]
		if !ok {
			envelope(w, http.StatusNotFound, "not_found")
			return
		}
		var out any
		switch {
		case len(segs) == 2:
			out = s.view(segs[1], p)
		case len(segs) == 3 && segs[2] == "pages":
			var list []PageSummary
			for _, pg := range p.pages {
				list = append(list, PageSummary{Number: pg.Number, State: "read", Source: pg.Source})
			}
			out = map[string]any{"pages": list}
		case len(segs) == 4 && segs[2] == "pages":
			n, _ := strconv.Atoi(segs[3])
			i := slices.IndexFunc(p.pages, func(pg Page) bool { return pg.Number == n })
			if i < 0 {
				envelope(w, http.StatusNotFound, "page_not_found")
				return
			}
			out = p.pages[i]
		case len(segs) == 3 && segs[2] == "document":
			if p.notReady > 0 {
				p.notReady--
				envelope(w, http.StatusConflict, CodeNotReady)
				return
			}
			q := r.URL.Query()
			if q.Get("format") != "markdown" || q.Get("page_breaks") != "true" {
				s.t.Errorf("a document read asks %s", r.URL.RawQuery)
			}
			sel, err := parsing.Select(q.Get("pages"), p.total)
			if err != nil {
				envelope(w, http.StatusBadRequest, "invalid_pages")
				return
			}
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			for _, n := range sel {
				_, _ = fmt.Fprintf(w, "<!-- page %d -->\n# Page %d\n\nThe text of page %d.\n\n", n, n, n)
			}
			return
		default:
			envelope(w, http.StatusNotFound, "not_found")
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			s.t.Error(err)
		}
	default:
		envelope(w, http.StatusNotFound, "not_found")
	}
}

// folder is a session's folder that keeps one file, attachments/e1/a.pdf,
// with the digest "abc".
type folder struct {
	mu       sync.Mutex
	located  []string
	presigns int
	err      error
}

func (f *folder) Locate(_ context.Context, path, sha string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.located = append(f.located, path+"@"+sha)
	if path != "attachments/e1/a.pdf" || sha != "abc" {
		return "", parsing.ErrNotKept
	}
	return "attachments/e1/a.pdf", nil
}

func (f *folder) Presign(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.presigns++
	if f.err != nil {
		return "", f.err
	}
	return "https://bucket.example/" + ref + "?sig=" + strconv.Itoa(f.presigns), nil
}

func (f *folder) Origin(ref, sha string) Origin {
	return Origin{Store: "storage", Path: "usr_1/sessions/s1/" + ref, Version: sha}
}

// price is the figures of one model, a micro-USD per input token and two
// per output token, and of no other.
func price(_ context.Context, model string) (models.Entry, error) {
	if model != "vendor/reader-1" {
		return models.Entry{}, models.ErrUnpriced
	}
	in, out := models.Price(1_000_000), models.Price(2_000_000)
	return models.Entry{Name: model, Pricing: &models.Pricing{Input: &in, Output: &out}}, nil
}

func reader(t *testing.T, s *stub, f Folder) *Reader {
	t.Helper()
	c, err := New(s.srv.URL+"/v1/parsing", func(context.Context) (string, error) { return "tok", nil }, s.srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReader(c, f, price, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.poll, r.pollMax = time.Millisecond, 4*time.Millisecond
	return r
}

// textLayer is a parse of 12 pages read from their text layer, which
// gets answer running once and then succeeded.
func textLayer(map[string]any) *stubParse {
	return &stubParse{states: []string{"running", "running", StateSucceeded}, total: 12}
}

var req = parsing.Request{Path: "attachments/e1/a.pdf", SHA256: "abc"}

// TestAReadSubmitsWithTheOrigin: a read finds the file in the folder by
// its path and digest, presigns it, submits the address with the file's
// origin, the pages bound, the failed pages allowed and a deadline,
// waits through Prefer and the polls, asks the document again while it
// is not ready, and answers the pages asked as Markdown.
func TestAReadSubmitsWithTheOrigin(t *testing.T) {
	s := newStub(t)
	s.next = func(b map[string]any) *stubParse { p := textLayer(b); p.notReady = 2; return p }
	f := &folder{}
	doc, err := reader(t, s, f).Read(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ID != "prs_1" || doc.Pages != 12 || doc.More || !slices.Equal(doc.Shown, []int{1, 2, 3, 4, 5}) || doc.CostUSDMicro == nil || *doc.CostUSDMicro != 0 || doc.Unpriced {
		t.Fatalf("the document %+v", doc)
	}
	if !strings.HasPrefix(doc.Text, "<!-- page 1 -->\n# Page 1\n") || !strings.Contains(doc.Text, "The text of page 5.") || strings.Contains(doc.Text, "page 6") {
		t.Fatalf("the text %q", doc.Text)
	}
	if !slices.Equal(f.located, []string{"attachments/e1/a.pdf@abc"}) || f.presigns != 1 {
		t.Fatalf("located %v, presigned %d", f.located, f.presigns)
	}
	sub := s.submits[0]
	origin, _ := sub["origin"].(map[string]any)
	if src, _ := sub["source"].(map[string]any); src["url"] != "https://bucket.example/attachments/e1/a.pdf?sig=1" {
		t.Fatalf("the source %v", sub["source"])
	}
	if origin["store"] != "storage" || origin["path"] != "usr_1/sessions/s1/attachments/e1/a.pdf" || origin["version"] != "abc" {
		t.Fatalf("the origin %v", origin)
	}
	if sub["pages"] != fmt.Sprintf("1-%d", parsing.MaxPages) || sub["allow_failed_pages"] != float64(parsing.MaxPages) {
		t.Fatalf("the submit %v", sub)
	}
	d, err := time.ParseDuration(sub["deadline"].(string))
	if err != nil || d > parsing.Timeout-grace || d < parsing.Timeout-grace-5*time.Second {
		t.Fatalf("the deadline %v %v", sub["deadline"], err)
	}
	if s.prefer[0] != "wait=60" {
		t.Fatalf("the submit prefers %q", s.prefer[0])
	}
	for _, b := range s.bearers {
		if b != "Bearer tok" {
			t.Fatalf("a request carried %q", b)
		}
	}
	if n := s.count("GET /v1/parsing/parses/prs_1/document"); n != 3 {
		t.Fatalf("the document was read %d times", n)
	}
	if s.count("GET /v1/parsing/parses/prs_1/pages") != 0 {
		t.Fatal("a parse that cost nothing had its pages read")
	}

	doc, err = reader(t, s, &folder{}).Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Pages: "11-, 2"})
	if err != nil || !slices.Equal(doc.Shown, []int{2, 11, 12}) || !strings.Contains(doc.Text, "The text of page 12.") {
		t.Fatalf("pages 11- and 2: %+v %v", doc, err)
	}
	if !strings.Contains(s.requests[len(s.requests)-1], "pages=2%2C11-12") {
		t.Fatalf("the last request %s", s.requests[len(s.requests)-1])
	}
}

// TestAParseIsKept: a second read of the same bytes reads the kept parse
// and costs nothing; a read that knows a parse from a result's meta reads
// it again, a known one the service no longer has is submitted again,
// and reads of the same bytes at once share one parse.
func TestAParseIsKept(t *testing.T) {
	s := newStub(t)
	s.next = textLayer
	r := reader(t, s, &folder{})
	if _, err := r.Read(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	doc, err := r.Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Pages: "6-7"})
	if err != nil || doc.ID != "prs_1" || doc.CostUSDMicro != nil || !slices.Equal(doc.Shown, []int{6, 7}) {
		t.Fatalf("the second read %+v %v", doc, err)
	}
	if n := s.count("POST "); n != 1 {
		t.Fatalf("%d submits", n)
	}

	// A later drive knows the parse from the thread's meta.
	s.parses["prs_1"].states = []string{StateSucceeded}
	f := &folder{}
	doc, err = reader(t, s, f).Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Known: "prs_1"})
	if err != nil || doc.ID != "prs_1" || doc.CostUSDMicro != nil || len(f.located) != 0 || s.count("POST ") != 1 {
		t.Fatalf("a known parse: %+v %v, located %v", doc, err, f.located)
	}
	doc, err = reader(t, s, f).Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Known: "prs_gone"})
	if err != nil || doc.ID != "prs_2" || doc.CostUSDMicro == nil || s.count("POST ") != 2 {
		t.Fatalf("a known parse past its retention: %+v %v", doc, err)
	}

	s.next = func(map[string]any) *stubParse {
		return &stubParse{states: []string{"running", "running", "running", "running", StateSucceeded}, total: 3}
	}
	r = reader(t, s, &folder{})
	var wg sync.WaitGroup
	docs := make([]parsing.Document, 4)
	errs := make([]error, 4)
	for i := range docs {
		wg.Go(func() { docs[i], errs[i] = r.Read(t.Context(), req) })
	}
	wg.Wait()
	priced := 0
	for i := range docs {
		if errs[i] != nil || docs[i].ID != "prs_3" {
			t.Fatalf("read %d: %+v %v", i, docs[i], errs[i])
		}
		if docs[i].CostUSDMicro != nil {
			priced++
		}
	}
	if s.count("POST ") != 3 || priced != 1 {
		t.Fatalf("%d submits, %d reads priced", s.count("POST "), priced)
	}
}

// TestTheCostOfAParse: a parse whose usage names no tokens costs 0
// without a page read; one a model read is priced at each page's model,
// tokens summed per model, a reused page and a page of the text layer
// left out; a model no source prices leaves the cost unset.
func TestTheCostOfAParse(t *testing.T) {
	s := newStub(t)
	model := "vendor/reader-1"
	s.next = func(map[string]any) *stubParse {
		return &stubParse{states: []string{StateSucceeded}, total: 4, usage: Usage{InputTokens: 3000, OutputTokens: 700}, pages: []Page{
			{Number: 1, Source: "text_layer"},
			{Number: 2, Source: SourceReader, Model: model, Usage: &Usage{InputTokens: 1000, OutputTokens: 200}},
			{Number: 3, Source: SourceReader, Model: model, Usage: &Usage{InputTokens: 2000, OutputTokens: 500}},
			{Number: 4, Source: SourceReader, Model: model, Reused: true, Usage: &Usage{InputTokens: 9000, OutputTokens: 900}},
		}}
	}
	doc, err := reader(t, s, &folder{}).Read(t.Context(), req)
	// 3000 input tokens at 1 and 700 output at 2 micro-USD each.
	if err != nil || doc.CostUSDMicro == nil || *doc.CostUSDMicro != 3000+1400 || doc.Unpriced {
		t.Fatalf("the cost %+v %v", doc, err)
	}
	if s.count("GET /v1/parsing/parses/prs_1/pages/1") != 0 || s.count("GET /v1/parsing/parses/prs_1/pages/2") != 1 {
		t.Fatalf("the pages read: %v", s.requests)
	}

	s.next = func(map[string]any) *stubParse {
		return &stubParse{states: []string{StateSucceeded}, total: 1, usage: Usage{InputTokens: 10}, pages: []Page{
			{Number: 1, Source: SourceReader, Model: "vendor/unknown", Usage: &Usage{InputTokens: 10}},
		}}
	}
	doc, err = reader(t, s, &folder{}).Read(t.Context(), req)
	if err != nil || doc.CostUSDMicro != nil || !doc.Unpriced {
		t.Fatalf("an unpriced model %+v %v", doc, err)
	}
}

// TestAReadsFailures: a refusal of the submit is the service's, a parse
// that ended failed is Failed with what it read priced, a file the
// folder does not keep and a presign that fails are answered without a
// submit, a parse still running at the read's end is a timeout with what
// it read priced, and a selection past the document names its pages.
func TestAReadsFailures(t *testing.T) {
	s := newStub(t)
	s.next = textLayer
	s.refuse["POST /v1/parsing/parses"] = http.StatusForbidden
	_, err := reader(t, s, &folder{}).Read(t.Context(), req)
	if ref, ok := errors.AsType[*parsing.Refused](err); !ok || ref.Status != 403 || ref.Code != "forbidden" || ref.Message != "The forbidden refusal." {
		t.Fatalf("a refused submit: %v", err)
	}
	clear(s.refuse)

	s.next = func(map[string]any) *stubParse {
		return &stubParse{states: []string{StateFailed}, total: 2, errCode: "budget_exhausted", usage: Usage{OutputTokens: 5}, pages: []Page{
			{Number: 1, Source: SourceReader, Model: "vendor/reader-1", Usage: &Usage{OutputTokens: 5}},
		}}
	}
	doc, err := reader(t, s, &folder{}).Read(t.Context(), req)
	if f, ok := errors.AsType[*parsing.Failed](err); !ok || f.State != StateFailed || f.Code != "budget_exhausted" || doc.CostUSDMicro == nil || *doc.CostUSDMicro != 10 {
		t.Fatalf("a failed parse: %+v %v", doc, err)
	}

	before := s.count("POST ")
	if _, err := reader(t, s, &folder{}).Read(t.Context(), parsing.Request{Path: "work/b.pdf", SHA256: "abc"}); !errors.Is(err, parsing.ErrNotKept) {
		t.Fatalf("a file not kept: %v", err)
	}
	if _, err := reader(t, s, &folder{err: errors.New("the files API refused")}).Read(t.Context(), req); !errors.Is(err, parsing.ErrUnavailable) {
		t.Fatalf("a presign that failed: %v", err)
	}
	if s.count("POST ") != before {
		t.Fatal("a file not presigned was submitted")
	}

	s.next = func(map[string]any) *stubParse {
		return &stubParse{states: []string{"running"}, total: 2, usage: Usage{InputTokens: 4}, pages: []Page{
			{Number: 1, Source: SourceReader, Model: "vendor/reader-1", Usage: &Usage{InputTokens: 4}},
		}}
	}
	r := reader(t, s, &folder{})
	r.timeout, r.grace = 2500*time.Millisecond, time.Second
	doc, err = r.Read(t.Context(), req)
	if !errors.Is(err, context.DeadlineExceeded) || doc.CostUSDMicro == nil || *doc.CostUSDMicro != 4 {
		t.Fatalf("a parse past the read's time: %+v %v", doc, err)
	}
	if sub := s.submits[len(s.submits)-1]; sub["deadline"] != "1s" {
		t.Fatalf("the short read's deadline %v", sub["deadline"])
	}
	r.timeout = 1500 * time.Millisecond
	if _, err := r.Read(t.Context(), parsing.Request{Path: req.Path, SHA256: "abc"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a read with no time to submit: %v", err)
	}

	s.next = textLayer
	doc, err = reader(t, s, &folder{}).Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Pages: "13-"})
	if pe, ok := errors.AsType[*parsing.PastEnd](err); !ok || pe.Pages != 12 || doc.Pages != 12 {
		t.Fatalf("pages past the end: %+v %v", doc, err)
	}
	if _, err := reader(t, s, &folder{}).Read(t.Context(), parsing.Request{Path: req.Path, SHA256: req.SHA256, Pages: "x"}); err == nil {
		t.Fatal("a malformed selection was read")
	}
}

// TestTheClientsFailures: a refusal without the envelope, a 5xx, a
// credential that cannot be had, an answer that does not decode and a
// document past its bound are each the contract's error.
func TestTheClientsFailures(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, func(context.Context) (string, error) { return "tok", nil }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{400, "no envelope"}, {500, `{"error":{"code":"internal","message":"m"}}`}, {200, "{"}} {
		status, body = tc.status, tc.body
		if _, err := c.Get(t.Context(), "p"); !errors.Is(err, parsing.ErrUnavailable) {
			t.Errorf("%d %q: %v", tc.status, tc.body, err)
		}
	}
	status, body = 200, strings.Repeat("x", parsing.MaxText+1)
	if _, err := c.Document(t.Context(), "p", "1"); !errors.Is(err, parsing.ErrTooMuchText) {
		t.Fatalf("a long document: %v", err)
	}
	broken, err := New(srv.URL, func(context.Context) (string, error) { return "", errors.New("no token") }, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := broken.Get(t.Context(), "p"); !errors.Is(err, parsing.ErrUnavailable) {
		t.Fatalf("no credential: %v", err)
	}
	for _, u := range []string{"", "ftp://x", "/relative"} {
		if _, err := New(u, func(context.Context) (string, error) { return "", nil }, srv.Client()); err == nil {
			t.Errorf("%q was taken", u)
		}
	}
	if _, err := New(srv.URL, nil, srv.Client()); err == nil {
		t.Error("no token was taken")
	}
	if _, err := NewReader(nil, nil, nil, nil); err == nil {
		t.Error("a reader of nothing was built")
	}
}
