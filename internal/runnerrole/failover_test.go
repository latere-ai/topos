// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// failoverServer is a runner protocol server whose failover question is
// answer, nil for none, a client of it, and the bodies of the failover
// requests it received.
func failoverServer(t *testing.T, answer func(context.Context, string, session.ModelRef, session.ModelRef, string, string) (session.ModelRef, error)) (session.Store, *Client, func() []string) {
	t.Helper()
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"}, Failover: answer})
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu     sync.Mutex
		bodies []string
	)
	h := srv.Handler()
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/failover") {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			bodies = append(bodies, string(b))
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(b))
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.Close)
	c, err := New(hs.URL, "runner-token", hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	c.SetRenewInterval(time.Hour)
	return st, c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// TestARemoteLeaseAsksTheServerToFailOver: a remote runner's lease asks
// the failover question over the internal listener (spec 051): the holder
// at its generation is answered the server's model, with the model the
// turn stands on, the failed model, the reason and the detail passed on;
// the request names the standing model only when it is not the failed
// one, and one that names none stands on the failed one; it carries a
// reason only when there is one; a stale generation is lease_lost and
// asks nothing; a question the server cannot answer reaches the runner as
// an error naming why; a server with no question answers the model the
// turn stands on, so nothing moves; a request that names no failed model,
// or a reason the server does not know, is invalid.
func TestARemoteLeaseAsksTheServerToFailOver(t *testing.T) {
	failed := session.ModelRef{Name: "vendor/model-a", Via: "tier/quick"}
	named := session.ModelRef{Name: "vendor/model-c", Via: "tier/quick"}
	var asked []string
	var refuse error
	st, c, bodies := failoverServer(t, func(_ context.Context, id string, on, f session.ModelRef, reason, detail string) (session.ModelRef, error) {
		asked = append(asked, id+" "+on.Name+" "+f.Name+" "+f.Via+" "+reason+" "+detail)
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
	next, err := l.Failover(ctx, failed, failed, "", "upstream status 429")
	if err != nil || next != (session.ModelRef{Name: "vendor/model-b", Via: "tier/quick"}) {
		t.Fatalf("the holder was answered %+v, %v", next, err)
	}
	if _, err := l.Failover(ctx, failed, named, "", "could not be connected"); err != nil {
		t.Fatal(err)
	}
	held := claims[0].Lease.(*lease)
	if err := c.json(ctx, "POST", "/leases/"+s.ID+"/failover", runnerapi.FailoverRequest{Generation: held.gen, Failed: failed}, nil); err != nil {
		t.Fatalf("a runner that names no standing model: %v", err)
	}
	if _, err := l.Failover(ctx, failed, failed, harness.FailedRejected, "upstream_rejected: upstream status 404"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Failover(ctx, failed, failed, harness.FailedToolAsText, "a call of question was written as text, again after a reminder"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 5 || asked[0] != s.ID+" vendor/model-a vendor/model-a tier/quick  upstream status 429" ||
		asked[1] != s.ID+" vendor/model-a vendor/model-c tier/quick  could not be connected" || asked[2] != s.ID+" vendor/model-a vendor/model-a tier/quick  " ||
		asked[3] != s.ID+" vendor/model-a vendor/model-a tier/quick rejected upstream_rejected: upstream status 404" ||
		asked[4] != s.ID+" vendor/model-a vendor/model-a tier/quick tool_as_text a call of question was written as text, again after a reminder" {
		t.Fatalf("the server was asked %q", asked)
	}
	if b := bodies(); len(b) != 5 || strings.Contains(b[0], `"standing"`) || !strings.Contains(b[1], `"standing":{"name":"vendor/model-a","via":"tier/quick"}`) ||
		strings.Contains(b[0], `"reason"`) || !strings.Contains(b[3], `"reason":"rejected"`) || !strings.Contains(b[4], `"reason":"tool_as_text"`) {
		t.Fatalf("the requests' bodies %q", b)
	}
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.Failover(ctx, failed, failed, "", ""); !errors.Is(err, session.ErrLeaseLost) || len(asked) != 5 {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	refuse = errors.New("model_tier_quick_unavailable")
	if _, err := l.Failover(ctx, failed, failed, "", ""); err == nil || !strings.Contains(err.Error(), runnerapi.CodeNoFailover) || !strings.Contains(err.Error(), "model_tier_quick_unavailable") {
		t.Fatalf("a refused question: %v", err)
	}
	if err := c.json(ctx, "POST", "/leases/"+s.ID+"/failover", runnerapi.FailoverRequest{Generation: held.gen}, nil); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("no failed model: %v", err)
	}
	if _, err := l.Failover(ctx, failed, failed, "throttled", ""); !errors.Is(err, session.ErrInvalid) || len(asked) != 6 {
		t.Fatalf("an unknown reason: %v, %d questions", err, len(asked))
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}

	st, c, _ = failoverServer(t, nil)
	hosted(t, st, "Go.")
	claims, err = c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	if next, err := claims[0].Lease.(runner.Failover).Failover(ctx, failed, named, "", ""); err != nil || next != failed {
		t.Fatalf("a server with no question: %+v, %v", next, err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
