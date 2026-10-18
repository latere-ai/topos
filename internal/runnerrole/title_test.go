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

// titleServer is a runner protocol server whose title question is
// answer, nil for none, and a client of it.
func titleServer(t *testing.T, answer func(context.Context, string, string) (session.ModelRef, error)) (session.Store, *Client) {
	t.Helper()
	st := session.NewMemoryStore()
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"}, Title: answer})
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

// TestARemoteLeaseAsksTheTitleQuestion: a remote runner's lease asks the
// title question over the internal listener (spec 083): the holder at
// its generation is answered the model the server's allow names, with
// its route, asked the name its request asks for; a question the server
// refuses reaches the runner as an error naming why, and so does a server
// that asks none; a stale generation is lease_lost and asks nothing; a
// request that names no model is invalid.
func TestARemoteLeaseAsksTheTitleQuestion(t *testing.T) {
	var asked []string
	var refuse error
	st, c := titleServer(t, func(_ context.Context, id, name string) (session.ModelRef, error) {
		asked = append(asked, id+" "+name)
		if refuse != nil {
			return session.ModelRef{}, refuse
		}
		return session.ModelRef{Name: "vendor/small", Route: "tier/small"}, nil
	})
	ctx := t.Context()
	s := hosted(t, st, "Go.")
	claims, err := c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	l, ok := claims[0].Lease.(runner.Titler)
	if !ok {
		t.Fatal("a remote lease does not ask the title question")
	}
	if m, err := l.Title(ctx, "tier/quick"); err != nil || m != (session.ModelRef{Name: "vendor/small", Route: "tier/small"}) {
		t.Fatalf("the holder was answered %+v, %v", m, err)
	}
	refuse = errors.New("titles_off")
	if _, err := l.Title(ctx, "tier/quick"); err == nil || !strings.Contains(err.Error(), runnerapi.CodeNoTitle) || !strings.Contains(err.Error(), "titles_off") {
		t.Fatalf("a refused question: %v", err)
	}
	if len(asked) != 2 || asked[0] != s.ID+" tier/quick" {
		t.Fatalf("the server was asked %q", asked)
	}
	held := claims[0].Lease.(*lease)
	stale := &lease{c: c, id: s.ID, gen: held.gen + 1, lost: make(chan struct{}), stop: make(chan struct{})}
	if _, err := stale.Title(ctx, "tier/quick"); !errors.Is(err, session.ErrLeaseLost) || len(asked) != 2 {
		t.Fatalf("another generation: %v, %d questions", err, len(asked))
	}
	if _, err := l.Title(ctx, ""); !errors.Is(err, session.ErrInvalid) || len(asked) != 2 {
		t.Fatalf("no model asked: %v, %d questions", err, len(asked))
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}

	st, c = titleServer(t, nil)
	hosted(t, st, "Go.")
	claims, err = c.Claim(ctx, session.Holder{Runner: "r1"}, 1, time.Second)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim %+v %v", claims, err)
	}
	if _, err := claims[0].Lease.(runner.Titler).Title(ctx, "tier/quick"); err == nil || !strings.Contains(err.Error(), "asks no title question") {
		t.Fatalf("a server that asks none: %v", err)
	}
	if err := claims[0].Lease.Release(); err != nil {
		t.Fatal(err)
	}
}
