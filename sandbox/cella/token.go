// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package cella

import (
	"context"
	"errors"

	"latere.ai/x/topos/sandbox"
)

// TokenSource yields the bearer token to present on each outbound request to
// the Cella API. Implementations MUST be safe for concurrent use.
//
// Cella issues no credential of its own. The token to present is the one the
// caller's own issuer mints for Cella and for nobody else, short-lived by
// design (300 seconds at the hosted deployment), and it goes on the request
// as it was minted. Obtaining it, and re-minting it before it lapses, is the
// host's job; a TokenSource only supplies whatever token the host holds at the
// moment of the call.
//
// The lifetime is the thing to design around: a run outlives one token, so a
// host arranges a source that hands out a valid token per request rather than
// a bearer bridged once at run start.
type TokenSource interface {
	// Token returns the bearer token to use for a request made under ctx.
	Token(ctx context.Context) (string, error)
}

// StaticTokenSource returns the same fixed token for every request. Suitable
// for service-account credentials and local development where one token is
// used for the whole process.
type StaticTokenSource string

// Token returns the fixed token.
func (s StaticTokenSource) Token(context.Context) (string, error) {
	if s == "" {
		return "", errors.New("cella: static token is empty")
	}
	return string(s), nil
}

// TokenFunc adapts a plain function to a [TokenSource], so a caller can supply
// the current bearer without defining a type. The SDK calls it on every request,
// so returning the owner's latest token makes credential rotation flow through
// automatically: when the owner refreshes the token out of band, the next
// request (including those deep inside a long-running Run) picks up the new
// value with no further plumbing.
//
// Keep the function cheap — return a cached current token; do the actual refresh
// out of band rather than blocking here.
type TokenFunc func(ctx context.Context) (string, error)

// Token calls the wrapped function.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// ContextTokenSource reads a per-request bearer from the context, as set by
// [sandbox.WithBearer]. It scopes a call to the identity the context carries,
// so the entry agent and every delegated peer's create/exec/destroy act as the
// session user when the host threads one context through the whole run.
//
// The bearer is fixed for whatever context is passed, so a run longer than the
// token's few minutes needs a fresh context per leg. A run that cannot promise
// that is better served by [TokenFunc], which is asked per request and can
// return a freshly minted token.
type ContextTokenSource struct{}

// Token returns the bearer carried by ctx, or an error if none was set.
func (ContextTokenSource) Token(ctx context.Context) (string, error) {
	tok, ok := sandbox.BearerFromContext(ctx)
	if !ok || tok == "" {
		return "", errors.New("cella: no bearer token in context; call sandbox.WithBearer before the run")
	}
	return tok, nil
}
