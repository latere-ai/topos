// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package cella implements [sandbox.Provider] against Latere Cella, the hosted
// Kubernetes sandbox platform (https://cella.latere.ai), over its versioned
// /v1 HTTP API.
//
// This package is the single place in Topos that knows Cella exists. Per the
// boundary rule (sandbox/boundary_test.go), no package under sandbox/ may
// import it, and the root topos package does not either: a host constructs a
// Provider with [New] and injects it as the [sandbox.Provider] interface.
//
// It is a hand-rolled net/http client rather than a dependency on the Cella Go
// module: the contract between the two systems is the OpenAPI surface, and
// Topos stays free of Cella's Kubernetes dependency tree.
//
// Authentication is bearer-token, supplied per request by a [TokenSource].
package cella

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"latere.ai/x/topos/sandbox"
)

// Options configure a [Provider].
type Options struct {
	// BaseURL is the root of the Cella API, e.g. "https://cella.latere.ai".
	// A trailing slash is trimmed. Required.
	BaseURL string
	// Token supplies the bearer token for each request. Required; typically
	// [ContextTokenSource] for user-scoped runs or [StaticTokenSource] for a
	// service account.
	Token TokenSource
	// HTTPClient is the client used for all requests. When nil a default
	// client with no timeout is used (per-request deadlines come from the
	// caller's context, which is the right knob for long-running execs).
	HTTPClient *http.Client
}

// Provider implements [sandbox.Provider] against the Cella /v1 API. It is safe
// for concurrent use.
type Provider struct {
	baseURL string
	token   TokenSource
	http    *http.Client

	// pollInterval overrides the log-poll cadence; 0 uses defaultPollInterval.
	// Set in tests to keep streaming exec fast; not part of the public API.
	pollInterval time.Duration
}

// New returns a Provider configured by opts. It panics if BaseURL or Token is
// empty, since neither has a sensible default and a misconfigured provider
// would fail every call.
func New(opts Options) *Provider {
	if opts.BaseURL == "" {
		panic("cella.New: BaseURL is required")
	}
	if opts.Token == nil {
		panic("cella.New: Token is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Provider{
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		token:   opts.Token,
		http:    hc,
	}
}

// send builds and executes a request to path (e.g. "/v1/sandboxes"), attaching
// the bearer token. body may be nil. contentType is set only when non-empty.
// The caller owns the returned response body.
func (p *Provider) send(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("cella: build request: %w", err)
	}
	tok, err := p.token.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cella: %s %s: %w", method, path, err)
	}
	return resp, nil
}

// doJSON sends a request with an optional JSON body and decodes a JSON
// response into out (which may be nil to discard the body). Non-2xx responses
// are mapped through [mapError]. The response body is always closed.
func (p *Provider) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("cella: marshal request: %w", err)
		}
		body = strings.NewReader(string(raw))
		contentType = "application/json"
	}
	resp, err := p.send(ctx, method, path, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mapError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("cella: decode response: %w", err)
	}
	return nil
}

// errorEnvelope is Cella's JSON error body: {code, message, request_id}.
type errorEnvelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// mapError converts a non-2xx response into the error contract the
// [sandbox.Provider] interface specifies: 404 -> [sandbox.ErrNotFound],
// 409 -> [sandbox.ErrConflict], everything else -> [*sandbox.APIError]. It
// consumes (but does not close) the response body; the caller owns closing.
func mapError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env errorEnvelope
	_ = json.Unmarshal(raw, &env) // best effort; body may be empty or non-JSON

	switch resp.StatusCode {
	case http.StatusNotFound:
		return sandbox.ErrNotFound
	case http.StatusConflict:
		return sandbox.ErrConflict
	default:
		return &sandbox.APIError{
			Status:    resp.StatusCode,
			Code:      env.Code,
			Message:   env.Message,
			RequestID: env.RequestID,
		}
	}
}
