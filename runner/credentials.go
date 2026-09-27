// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"sync"
	"time"

	"latere.ai/x/topos/session"
)

// AudienceLux is the audience that answers the session's Lux key for a
// workload; every other audience answers a token for that core (spec
// 018).
const AudienceLux = "lux"

// The workloads a session's credentials are for: the runner's own calls,
// or the session's sandbox.
const (
	WorkloadSession = "session"
	WorkloadSandbox = "sandbox"
)

// RefreshBefore is how long before its expiry a held credential is asked
// for again.
const RefreshBefore = 2 * time.Minute

// ErrNotMinted is an installation that mints no credential for the
// audience asked: it has no identity provider, or its authorizer creates
// no session keys, and the runner acts with the installation's own
// credential instead.
var ErrNotMinted = errors.New("runner: the installation mints no credential for this audience")

// CodeAgentIdentityMissing is the setup code of a session whose agent
// has no identity at the installation's identity provider.
const CodeAgentIdentityMissing = "agent_identity_missing"

// ErrNoIdentity is a token asked for a session whose agent has no
// identity at the installation's identity provider: it was applied
// before the installation had one.
var ErrNoIdentity = errors.New("runner: the session's agent has no identity at the identity provider; apply it again")

// Credential is a value a session acts with, a token or a Lux key, and
// when it expires. It is held in memory and never appended.
type Credential struct {
	Value     string
	ExpiresAt time.Time
}

// Credentials reaches the credentials of one session for the holder of
// its lease. A lease that implements it is asked directly, as a remote
// runner's claim is; Options.Credentials builds one for any other lease.
type Credentials interface {
	Credential(ctx context.Context, audience, workload string) (Credential, error)
}

// TokenSource is a drive's view of its session's credentials: it holds
// each answer until RefreshBefore its expiry, refuses once the drive's
// lease is lost, and drops everything it holds on Drop, which the runner
// calls after every session.scope_changed, so no credential crosses a
// change of the session's scope.
type TokenSource struct {
	c    Credentials
	lost <-chan struct{}
	now  func() time.Time

	mu    sync.Mutex
	held  map[[2]string]Credential
	drops int
}

// NewTokenSource builds the source of one drive over c, refusing once
// lost is closed.
func NewTokenSource(c Credentials, lost <-chan struct{}, now func() time.Time) *TokenSource {
	if now == nil {
		now = time.Now
	}
	return &TokenSource{c: c, lost: lost, now: now, held: map[[2]string]Credential{}}
}

// Token answers the credential for audience and workload.
func (s *TokenSource) Token(ctx context.Context, audience, workload string) (Credential, error) {
	select {
	case <-s.lost:
		s.Drop()
		return Credential{}, ErrLeaseLost
	default:
	}
	key := [2]string{audience, workload}
	s.mu.Lock()
	c, ok := s.held[key]
	drops := s.drops
	s.mu.Unlock()
	if ok && s.now().Add(RefreshBefore).Before(c.ExpiresAt) {
		return c, nil
	}
	c, err := s.c.Credential(ctx, audience, workload)
	if err != nil {
		return Credential{}, err
	}
	s.mu.Lock()
	// An answer asked for before a Drop is not held past it.
	if s.drops == drops {
		s.held[key] = c
	}
	s.mu.Unlock()
	return c, nil
}

// Drop forgets every credential held, so the next Token asks again.
func (s *TokenSource) Drop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.held)
	s.drops++
}

type tokensKey struct{}

// WithTokens is ctx carrying a drive's token source, which the harness
// builder reads with TokensFrom.
func WithTokens(ctx context.Context, s *TokenSource) context.Context {
	return context.WithValue(ctx, tokensKey{}, s)
}

// TokensFrom is the token source of the drive ctx belongs to, nil when
// the runner reaches no session credentials, as on a person's own
// machine.
func TokensFrom(ctx context.Context) *TokenSource {
	s, _ := ctx.Value(tokensKey{}).(*TokenSource)
	return s
}

// tokens is ctx with the token source of a drive holding lease on the
// session id, and the source; ctx and nil when the runner reaches no
// session credentials.
func (r *Runner) tokens(ctx context.Context, id string, lease session.Lease) (context.Context, *TokenSource) {
	var c Credentials
	if lc, ok := lease.(Credentials); ok {
		c = lc
	} else if r.o.Credentials != nil {
		c = r.o.Credentials(id, lease)
	}
	if c == nil {
		return ctx, nil
	}
	src := NewTokenSource(c, lease.Lost(), r.o.Clock)
	return WithTokens(ctx, src), src
}
