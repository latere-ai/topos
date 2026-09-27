// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
)

// fetching is a machine with a Fetcher whose answer a test sets.
type fetching struct {
	machine.Machine
	got   *machine.FetchRequest
	res   machine.FetchResult
	err   error
	block bool
}

func (f fetching) Fetch(ctx context.Context, r machine.FetchRequest) (machine.FetchResult, error) {
	if f.got != nil {
		*f.got = r
	}
	if f.block {
		<-ctx.Done()
		return machine.FetchResult{}, fmt.Errorf("fetch: %w", ctx.Err())
	}
	return f.res, f.err
}

func TestWebFetch(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	wf := builtinTool(t, NameWebFetch)
	page := func(status int, ctype, body string) machine.FetchResult {
		return machine.FetchResult{URL: "https://example.com/final", Status: status, ContentType: ctype, Body: []byte(body)}
	}
	var got machine.FetchRequest
	for _, c := range []struct {
		name    string
		res     machine.FetchResult
		outcome string
		want    string
	}{
		{"html", page(200, "text/html; charset=utf-8", "<html><head><title>T</title><script>x()</script></head><body><h1>Hi</h1><p>A&amp;B</p></body></html>"), OutcomeOK,
			"URL: https://example.com/final\nStatus: 200\nContent-Type: text/html; charset=utf-8\n\nT\n\n# Hi\n\nA&B\n"},
		{"text", page(200, "text/plain", "plain\n"), OutcomeOK, "URL: https://example.com/final\nStatus: 200\nContent-Type: text/plain\n\nplain\n"},
		{"json", page(200, "application/problem+json", `{"a":1}`), OutcomeOK, "URL: https://example.com/final\nStatus: 200\nContent-Type: application/problem+json\n\n{\"a\":1}\n"},
		{"known", page(200, "application/json", `[]`), OutcomeOK, "URL: https://example.com/final\nStatus: 200\nContent-Type: application/json\n\n[]\n"},
		{"untyped text", page(200, "", "hello"), OutcomeOK, "URL: https://example.com/final\nStatus: 200\n\nhello\n"},
		{"invalid utf-8", page(200, "text/plain", "a\xffb"), OutcomeOK, "URL: https://example.com/final\nStatus: 200\nContent-Type: text/plain\n\na\uFFFDb\n"},
		{"not found", page(404, "text/plain", "no such page"), OutcomeError, "URL: https://example.com/final\nStatus: 404\nContent-Type: text/plain\n\nno such page\n"},
		{"truncated", machine.FetchResult{URL: "https://example.com/final", Status: 200, ContentType: "text/plain", Body: []byte("cut"), Truncated: true}, OutcomeOK,
			"URL: https://example.com/final\nStatus: 200\nContent-Type: text/plain\n\ncut\n[the body passed 10 MiB and was cut there]\n"},
		{"binary", page(200, "image/png", "\x89PNG"), OutcomeError, "https://example.com/final returned image/png, which is not text; web_fetch returns text and HTML pages."},
		{"untyped binary", page(200, "", "a\x00b"), OutcomeError, "https://example.com/final returned no content type, which is not text; web_fetch returns text and HTML pages."},
		{"bad type", page(200, "text/", "x"), OutcomeOK, "URL: https://example.com/final\nStatus: 200\nContent-Type: text/\n\nx\n"},
	} {
		res := run(ctx, t, wf, fetching{Machine: f.h, got: &got, res: c.res}, State{}, `{"url":"https://example.com/start"}`)
		if res.Outcome != c.outcome || text(res) != c.want {
			t.Fatalf("%s: %s %q\nwant %q", c.name, res.Outcome, text(res), c.want)
		}
	}
	if got.URL != "https://example.com/start" {
		t.Fatalf("the fetcher was asked for %q", got.URL)
	}
}

func TestWebFetchFailures(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	wf := builtinTool(t, NameWebFetch)
	in := `{"url":"https://example.com/"}`
	for _, c := range []struct {
		name    string
		m       machine.Machine
		input   string
		outcome string
		want    string
	}{
		{"no fetcher", faulty{Machine: f.h}, in, OutcomeError, "Web fetch is not available on this machine."},
		{"scheme", fetching{Machine: f.h}, `{"url":"ftp://example.com/"}`, OutcomeError, `"ftp://example.com/" is not an http or https URL.`},
		{"no host", fetching{Machine: f.h}, `{"url":"https:///path"}`, OutcomeError, `"https:///path" is not an http or https URL.`},
		{"unparsable", fetching{Machine: f.h}, `{"url":"http://%zz"}`, OutcomeError, `"http://%zz" is not an http or https URL.`},
		{"timeout", fetching{Machine: f.h, err: fmt.Errorf("machine: the fetch passed its 30s timeout: %w", context.DeadlineExceeded)}, in, OutcomeTimeout, "The fetch of https://example.com/ passed its timeout of 30s."},
		{"refused", fetching{Machine: f.h, err: errors.New("machine: fetch: connection refused")}, in, OutcomeError, "The fetch of https://example.com/ failed: fetch: connection refused."},
	} {
		res := run(ctx, t, wf, c.m, State{}, c.input)
		if res.Outcome != c.outcome || text(res) != c.want {
			t.Fatalf("%s: %s %q", c.name, res.Outcome, text(res))
		}
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res := run(cctx, t, wf, fetching{Machine: f.h, block: true}, State{}, in)
	if res.Outcome != OutcomeCanceled || text(res) != "The fetch of https://example.com/ was canceled." {
		t.Fatalf("canceled %s %q", res.Outcome, text(res))
	}
	if _, err := wf.Run(ctx, Call{Input: []byte(in), Machine: fetching{Machine: f.h, err: machine.ErrReleased}}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a released machine %v", err)
	}
	big := machine.FetchResult{URL: "https://example.com/", Status: 200, ContentType: "text/plain", Body: []byte(strings.Repeat("x", 40<<10))}
	res = run(ctx, t, wf, fetching{Machine: f.h, res: big}, State{}, in)
	if res.Spill == nil || res.Spill.Bytes <= 40<<10 {
		t.Fatalf("a long page is not spilled: %+v", res.Spill)
	}
}
