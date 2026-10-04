// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/search"
)

// searching is a search service whose answer a test sets.
type searching struct {
	got   *search.Request
	res   search.Result
	err   error
	block bool
}

func (s searching) Search(ctx context.Context, r search.Request) (search.Result, error) {
	if s.got != nil {
		*s.got = r
	}
	if s.block {
		<-ctx.Done()
		return search.Result{}, fmt.Errorf("%w: %w", search.ErrFailed, ctx.Err())
	}
	return s.res, s.err
}

// TestWebSearchResults: each row of spec 040's result table renders its
// text and outcome, a search's cost is the result's, and the call asks
// the count it names or the default.
func TestWebSearchResults(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	cost := int64(10000)
	hits := search.Result{Hits: []search.Hit{
		{Title: "Go 1.25 is released", URL: "https://go.dev/blog/go1.25", Snippet: "Go 1.25 is now available."},
		{Title: "Release History", URL: "https://go.dev/doc/devel/release"},
	}, CostUSDMicro: &cost}
	var got search.Request
	for _, c := range []struct {
		name, input string
		s           search.Searcher
		outcome     string
		want        string
		cost        int64
		refusal     string
		asked       search.Request
	}{
		{"results", `{"query":"  go release  ","max_results":2}`, searching{got: &got, res: hits}, OutcomeOK,
			"1. Go 1.25 is released\n   https://go.dev/blog/go1.25\n   Go 1.25 is now available.\n\n2. Release History\n   https://go.dev/doc/devel/release\n\n", cost, "", search.Request{Query: "go release", MaxResults: 2}},
		{"the default count", `{"query":"go"}`, searching{got: &got, res: search.Result{Hits: []search.Hit{}}}, OutcomeOK,
			`No results for "go".`, 0, "", search.Request{Query: "go", MaxResults: search.DefaultResults}},
		{"a refusal", `{"query":"go"}`, searching{got: &got, err: &search.Refused{Status: 403, Code: "search_needs_credit", Message: "Searching the web needs credit."}}, OutcomeError,
			"The search service refused the search: Searching the web needs credit.", 0, "search_needs_credit", search.Request{Query: "go", MaxResults: search.DefaultResults}},
		{"a rate", `{"query":"go"}`, searching{got: &got, err: &search.Refused{Status: 429, Code: "rate_limited", Message: "Too many searches.", RetryAfter: 7 * time.Second}}, OutcomeError,
			"The search service refused the search: Too many searches. It can be tried again in 7s.", 0, "rate_limited", search.Request{Query: "go", MaxResults: search.DefaultResults}},
		{"a failure", `{"query":"go"}`, searching{got: &got, err: fmt.Errorf("%w: the service answered 503", search.ErrFailed)}, OutcomeError,
			"The search failed: the service answered 503.", 0, "", search.Request{Query: "go", MaxResults: search.DefaultResults}},
		{"a timeout", `{"query":"go"}`, searching{got: &got, err: fmt.Errorf("%w: %w", search.ErrFailed, context.DeadlineExceeded)}, OutcomeTimeout,
			`The search for "go" passed its timeout of 30s.`, 0, "", search.Request{Query: "go", MaxResults: search.DefaultResults}},
		{"no service", `{"query":"go"}`, nil, OutcomeError, "Web search is not available on this server.", 0, "", search.Request{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got = search.Request{}
			res := run(ctx, t, WebSearch(c.s), f.h, State{}, c.input)
			if res.Outcome != c.outcome || text(res) != c.want {
				t.Fatalf("%s %q, want %s %q", res.Outcome, text(res), c.outcome, c.want)
			}
			if got != c.asked {
				t.Fatalf("asked %+v, want %+v", got, c.asked)
			}
			switch {
			case c.cost == 0 && res.CostUSDMicro != nil:
				t.Fatalf("a cost of %d on a search that charged nothing", *res.CostUSDMicro)
			case c.cost != 0 && (res.CostUSDMicro == nil || *res.CostUSDMicro != c.cost):
				t.Fatalf("cost %v, want %d", res.CostUSDMicro, c.cost)
			}
			if (c.refusal == "") != (res.Meta == nil) || (res.Meta != nil && res.Meta.Refusal != c.refusal) {
				t.Fatalf("meta %+v, want the refusal %q", res.Meta, c.refusal)
			}
		})
	}

	// A canceled turn cancels the search.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan Result)
	go func() {
		res, err := WebSearch(searching{block: true}).Run(cctx, Call{ID: "toolu_1", Input: json.RawMessage(`{"query":"go"}`), Machine: f.h})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	cancel()
	if res := <-done; res.Outcome != OutcomeCanceled || text(res) != `The search for "go" was canceled.` {
		t.Fatalf("canceled: %s %q", res.Outcome, text(res))
	}

	// A query of white space is refused without a search.
	got = search.Request{}
	res := run(ctx, t, WebSearch(searching{got: &got}), f.h, State{}, `{"query":"   "}`)
	if res.Outcome != OutcomeInvalidInput || got != (search.Request{}) {
		t.Fatalf("a blank query: %s %q, asked %+v", res.Outcome, text(res), got)
	}
	// A charged result keeps no meta of a refusal, and a zero cost is no
	// cost.
	zero := int64(0)
	res = run(ctx, t, WebSearch(searching{res: search.Result{Hits: []search.Hit{}, CostUSDMicro: &zero}}), f.h, State{}, `{"query":"go"}`)
	if res.CostUSDMicro != nil || res.Meta != nil {
		t.Fatalf("a zero cost: %+v", res)
	}
}

// TestWebSearchSchemaFollowsTheConstants: the schema states each bound
// from the search package's constants, and the registry refuses a call
// past one before the service is asked.
func TestWebSearchSchemaFollowsTheConstants(t *testing.T) {
	tool := WebSearch(nil)
	var schema struct {
		Properties struct {
			Query struct {
				MinLength int `json:"minLength"`
				MaxLength int `json:"maxLength"`
			} `json:"query"`
			MaxResults struct {
				Minimum int `json:"minimum"`
				Maximum int `json:"maximum"`
			} `json:"max_results"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.Definition().InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	p := schema.Properties
	if p.Query.MinLength != 1 || p.Query.MaxLength != search.MaxQueryLength || p.MaxResults.Minimum != 1 || p.MaxResults.Maximum != search.MaxResults || !slices.Equal(schema.Required, []string{"query"}) {
		t.Fatalf("the schema %s", tool.Definition().InputSchema)
	}
	if props := tool.Properties(); props.Effect != EffectRead || !props.Parallel {
		t.Fatalf("properties %+v", props)
	}
	reg := NewRegistry()
	if err := reg.AddBuiltin(tool); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"query":""}`,
		`{"query":"` + strings.Repeat("a", search.MaxQueryLength+1) + `"}`,
		fmt.Sprintf(`{"query":"go","max_results":%d}`, search.MaxResults+1),
		`{"query":"go","max_results":0}`,
		`{"query":"go","depth":"deep"}`,
	} {
		if _, r := reg.Validate(NameWebSearch, json.RawMessage(input)); r == nil || r.Outcome != OutcomeInvalidInput {
			t.Errorf("%s was not refused", input)
		}
	}
	if _, r := reg.Validate(NameWebSearch, json.RawMessage(`{"query":"`+strings.Repeat("é", search.MaxQueryLength)+`","max_results":10}`)); r != nil {
		t.Errorf("a call at the bounds was refused: %+v", r)
	}
	if !slices.Equal(OptIn(), []string{NameWebSearch}) || slices.ContainsFunc(Builtins(), func(b Tool) bool { return b.Definition().Name == NameWebSearch }) {
		t.Fatal("web_search is not opt-in alone")
	}
}
