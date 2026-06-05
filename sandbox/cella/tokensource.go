// Package cella implements SandboxProvider against Cella's public sandbox
// API (/v1/sandboxes, /v1/sandboxes/{id}/commands). All Cella request and
// response types are private to this package; upstream code depends only on
// the sandbox.SandboxProvider interface and shared types in the parent package.
package cella

import (
	"context"
	"errors"

	"latere.ai/x/agents/internal/sandbox"
)

// TokenSource yields a bearer token for each outbound Cella request.
// Implementations MUST be safe for concurrent use.
//
// This is the trust-plane swap-point. Two implementations ship:
//   - StaticTokenSource: a single fixed service credential (local dev).
//   - ContextTokenSource: reads a per-request, per-user bearer from the
//     context (production), so each agent run acts as its session user.
//
// No interface change and no upstream-caller change is required to swap them.
type TokenSource interface {
	// Token returns a valid bearer token for the current point in time.
	// It may refresh or re-derive the token on each call. Callers MUST
	// NOT cache the returned value; call Token per request.
	Token(ctx context.Context) (string, error)
}

// StaticTokenSource is a TokenSource backed by a single fixed token. It
// satisfies Phase-1 requirements (static service credential from config or
// environment) and is the default implementation shipped with the package.
type StaticTokenSource struct {
	token string
}

// NewStaticTokenSource returns a TokenSource that always returns the given
// token. It is safe for concurrent use.
func NewStaticTokenSource(token string) *StaticTokenSource {
	return &StaticTokenSource{token: token}
}

// Token implements TokenSource.
func (s *StaticTokenSource) Token(_ context.Context) (string, error) {
	return s.token, nil
}

// errNoContextBearer is returned by ContextTokenSource when the context
// carries no bearer and no fallback is configured. It fails closed: a
// production provider must never silently downgrade to a service identity.
var errNoContextBearer = errors.New("cella: no per-request bearer in context and no fallback token source")

// ContextTokenSource yields the per-request bearer carried by the context
// (see sandbox.WithBearer), so each outbound Cella request is authenticated as
// the run's session user. When the context carries no bearer it consults an
// optional fallback; with no fallback it returns an error (fail closed).
//
// Production wiring uses ContextTokenSource{fallback: nil} so an absent user
// bearer is an error, never a silent downgrade to a static service credential.
// Local dev uses StaticTokenSource directly instead.
type ContextTokenSource struct {
	fallback TokenSource
}

// NewContextTokenSource returns a ContextTokenSource. Pass a nil fallback for
// production (absent bearer → error); pass a StaticTokenSource fallback only
// where a service-identity downgrade is acceptable.
func NewContextTokenSource(fallback TokenSource) *ContextTokenSource {
	return &ContextTokenSource{fallback: fallback}
}

// Token implements TokenSource. It returns the context bearer when present,
// otherwise the fallback's token, otherwise errNoContextBearer.
func (s *ContextTokenSource) Token(ctx context.Context) (string, error) {
	if tok, ok := sandbox.BearerFromContext(ctx); ok && tok != "" {
		return tok, nil
	}
	if s.fallback != nil {
		return s.fallback.Token(ctx)
	}
	return "", errNoContextBearer
}
