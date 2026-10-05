// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"latere.ai/x/topos/harness/search"
	"latere.ai/x/topos/prompts"
)

// NameWebSearch is the web search tool of spec 042, a built-in an agent
// holds only when it names it.
const NameWebSearch = "web_search"

// OptIn are the names of the built-ins an agent holds only by naming
// them: in no default set, so an agent that names no tools keeps the
// eight of Builtins and its digest.
func OptIn() []string { return []string{NameWebSearch} }

// webSearchSchema is the tool's input schema, its bounds the search
// package's.
var webSearchSchema = fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "minLength": 1, "maxLength": %d, "description": "What to search for, in the words a person would type."},
    "max_results": {"type": "integer", "minimum": 1, "maximum": %d, "description": "How many results to return; %d when omitted."}
  },
  "required": ["query"],
  "additionalProperties": false
}`, search.MaxQueryLength, search.MaxResults, search.DefaultResults)

// WebSearch is the web search tool over s, the installation's search
// service. Its effect is none: a search changes nothing on the machine
// and reaches only the service the operator set, so it is allowed in
// every mode and never opens a machine. A nil s is no service: every
// call answers that web search is not available, as web_fetch does on a
// machine without network.
func WebSearch(s search.Searcher) Tool {
	return newBuiltin(NameWebSearch, prompts.Text(prompts.ToolWebSearch), webSearchSchema, Properties{Parallel: true, Effect: EffectNone},
		func(ctx context.Context, b *builtin, c Call) (Result, error) { return runWebSearch(ctx, b, c, s) })
}

type webSearchInput struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results"`
}

func runWebSearch(ctx context.Context, b *builtin, c Call, s search.Searcher) (Result, error) {
	var in webSearchInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return Text(OutcomeInvalidInput, invalidInput(NameWebSearch, "/query: empty")), nil
	}
	if s == nil {
		return b.result(ctx, c, OutcomeError, prompts.Text(prompts.WebSearchUnavailable), nil)
	}
	limit := in.MaxResults
	if limit < 1 {
		limit = search.DefaultResults
	}
	res, err := s.Search(ctx, search.Request{Query: query, MaxResults: limit})
	var refused *search.Refused
	switch {
	case err != nil && ctx.Err() != nil:
		return b.result(ctx, c, OutcomeCanceled, prompts.Render(prompts.WebSearchCanceled, prompts.Data{"Query": query}), nil)
	case errors.Is(err, context.DeadlineExceeded):
		return b.result(ctx, c, OutcomeTimeout, prompts.Render(prompts.WebSearchTimeout, prompts.Data{"Query": query, "Timeout": search.Timeout.String()}), nil)
	case errors.As(err, &refused):
		retry := ""
		if refused.RetryAfter > 0 {
			retry = refused.RetryAfter.String()
		}
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.WebSearchRefused, prompts.Data{"Message": refused.Message, "Retry": retry}), &Meta{Refusal: refused.Code})
	case err != nil:
		detail := strings.TrimPrefix(strings.TrimPrefix(err.Error(), search.ErrFailed.Error()+": "), "search: ")
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.WebSearchFailed, prompts.Data{"Error": detail}), nil)
	}
	var out Result
	if len(res.Hits) == 0 {
		out, err = b.result(ctx, c, OutcomeOK, prompts.Render(prompts.WebSearchNone, prompts.Data{"Query": query}), nil)
	} else {
		hits := make([]map[string]any, len(res.Hits))
		for i, h := range res.Hits {
			hits[i] = map[string]any{"Number": i + 1, "Title": h.Title, "URL": h.URL, "Snippet": h.Snippet}
		}
		out, err = b.result(ctx, c, OutcomeOK, prompts.Render(prompts.WebSearchResults, prompts.Data{"Results": hits}), nil)
	}
	if err != nil {
		return Result{}, err
	}
	if res.CostUSDMicro != nil && *res.CostUSDMicro > 0 {
		cost := *res.CostUSDMicro
		out.CostUSDMicro = &cost
	}
	return out, nil
}
