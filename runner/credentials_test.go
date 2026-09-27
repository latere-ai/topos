// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/session"
)

// minted is a Credentials that answers a new value on every call, each
// living life, and counts the calls.
type minted struct {
	mu    sync.Mutex
	calls int
	life  time.Duration
	now   func() time.Time
	err   error
}

func (m *minted) Credential(_ context.Context, audience, workload string) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return Credential{}, m.err
	}
	m.calls++
	return Credential{Value: fmt.Sprintf("%s-%s-%d", audience, workload, m.calls), ExpiresAt: m.now().Add(m.life)}, nil
}

func (m *minted) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// TestTokenSourceCachesAndDrops: an answer is held per audience and
// workload until two minutes before its expiry, a Drop forgets every one,
// an answer asked for before a Drop is not held past it, a failure is
// not held, and a lost lease answers nothing.
func TestTokenSourceCachesAndDrops(t *testing.T) {
	now := t0
	clock := func() time.Time { return now }
	m := &minted{life: 15 * time.Minute, now: clock}
	lost := make(chan struct{})
	src := NewTokenSource(m, lost, clock)
	ctx := t.Context()
	first, err := src.Token(ctx, "cella", WorkloadSession)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := src.Token(ctx, "cella", WorkloadSession); again != first || m.count() != 1 {
		t.Fatalf("a held token was asked again: %+v, %d calls", again, m.count())
	}
	if other, _ := src.Token(ctx, "cella", WorkloadSandbox); other == first || m.count() != 2 {
		t.Fatalf("the sandbox's token is the runner's: %+v", other)
	}
	now = now.Add(13*time.Minute + time.Second)
	if later, _ := src.Token(ctx, "cella", WorkloadSession); later == first || m.count() != 3 {
		t.Fatalf("a token within two minutes of expiry was kept: %+v", later)
	}
	src.Drop()
	if _, err := src.Token(ctx, "cella", WorkloadSandbox); err != nil || m.count() != 4 {
		t.Fatalf("a dropped token was kept: %d calls, %v", m.count(), err)
	}
	m.err = ErrNotMinted
	if _, err := src.Token(ctx, "origo", WorkloadSession); !errors.Is(err, ErrNotMinted) {
		t.Fatalf("a failure: %v", err)
	}
	m.err = nil
	close(lost)
	if _, err := src.Token(ctx, "cella", WorkloadSandbox); !errors.Is(err, ErrLeaseLost) || m.count() != 4 {
		t.Fatalf("a lost lease answered: %d calls, %v", m.count(), err)
	}
	if NewTokenSource(m, nil, nil).now == nil {
		t.Fatal("no clock")
	}
	if TokensFrom(t.Context()) != nil {
		t.Fatal("a bare context carries a source")
	}
}

// scoper is a tool that appends a session.scope_changed through the
// store and answers once the drive's source has dropped what it held.
type scoper struct {
	st    session.Store
	id    string
	src   func() *TokenSource
	calls func() int
}

func (scoper) Definition() tools.Definition {
	return tools.Definition{Name: "narrow", Description: "narrows the scope", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (scoper) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }
func (sc scoper) Run(ctx context.Context, _ tools.Call) (tools.Result, error) {
	before := sc.calls()
	s, err := sc.st.Get(ctx, sc.id)
	if err != nil {
		return tools.Result{}, err
	}
	e, err := session.NewEvent(session.TypeScopeChanged, session.ScopeChanged{By: session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, Old: []json.RawMessage{}, New: []json.RawMessage{}}, t0)
	if err != nil {
		return tools.Result{}, err
	}
	evs := []session.Event{e}
	session.Stamp(sc.id, s.LastSeq, evs)
	if _, err := sc.st.Append(ctx, sc.id, s.LastSeq, evs); err != nil {
		return tools.Result{}, err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := sc.src().Token(ctx, "cella", WorkloadSession); err != nil {
			return tools.Result{}, err
		}
		if sc.calls() > before {
			return tools.Text(tools.OutcomeOK, "fresh"), nil
		}
	}
	return tools.Text(tools.OutcomeError, "the held token outlived the scope change"), nil
}

// TestScopeChangeTakesFreshToken: a drive's harness builder reads the
// token source the runner built from Options.Credentials, and the first
// credential asked after a session.scope_changed is a fresh one.
func TestScopeChangeTakesFreshToken(t *testing.T) {
	f := setup(t)
	m := &minted{life: 15 * time.Minute, now: time.Now}
	var got *TokenSource
	var mu sync.Mutex
	source := func() *TokenSource { mu.Lock(); defer mu.Unlock(); return got }
	f.r.o.Credentials = func(id string, lease session.Lease) Credentials {
		if id != f.s.ID || lease == nil {
			t.Errorf("credentials for %s, %v", id, lease)
		}
		return m
	}
	base := f.r.o.Harness
	f.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		mu.Lock()
		got = TokensFrom(ctx)
		mu.Unlock()
		if got == nil {
			return harness.Config{}, errors.New("no token source")
		}
		if _, err := got.Token(ctx, "cella", WorkloadSession); err != nil {
			return harness.Config{}, err
		}
		c, err := base(ctx, s)
		if err != nil {
			return c, err
		}
		return c, c.Tools.AddBuiltin(scoper{st: f.store, id: s.ID, src: source, calls: m.count})
	}
	ctx := t.Context()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_s", Name: "narrow", Args: json.RawMessage(`{}`)}}), reply(ir.Block{Type: ir.BlockText, Text: "done"}))
	f.message(ctx, "Go.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		var p session.ToolResult
		if e.Type == session.TypeToolResult && e.Decode(&p) == nil && p.Outcome != tools.OutcomeOK {
			t.Fatalf("the call after the scope change: %+v", p)
		}
	}
	if m.count() != 2 {
		t.Fatalf("%d credentials minted, want the first and one after the change", m.count())
	}
}
