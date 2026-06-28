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
// The Cella backend does not mint tokens itself: exchanging an upstream user
// JWT for a Cella bearer (POST /v1/tokens/exchange) is the host's job, done
// once at run start. A TokenSource only supplies whatever token the host has
// already arranged.
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
// [sandbox.WithBearer]. This is how a host scopes an entire agent run — the
// entry agent plus every delegated peer's create/exec/destroy — to the session
// user's identity: it bridges the inbound user JWT to a user-subject Cella
// bearer once at run start, stores it with sandbox.WithBearer, and threads the
// resulting context through every sandbox call.
type ContextTokenSource struct{}

// Token returns the bearer carried by ctx, or an error if none was set.
func (ContextTokenSource) Token(ctx context.Context) (string, error) {
	tok, ok := sandbox.BearerFromContext(ctx)
	if !ok || tok == "" {
		return "", errors.New("cella: no bearer token in context; call sandbox.WithBearer before the run")
	}
	return tok, nil
}
