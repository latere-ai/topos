// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// asked records what the token route asked of the minter.
type asked struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (a *asked) credentials(_ context.Context, id, lease, audience, workload string) (runner.Credential, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return runner.Credential{}, a.err
	}
	a.calls = append(a.calls, id+" "+lease+" "+audience+" "+workload)
	return runner.Credential{Value: "tok-" + audience, ExpiresAt: time.Now().Add(15 * time.Minute)}, nil
}

func (a *asked) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func tokenServer(t *testing.T, a *asked) (session.Store, *Client) {
	t.Helper()
	st := session.NewMemoryStore()
	o := runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"}}
	if a != nil {
		o.Credentials = a.credentials
	}
	srv, err := runnerapi.New(o)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRenewInterval(time.Hour)
	return st, c
}

// TestTokensOnlyForTheLeaseHolder, the runner protocol's half: the
// token route answers the holder of the session's lease at its current
// generation, naming the claim to the minter, and answers lease_lost to
// a stale generation and to a released lease without asking the minter;
// a minter's refusal reaches the runner as the setup error it was, and
// an installation that mints nothing says not_minted.
func TestTokensOnlyForTheLeaseHolder(t *testing.T) {
	a := &asked{}
	st, c := tokenServer(t, a)
	ctx := t.Context()
	s := hosted(t, st, "Go.")
	claims, err := c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 || claims[0].ID != s.ID {
		t.Fatalf("claim %+v %v", claims, err)
	}
	l := claims[0].Lease.(*lease)
	cred, err := l.Credential(ctx, "cella", runner.WorkloadSandbox)
	if err != nil || cred.Value != "tok-cella" || time.Until(cred.ExpiresAt) < 14*time.Minute {
		t.Fatalf("the holder's token %+v %v", cred, err)
	}
	if a.calls[0] != s.ID+" claim:1 cella sandbox" {
		t.Fatalf("the minter was asked %q", a.calls[0])
	}
	stale := &lease{c: c, id: s.ID, gen: l.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.Credential(ctx, "cella", runner.WorkloadSession); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("another generation: %v", err)
	}
	select {
	case <-stale.Lost():
	default:
		t.Fatal("a lease_lost answer did not end the lease")
	}
	for _, bad := range []runnerapi.TokenRequest{{Generation: l.gen, Workload: runner.WorkloadSession}, {Generation: l.gen, Audience: "cella", Workload: "other"}} {
		if err := c.json(ctx, "POST", "/leases/"+s.ID+"/tokens", bad, nil); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	a.err = &runner.SetupError{Code: runner.CodeAgentIdentityMissing, Err: runner.ErrNoIdentity}
	var se *runner.SetupError
	if _, err := l.Credential(ctx, "cella", runner.WorkloadSession); !errors.As(err, &se) || se.Code != runner.CodeAgentIdentityMissing || !errors.Is(err, runner.ErrNoIdentity) {
		t.Fatalf("a missing identity: %v", err)
	}
	a.err = &runner.SetupError{Code: "session_unknown", Err: errors.New("the authorizer has no record")}
	if _, err := l.Credential(ctx, runner.AudienceLux, runner.WorkloadSession); !errors.As(err, &se) || se.Code != "session_unknown" {
		t.Fatalf("a refused key: %v", err)
	}
	a.err = &runner.SetupError{Code: "agent_disabled"}
	if _, err := l.Credential(ctx, "cella", runner.WorkloadSession); !errors.As(err, &se) || se.Code != "agent_disabled" {
		t.Fatalf("a refusal with no cause: %v", err)
	}
	a.err = runner.ErrNotMinted
	if _, err := l.Credential(ctx, runner.AudienceLux, runner.WorkloadSession); !errors.Is(err, runner.ErrNotMinted) {
		t.Fatalf("an installation that mints nothing: %v", err)
	}
	a.err = nil
	n := a.count()
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Credential(ctx, "cella", runner.WorkloadSession); !errors.Is(err, session.ErrLeaseLost) || a.count() != n {
		t.Fatalf("a released lease: %v, %d calls", err, a.count())
	}
}

// TestTheTokenRouteWithoutAMinter: a server with no minter answers the
// lease holder not_minted.
func TestTheTokenRouteWithoutAMinter(t *testing.T) {
	st, c := tokenServer(t, nil)
	hosted(t, st, "Go.")
	claims, err := c.Claim(t.Context(), session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	if _, err := claims[0].Lease.(runner.Credentials).Credential(t.Context(), "cella", runner.WorkloadSession); !errors.Is(err, runner.ErrNotMinted) {
		t.Fatalf("no minter: %v", err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
