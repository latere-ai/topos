// Package cella implements SandboxProvider against Cella's public sandbox
// API (/v1/sandboxes, /v1/sandboxes/{id}/commands). All Cella request and
// response types are private to this package; upstream code depends only on
// the sandbox.SandboxProvider interface and shared types in the parent package.
package cella

import "context"

// TokenSource yields a bearer token for each outbound Cella request.
// Implementations MUST be safe for concurrent use.
//
// This is the trust-plane swap-point: Phase 1 wires a static service token
// from config or environment; a later phase replaces it with an Auth-minted
// short-TTL Cella token via the trust-plane sidecar — no interface change,
// no upstream-caller change required.
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
