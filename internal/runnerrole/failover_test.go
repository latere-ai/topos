// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// failoverServer is a runner protocol server whose failover question is
// answer, nil for none, and a client of it.
func failoverServer(t *testing.T, answer func(context.Context, string, session.ModelRef, session.ModelRef, string) (session.ModelRef, error)) (session.Store, *Client) {
	t.Helper()
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"}, Failover: answer})
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

// TestARemoteLeaseAsksTheServerToFailOver: a remote runner's lease asks
// the failover question over the internal listener (spec 051): the holder
// at its generation is answered the server's model, with the model the
// turn stands on, the failed model and the detail passed on, and a request
// from a runner that names no standing model stands on the failed one; a
// stale generation is lease_lost and asks nothing; a question the server
// cannot answer reaches the runner as an error naming why; a server with
// no question answers the model the turn stands on, so nothing moves; a
// request that names no failed model is invalid.
func TestARemoteLeaseAsksTheServerToFailOver(t *testing.T) {
	failed := session.ModelRef{Name: "vendor/model-a", Via: "tier/quick"}
	named := session.ModelRef{Name: "vendor/model-c", Via: "tier/quick"}
	var asked []string
	var refuse error
	st, c := failoverServer(t, func(_ context.Context, id string, on, f session.ModelRef, detail string) (session.ModelRef, error) {
		asked = append(asked, id+" "+on.Name+" "+f.Name+" "+f.Via+" "+detail)
		if refuse != nil {
			return session.ModelRef{}, refuse
		}
		return session.ModelRef{Name: "vendor/model-b", Via: f.Via}, nil
	})
	ctx := t.Context()
	s := hosted(t, st, "Go.")
	claims, err := c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	l, ok := claims[0].Lease.(runner.Failover)
	if !ok {
		t.Fatal("a remote lease does not ask the failover question")
	}
	next, err := l.Failover(ctx, failed, failed, "upstream status 429")
	if err != nil || next != (session.ModelRef{Name: "vendor/model-b", Via: "tier/quick"}) {
		t.Fatalf("the holder was answered %+v, %v", next, err)
	}
	if _, err := l.Failover(ctx, failed, named, "could not be connected"); err != nil {
		t.Fatal(err)
	}
	held := claims[0].Lease.(*lease)
	if err := c.json(ctx, "POST", "/leases/"+s.ID+"/failover", runnerapi.FailoverRequest{Generation: held.gen, Failed: failed}, nil); err != nil {
		t.Fatalf("a runner that names no standing model: %v", err)
	}
	if len(asked) != 3 || asked[0] != s.ID+" vendor/model-a vendor/model-a tier/quick upstream status 429" ||
		asked[1] != s.ID+" vendor/model-a vendor/model-c tier/quick could not be connected" || asked[2] != s.ID+" vendor/model-a vendor/model-a tier/quick " {
		t.Fatalf("the server was asked %q", asked)
	}
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.Failover(ctx, failed, failed, ""); !errors.Is(err, session.ErrLeaseLost) || len(asked) != 3 {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	refuse = errors.New("model_tier_quick_unavailable")
	if _, err := l.Failover(ctx, failed, failed, ""); err == nil || !strings.Contains(err.Error(), runnerapi.CodeNoFailover) || !strings.Contains(err.Error(), "model_tier_quick_unavailable") {
		t.Fatalf("a refused question: %v", err)
	}
	if err := c.json(ctx, "POST", "/leases/"+s.ID+"/failover", runnerapi.FailoverRequest{Generation: held.gen}, nil); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("no failed model: %v", err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}

	st, c = failoverServer(t, nil)
	hosted(t, st, "Go.")
	claims, err = c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	if next, err := claims[0].Lease.(runner.Failover).Failover(ctx, failed, named, ""); err != nil || next != failed {
		t.Fatalf("a server with no question: %+v, %v", next, err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
