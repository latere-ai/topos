// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package websearch is toposd's HTTP client of the search service
// web_search sends its queries to (spec 047): one POST of a query and a
// result count to the URL the installation configures, with the
// credential its function answers at that search, read into the
// contract of harness/search.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness/search"
)

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
	if err := search.CheckURL(u); err != nil {
		return nil, err
	}
	if c == nil {
		c = otel.HTTPClient()
	}
	return &Client{url: u, cred: cred, http: c}, nil
}

// Search sends r and reads the answer. r.MaxResults below one asks
// search.DefaultResults, so the service never applies a default of its
// own.
func (c *Client) Search(ctx context.Context, r search.Request) (search.Result, error) {
	if r.MaxResults < 1 {
		r.MaxResults = search.DefaultResults
	}
	ctx, cancel := context.WithTimeout(ctx, search.Timeout)
	defer cancel()
	body, err := json.Marshal(r)
	if err != nil {
		return search.Result{}, fmt.Errorf("search: encode the request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return search.Result{}, fmt.Errorf("search: build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.cred != nil {
		tok, err := c.cred(ctx)
		if err != nil {
			return search.Result{}, fmt.Errorf("%w: the credential: %w", search.ErrFailed, err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return search.Result{}, fmt.Errorf("%w: %w", search.ErrFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, search.MaxResponseBody+1))
	if err != nil {
		return search.Result{}, fmt.Errorf("%w: read the answer: %w", search.ErrFailed, err)
	}
	if len(raw) > search.MaxResponseBody {
		return search.Result{}, fmt.Errorf("%w: the answer passed %d bytes", search.ErrFailed, search.MaxResponseBody)
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return read(raw, r.MaxResults)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		if ref := refusal(resp, raw); ref != nil {
			return search.Result{}, ref
		}
	}
	return search.Result{}, fmt.Errorf("%w: the service answered %d", search.ErrFailed, resp.StatusCode)
}

// read decodes a 200's body into at most limit results.
func read(raw []byte, limit int) (search.Result, error) {
	var doc struct {
		Results      *[]search.Hit `json:"results"`
		CostUSDMicro *int64        `json:"cost_usd_micro"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return search.Result{}, fmt.Errorf("%w: the answer does not decode: %w", search.ErrFailed, err)
	}
	if doc.Results == nil {
		return search.Result{}, fmt.Errorf("%w: the answer has no results member", search.ErrFailed)
	}
	if doc.CostUSDMicro != nil && *doc.CostUSDMicro < 0 {
		return search.Result{}, fmt.Errorf("%w: the answer's cost is negative", search.ErrFailed)
	}
	out := search.Result{CostUSDMicro: doc.CostUSDMicro, Hits: []search.Hit{}}
	for _, h := range *doc.Results {
		if len(out.Hits) == limit {
			break
		}
		if clean, ok := search.Clean(h); ok {
			out.Hits = append(out.Hits, clean)
		}
	}
	return out, nil
}

// refusal reads a 4xx's error envelope, nil when the body is none.
func refusal(resp *http.Response, raw []byte) *search.Refused {
	var env struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decoded := json.Unmarshal(raw, &env) == nil
	if !decoded || env.Error == nil || strings.TrimSpace(env.Error.Message) == "" {
		return nil
	}
	ref := &search.Refused{Status: resp.StatusCode, Code: env.Error.Code, Message: strings.TrimSpace(env.Error.Message)}
	if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
		ref.RetryAfter = time.Duration(s) * time.Second
	}
	return ref
}
