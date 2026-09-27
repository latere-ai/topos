// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// hosted creates a hosted session in st and, when text is set, its first
// message.
func hosted(t *testing.T, st session.Store, text string) session.Session {
	t.Helper()
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: machine.KindHost}, t0)
	if err := st.Create(t.Context(), s, nil); err != nil {
		t.Fatal(err)
	}
	if text != "" {
		appendTo(t, st, s.ID, session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}})
	}
	return s
}

func appendTo(t *testing.T, st session.Store, id string, typ session.Type, payload any) {
	t.Helper()
	s, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	e, err := session.NewEvent(typ, payload, t0)
	if err != nil {
		t.Fatal(err)
	}
	batch := []session.Event{e}
	session.Stamp(id, s.LastSeq, batch)
	if _, err := st.Append(t.Context(), id, s.LastSeq, batch); err != nil {
		t.Fatal(err)
	}
}

func ids(claims []Claim) []string {
	var out []string
	for _, c := range claims {
		out = append(out, c.ID)
	}
	slices.Sort(out)
	return out
}

// TestQueueClaimsHostedSessionsWithWork: a hosted session idle with a
// message, or running with no live lease, is claimed with its lease; an
// answered one, an external one and a held one are not; new input and a
// released lease make a session claimable again.
func TestQueueClaimsHostedSessionsWithWork(t *testing.T) {
	st := session.NewMemoryStore()
	waiting := hosted(t, st, "Go.")
	answered := hosted(t, st, "Go.")
	appendTo(t, st, answered.ID, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn})
	abandoned := hosted(t, st, "Go.")
	appendTo(t, st, abandoned.ID, session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning})
	ext := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Version: 1}, session.Sender{Subject: "u"}, session.RunnerExternal, session.Machine{}, t0)
	if err := st.Create(t.Context(), ext, nil); err != nil {
		t.Fatal(err)
	}
	appendTo(t, st, ext.ID, session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "x"}}})
	hosted(t, st, "")

	q := NewQueue(st, time.Hour)
	a := session.Holder{Runner: "run_a"}
	claims, err := q.Claim(t.Context(), a, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{waiting.ID, abandoned.ID}
	slices.Sort(want)
	if got := ids(claims); !slices.Equal(got, want) {
		t.Fatalf("claimed %v, want %v", got, want)
	}
	if again, err := q.Claim(t.Context(), session.Holder{Runner: "run_b"}, 10, 0); err != nil || len(again) != 0 {
		t.Fatalf("another runner claimed %v, %v", ids(again), err)
	}
	if err := release(claims); err != nil {
		t.Fatal(err)
	}
	if one, err := q.Claim(t.Context(), a, 1, 0); err != nil || len(one) != 1 {
		t.Fatalf("a claim of one: %v, %v", ids(one), err)
	} else if err := release(one); err != nil {
		t.Fatal(err)
	}
	appendTo(t, st, answered.ID, session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "More."}}})
	claims, err = q.Claim(t.Context(), a, 10, 0)
	if err != nil || !slices.Contains(ids(claims), answered.ID) {
		t.Fatalf("a message to an answered session: %v, %v", ids(claims), err)
	}
	if err := release(claims); err != nil {
		t.Fatal(err)
	}
}

// TestQueueWakesOnNotify: a claim waiting for work returns as soon as
// work arrives and Notify is called, well before its poll.
func TestQueueWakesOnNotify(t *testing.T) {
	st := session.NewMemoryStore()
	q := NewQueue(st, time.Hour)
	got := make(chan []Claim, 1)
	go func() {
		claims, err := q.Claim(t.Context(), session.Holder{Runner: "run_a"}, 1, 30*time.Second)
		if err != nil {
			t.Error(err)
		}
		got <- claims
	}()
	time.Sleep(20 * time.Millisecond)
	s := hosted(t, st, "Go.")
	q.Notify()
	select {
	case claims := <-got:
		if len(claims) != 1 || claims[0].ID != s.ID {
			t.Fatalf("claimed %v", ids(claims))
		}
		if err := release(claims); err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the claim did not wake")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if claims, err := q.Claim(ctx, session.Holder{Runner: "run_a"}, 1, time.Minute); err != nil || claims != nil {
		t.Fatalf("a canceled claim: %v, %v", claims, err)
	}
}

// failingClaimer answers errors, then nothing.
type failingClaimer struct {
	mu    sync.Mutex
	calls int
}

func (f *failingClaimer) Claim(ctx context.Context, _ session.Holder, _ int, _ time.Duration) ([]Claim, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return nil, errors.New("store down")
}

// served is a runner over a memory store whose sessions run on the host
// in dir against the stub.
func served(t *testing.T, st session.Store, stub *luxstub.Server, model models.Model) *Runner {
	t.Helper()
	base := t.TempDir()
	if model == nil {
		model = &dialect.Model{}
	}
	r, err := New(Options{
		Store: st, ID: "run_serve", Kind: KindServe, Clock: func() time.Time { return t0 },
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			work := filepath.Join(base, s.ID)
			if err := os.MkdirAll(work, 0o755); err != nil {
				return harness.Config{}, err
			}
			m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill", s.ID), Environ: []string{"PATH=" + os.Getenv("PATH")}})
			if err != nil {
				return harness.Config{}, err
			}
			return harness.Config{
				Model: model, Connection: models.Connection{BaseURL: stub.URL() + "/anthropic", Model: "builder-model", Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: tools.NewRegistry(),
				Clock: func() time.Time { return t0 }, Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never happened", what)
		}
	}
}

// TestServeDrivesTheSessionsItClaims: a served runner claims each hosted
// session with a message, runs its turn, and stops when its context
// ends; a claimer that fails is reported and retried.
func TestServeDrivesTheSessionsItClaims(t *testing.T) {
	st := session.NewMemoryStore()
	stub := luxstub.New(t)
	stub.Script("builder-model",
		luxstub.Reply{Response: ir.Response{Model: "builder-model", Blocks: []ir.Block{{Type: ir.BlockText, Text: "one"}}, StopReason: ir.StopEndTurn}},
		luxstub.Reply{Response: ir.Response{Model: "builder-model", Blocks: []ir.Block{{Type: ir.BlockText, Text: "two"}}, StopReason: ir.StopEndTurn}},
	)
	a, b := hosted(t, st, "First."), hosted(t, st, "Second.")
	r := served(t, st, stub, nil)
	q := NewQueue(st, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, q, 2, func(id string, err error) { t.Errorf("%s: %v", id, err) }) }()
	for _, s := range []session.Session{a, b} {
		waitFor(t, s.ID+" answered", func() bool {
			got, err := st.Get(t.Context(), s.ID)
			return err == nil && got.Status == session.StatusIdle && got.StopReason == session.StopEndTurn
		})
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	f := &failingClaimer{}
	ctx, cancel = context.WithCancel(t.Context())
	var reported []error
	var mu sync.Mutex
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := r.Serve(ctx, f, 1, func(_ string, err error) { mu.Lock(); reported = append(reported, err); mu.Unlock() }); err != nil {
		t.Fatal(err)
	}
	if len(reported) == 0 {
		t.Fatal("a failing claim was not reported")
	}
	if err := r.Serve(t.Context(), f, 0, nil); err == nil {
		t.Fatal("served with no capacity")
	}
}

// blocking is a model whose request waits until its context ends.
type blocking struct{ started chan struct{} }

func (b *blocking) Stream(ctx context.Context, _ models.Request) (models.Stream, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestAServedDriveLeavesItsSessionToTheNextRunner: a server stopping in
// the middle of a turn appends nothing more and releases the lease, so
// the session is still running and the next claim resumes it.
func TestAServedDriveLeavesItsSessionToTheNextRunner(t *testing.T) {
	st := session.NewMemoryStore()
	s := hosted(t, st, "Go.")
	m := &blocking{started: make(chan struct{})}
	r := served(t, st, luxstub.New(t), m)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, NewQueue(st, 10*time.Millisecond), 1, nil) }()
	<-m.started
	before, err := st.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := st.Get(t.Context(), s.ID)
	if err != nil || after.Status != session.StatusRunning || after.LastSeq != before.LastSeq {
		t.Fatalf("after the stop %+v (last %d before), %v", after, before.LastSeq, err)
	}
	claims, err := NewQueue(st, time.Hour).Claim(t.Context(), session.Holder{Runner: "run_next"}, 1, 0)
	if err != nil || len(claims) != 1 {
		t.Fatalf("the next runner's claim: %v, %v", ids(claims), err)
	}
	if err := release(claims); err != nil {
		t.Fatal(err)
	}
}

// TestALostLeaseStopsTheDrive: once the lease is gone the turn stops and
// the log refuses every append.
func TestALostLeaseStopsTheDrive(t *testing.T) {
	st := session.NewMemoryStore()
	s := hosted(t, st, "Go.")
	m := &blocking{started: make(chan struct{})}
	r := served(t, st, luxstub.New(t), m)
	lease, err := st.Acquire(t.Context(), s.ID, session.Holder{Runner: "run_serve"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.drive(t.Context(), s.ID, lease, true)
		done <- err
	}()
	<-m.started
	before, err := st.Get(t.Context(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("a drive past its lease reported nothing")
	}
	if after, err := st.Get(t.Context(), s.ID); err != nil || after.LastSeq != before.LastSeq {
		t.Fatalf("the drive appended after its lease: %d, then %d, %v", before.LastSeq, after.LastSeq, err)
	}
	log := NewLog(st, s.ID, before.LastSeq)
	closed := make(chan struct{})
	close(closed)
	log.lost = closed
	if _, err := log.Append(t.Context(), nil); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("an append on a fenced log: %v", err)
	}
}

// TestASetupFailureClosesTheTurn: a session whose harness cannot be had
// ends its turn idle with a session.error naming the code, so the queue
// does not claim it again until a new message arrives.
func TestASetupFailureClosesTheTurn(t *testing.T) {
	st := session.NewMemoryStore()
	s := hosted(t, st, "Go.")
	r, err := New(Options{Store: st, ID: "run_serve", Clock: func() time.Time { return t0 },
		Harness: func(context.Context, session.Session) (harness.Config, error) {
			return harness.Config{}, &SetupError{Code: "machine_unavailable", Err: errors.New("no Cella")}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Drive(t.Context(), s.ID); err == nil || (&SetupError{Code: "c", Err: errors.New("e")}).Error() != "c: e" {
		t.Fatalf("Drive: %v", err)
	}
	got, err := st.Get(t.Context(), s.ID)
	if err != nil || got.Status != session.StatusIdle || got.StopReason != session.StopError {
		t.Fatalf("after a setup failure %+v, %v", got, err)
	}
	evs, err := st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var e session.SessionError
	for _, ev := range evs {
		if ev.Type == session.TypeSessionError {
			if err := ev.Decode(&e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if e.Code != "machine_unavailable" {
		t.Fatalf("session.error %+v", e)
	}
	if claims, err := NewQueue(st, time.Hour).Claim(t.Context(), session.Holder{Runner: "run_b"}, 1, 0); err != nil || len(claims) != 0 {
		t.Fatalf("a failed session was claimed again: %v, %v", ids(claims), err)
	}
	r.o.Harness = func(context.Context, session.Session) (harness.Config, error) {
		return harness.Config{}, &models.Coded{Code: models.CodeUnknown, Message: "no such model"}
	}
	appendTo(t, st, s.ID, session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "Again."}}})
	if _, err := r.Drive(t.Context(), s.ID); err == nil {
		t.Fatal("a second failure reported nothing")
	}
	r.o.Harness = func(context.Context, session.Session) (harness.Config, error) {
		return harness.Config{}, errors.New("plain")
	}
	appendTo(t, st, s.ID, session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "Again."}}})
	if _, err := r.Drive(t.Context(), s.ID); err == nil {
		t.Fatal("a third failure reported nothing")
	}
	evs, err = st.Events(t.Context(), s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, ev := range evs {
		if ev.Type == session.TypeSessionError {
			if err := ev.Decode(&e); err != nil {
				t.Fatal(err)
			}
			codes = append(codes, e.Code)
		}
	}
	if !slices.Equal(codes, []string{"machine_unavailable", models.CodeUnknown, CodeSetupFailed}) {
		t.Fatalf("codes %v", codes)
	}
}
