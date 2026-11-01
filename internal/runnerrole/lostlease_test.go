// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runnerrole

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/runnerapi"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// resumeBound is spec 016's acceptance bound: the claim's TTL, plus the
// margin its reaper's cadence leaves (5 seconds in production) folded
// into a round 15 seconds.
const resumeBound = runnerapi.DefaultTTL + 15*time.Second

// TestLostLeaseResumes: a runner that stops renewing its claim loses it
// no later than the claim's TTL after it was made, and another runner
// resumes the session within resumeBound of that claim, continuing the
// session log with the person's message answered once, no event lost
// or appended twice (spec 016). The claim's own clock is a fake one, so
// the test proves the server's real TTL rather than a shortened one.
func TestLostLeaseResumes(t *testing.T) {
	st := session.NewMemoryStore()
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	srv, err := runnerapi.New(runnerapi.Options{Store: st, Queue: runner.NewQueue(st, 10*time.Millisecond), Tokens: []string{"runner-token"}, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	// The reaper's own real-time cadence is not what this test proves;
	// a short one just keeps the test fast. What it frees is gated on
	// the fake clock above.
	reapCtx, cancelReap := context.WithCancel(t.Context())
	defer cancelReap()
	go srv.Reap(reapCtx, 10*time.Millisecond)

	a, err := New(hs.URL, "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := hosted(t, st, "Go.")
	t0 := clock()
	claims, err := a.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 0)
	if err != nil || len(claims) != 1 {
		t.Fatalf("A's claim: %+v, %v", claims, err)
	}
	fence := claims[0].Lease.(session.Fence)
	defer func() { _ = claims[0].Lease.Release() }()

	// A is still alive a second short of the TTL: an append under its
	// lease still goes through. Append does not renew the claim, so
	// what follows still tests the claim A made at t0.
	advance(runnerapi.DefaultTTL - time.Second)
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "run_a", Kind: runner.KindRunner}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{e}
	session.Stamp(s.ID, 1, batch)
	if _, err := fence.Append(t.Context(), 1, batch); err != nil {
		t.Fatalf("A's append a second short of the TTL: %v", err)
	}

	// Past the TTL since the claim, A's lease is lost: a runner that
	// stops renewing loses the session no later than that.
	advance(2 * time.Second)
	if err := claims[0].Lease.Renew(t.Context()); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("A's renew past the TTL: %v", err)
	}

	// B resumes the session. The reaper frees A's claim on the store's
	// lease once the fake clock reads past its expiry, so B's claim
	// succeeds once that has happened, within resumeBound of A's claim
	// (A never renewed again, so the claim is the last renew there was).
	b, err := New(hs.URL, "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	stub := luxstub.New(t)
	stub.Script("builder-model", luxstub.Reply{Response: ir.Response{Model: "builder-model", Blocks: []ir.Block{{Type: ir.BlockText, Text: "resumed"}}, StopReason: ir.StopEndTurn}})
	base := t.TempDir()
	rb, err := runner.New(runner.Options{Store: b, ID: "run_b", Kind: runner.KindRunner,
		Harness: func(ctx context.Context, sess session.Session) (harness.Config, error) {
			m, err := host.Open(host.Options{Workdir: base, SpillDir: filepath.Join(base, ".spill"), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: &dialect.Model{}, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "builder-model", Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: tools.NewRegistry(),
				Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	bctx, cancelB := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rb.Serve(bctx, b, 1, func(id string, err error) { t.Errorf("%s: %v", id, err) }) }()

	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		got, err := st.Get(t.Context(), s.ID)
		if err == nil && got.Status == session.StatusIdle && got.StopReason == session.StopEndTurn {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never resumed the session: %+v, %v", got, err)
		}
	}
	cancelB()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if resumed := clock().Sub(t0); resumed > resumeBound {
		t.Fatalf("B resumed %s after A's claim, past the spec's bound of %s", resumed, resumeBound)
	}

	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var userMessages, running int
	var runnerIDs []string
	for _, ev := range evs {
		switch ev.Type {
		case session.TypeUserMessage:
			userMessages++
		case session.TypeSessionStatus:
			var p session.SessionStatus
			if err := ev.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.Status == session.StatusRunning && p.Runner != nil {
				running++
				runnerIDs = append(runnerIDs, p.Runner.ID)
			}
		}
	}
	if userMessages != 1 {
		t.Fatalf("the person's message appears %d times in the resumed log", userMessages)
	}
	if running != 2 || runnerIDs[0] != "run_a" || runnerIDs[1] != "run_b" {
		t.Fatalf("the session's running turns: %v", runnerIDs)
	}
	last := evs[len(evs)-1].Seq

	// A's lease is long gone: a late append under it still refuses, and
	// the log it left for B is unchanged, so nothing A might still send
	// reaches the log a second time.
	if _, err := fence.Append(t.Context(), last, batch); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("A's append after losing the claim: %v", err)
	}
	if got, err := st.Get(t.Context(), s.ID); err != nil || got.LastSeq != last {
		t.Fatalf("A's refused append changed the log: %+v, %v", got, err)
	}
}
