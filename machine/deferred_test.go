// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fake is a machine that records the operations it served.
type fake struct {
	mu       sync.Mutex
	ops      []string
	released []bool
}

func (f *fake) did(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
}

func (f *fake) Info() Info       { return Info{Kind: KindCella, ID: "sbx_1", Workdir: "/work"} }
func (f *fake) Roots() []string  { return []string{"/work"} }
func (f *fake) SpillDir() string { return "/spill" }
func (f *fake) Exec(context.Context, ExecRequest) (ExecResult, error) {
	f.did("exec")
	return ExecResult{Output: []byte("ok")}, nil
}
func (f *fake) ExecStream(context.Context, ExecRequest) (ExecStream, error) {
	f.did("stream")
	return nil, nil
}
func (f *fake) ReadFile(context.Context, string) (io.ReadCloser, error) {
	f.did("read")
	return io.NopCloser(strings.NewReader("body")), nil
}
func (f *fake) WriteFile(context.Context, string, io.Reader, fs.FileMode) error {
	f.did("write")
	return nil
}
func (f *fake) Stat(context.Context, string) (FileInfo, error) {
	f.did("stat")
	return FileInfo{Path: "/work/a"}, nil
}
func (f *fake) List(context.Context, string) ([]FileInfo, error) {
	f.did("list")
	return nil, nil
}
func (f *fake) Remove(context.Context, string) error {
	f.did("remove")
	return nil
}
func (f *fake) Rename(context.Context, string, string) error {
	f.did("rename")
	return nil
}
func (f *fake) Search(context.Context, SearchRequest) (SearchResult, error) {
	f.did("search")
	return SearchResult{}, nil
}
func (f *fake) Release(_ context.Context, end bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, end)
	return nil
}

// fetcher is a fake that fetches.
type fetcher struct{ *fake }

func (f fetcher) Fetch(context.Context, FetchRequest) (FetchResult, error) {
	f.did("fetch")
	return FetchResult{Status: 200}, nil
}

func TestADeferredMachineOpensOnFirstUse(t *testing.T) {
	ctx := t.Context()
	under := &fake{}
	var opens, hooks atomic.Int32
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		opens.Add(1)
		return fetcher{under}, nil
	})
	d.OnOpen(func(_ context.Context, m Machine) error {
		hooks.Add(1)
		if m.Info().ID != "sbx_1" {
			t.Errorf("the hook got %+v", m.Info())
		}
		return nil
	})
	if info := d.Info(); info.Kind != KindCella || info.ID != "" || d.Roots() != nil || d.SpillDir() != "" || d.Opened() != nil {
		t.Fatalf("before the first use: %+v %v %q", info, d.Roots(), d.SpillDir())
	}
	if err := d.Release(ctx, false); err != nil || opens.Load() != 0 {
		t.Fatalf("an idle release opened the machine: %v", err)
	}
	if err := Open(ctx, d); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := d.Exec(ctx, ExecRequest{Command: "true"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := d.ExecStream(ctx, ExecRequest{}); err != nil {
		t.Fatal(err)
	}
	rc, err := d.ReadFile(ctx, "/work/a")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{
		func() error { return d.WriteFile(ctx, "/work/a", strings.NewReader("x"), 0) },
		func() error { _, err := d.Stat(ctx, "/work/a"); return err },
		func() error { _, err := d.List(ctx, "/work"); return err },
		func() error { return d.Remove(ctx, "/work/a") },
		func() error { return d.Rename(ctx, "/work/a", "/work/b") },
		func() error { _, err := d.Search(ctx, SearchRequest{}); return err },
		func() error { _, err := d.Fetch(ctx, FetchRequest{URL: "https://example.com"}); return err },
	} {
		if err := op(); err != nil {
			t.Fatal(err)
		}
	}
	if opens.Load() != 1 || hooks.Load() != 1 {
		t.Fatalf("%d opens and %d hooks, want one each", opens.Load(), hooks.Load())
	}
	if info := d.Info(); info.ID != "sbx_1" || d.SpillDir() != "/spill" || len(d.Roots()) != 1 {
		t.Fatalf("after the open: %+v", info)
	}
	if err := d.Release(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(under.released) != 1 || !under.released[0] {
		t.Fatalf("releases %v", under.released)
	}
	if _, err := d.Exec(ctx, ExecRequest{}); !errors.Is(err, ErrReleased) {
		t.Fatalf("after the end: %v", err)
	}
}

func TestADeferredMachineThatNeverOpens(t *testing.T) {
	ctx := t.Context()
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		t.Fatal("opened a machine nothing acted on")
		return nil, nil
	})
	for _, end := range []bool{false, true} {
		if err := d.Release(ctx, end); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Open(ctx); !errors.Is(err, ErrReleased) {
		t.Fatalf("an open after the end: %v", err)
	}
}

func TestADeferredMachineThatCannotOpen(t *testing.T) {
	ctx := t.Context()
	cause := errors.New("no sandbox")
	tries := 0
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		tries++
		if tries == 2 {
			return nil, &OpenError{Code: "budget", Err: cause}
		}
		return nil, cause
	})
	err := d.Open(ctx)
	oe, ok := errors.AsType[*OpenError](err)
	if !ok || oe.Code != CodeUnavailable || !errors.Is(err, cause) || !strings.Contains(err.Error(), "no sandbox") {
		t.Fatalf("the first open: %v", err)
	}
	if oe, ok := errors.AsType[*OpenError](d.Open(ctx)); !ok || oe.Code != "budget" {
		t.Fatalf("a coded failure keeps its code: %v", oe)
	}
	if d.Opened() != nil || tries != 2 {
		t.Fatalf("%d tries", tries)
	}
}

func TestADeferredMachinesFailedHookLeavesItOpen(t *testing.T) {
	ctx := t.Context()
	under := &fake{}
	failed := errors.New("the repository could not be cloned")
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) { return under, nil })
	d.OnOpen(func(context.Context, Machine) error { return failed })
	if _, err := d.Exec(ctx, ExecRequest{}); !errors.Is(err, failed) {
		t.Fatalf("the first call: %v", err)
	}
	if _, err := d.Exec(ctx, ExecRequest{}); err != nil {
		t.Fatalf("the machine did not stay open: %v", err)
	}
	if _, err := d.Fetch(ctx, FetchRequest{}); !errors.Is(err, errNoFetch) {
		t.Fatalf("a machine that cannot fetch: %v", err)
	}
	if d.Opened() != under {
		t.Fatal("not the machine it opened")
	}
}

func TestOpenLeavesAnyOtherMachineAlone(t *testing.T) {
	under := &fake{}
	if err := Open(t.Context(), under); err != nil || len(under.ops) != 0 {
		t.Fatalf("%v %v", err, under.ops)
	}
}

// TestStartBeginsADeferredOpenAndLeavesAnyOtherMachineAlone: Start begins
// a deferred machine's open without waiting for it, and does nothing to a
// machine that exists already, or to none.
func TestStartBeginsADeferredOpenAndLeavesAnyOtherMachineAlone(t *testing.T) {
	under := &fake{}
	Start(under)
	Start(nil)
	if len(under.ops) != 0 {
		t.Fatalf("a machine that exists was acted on: %v", under.ops)
	}
	began, gate := make(chan struct{}), make(chan struct{})
	d := Defer(t.Context(), KindCella, func(context.Context) (Machine, error) {
		close(began)
		<-gate
		return under, nil
	})
	Start(d)
	select {
	case <-began:
	case <-time.After(5 * time.Second):
		t.Fatal("the deferred machine's open did not begin")
	}
	close(gate)
	if err := d.Open(t.Context()); err != nil || d.Opened() != under {
		t.Fatalf("the open Start began: %v", err)
	}
}

// TestAStartedMachineOpensInTheBackground: Start opens the machine
// without a caller waiting and without the hook; an operation that comes
// while the open runs waits for it, runs the hook on the machine Start
// opened and acts on it; a second Start opens nothing more.
func TestAStartedMachineOpensInTheBackground(t *testing.T) {
	ctx := t.Context()
	under := &fake{}
	gate := make(chan struct{})
	var opens, hooks atomic.Int32
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		opens.Add(1)
		<-gate
		return under, nil
	})
	d.OnOpen(func(context.Context, Machine) error { hooks.Add(1); return nil })
	d.Start()
	d.Start()
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec(ctx, ExecRequest{Command: "true"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the call did not wait for the open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if hooks.Load() != 0 || d.Opened() != nil {
		t.Fatal("the start ran the hook or published the machine")
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	d.Start()
	if err := d.Release(ctx, false); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 || hooks.Load() != 1 || len(under.ops) != 1 || d.Opened() != under {
		t.Fatalf("%d opens, %d hooks, ops %v", opens.Load(), hooks.Load(), under.ops)
	}
}

// TestAStartThatFailsAnswersTheNextCall: a start whose open fails
// answers the first operation after it with that failure, in the place
// of the open it would have made; the next operation tries again, and so
// does a later Start. A hook that fails on a started machine answers the
// operation that ran it, and leaves the machine open.
func TestAStartThatFailsAnswersTheNextCall(t *testing.T) {
	ctx := t.Context()
	var opens atomic.Int32
	fail := errors.New("no capacity")
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		if opens.Add(1) == 1 {
			return nil, fail
		}
		return &fake{}, nil
	})
	d.Start()
	if oe, ok := errors.AsType[*OpenError](Open(ctx, d)); !ok || !errors.Is(oe, fail) || oe.Code != CodeUnavailable {
		t.Fatalf("the first call after a failed start: %v", oe)
	}
	if opens.Load() != 1 {
		t.Fatalf("the first call opened again: %d opens", opens.Load())
	}
	if err := Open(ctx, d); err != nil || opens.Load() != 2 {
		t.Fatalf("the next call: %v, %d opens", err, opens.Load())
	}

	hookErr := &OpenError{Code: "repository_unavailable", Err: errors.New("clone refused")}
	h := Defer(ctx, KindCella, func(context.Context) (Machine, error) { return &fake{}, nil })
	h.OnOpen(func(context.Context, Machine) error { return hookErr })
	h.Start()
	if _, err := h.Exec(ctx, ExecRequest{}); !errors.Is(err, hookErr) {
		t.Fatalf("the first call on a started machine whose hook fails: %v", err)
	}
	if _, err := h.Exec(ctx, ExecRequest{}); err != nil {
		t.Fatalf("the machine a failed hook left open: %v", err)
	}

	var again atomic.Int32
	r := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		if again.Add(1) == 1 {
			return nil, fail
		}
		return &fake{}, nil
	})
	r.Start()
	if err := Open(ctx, r); !errors.Is(err, fail) {
		t.Fatalf("the first call: %v", err)
	}
	r.Start()
	if _, err := r.Exec(ctx, ExecRequest{}); err != nil || again.Load() != 2 {
		t.Fatalf("a start after a failed one: %v, %d opens", err, again.Load())
	}
}

// TestAReleaseAndAStartedOpen: an idle release does not wait for an
// open a start began, and a release at the session's end waits for it,
// then removes the machine it made, which never ran the hook.
func TestAReleaseAndAStartedOpen(t *testing.T) {
	ctx := t.Context()
	under := &fake{}
	gate := make(chan struct{})
	var hooks atomic.Int32
	d := Defer(ctx, KindCella, func(context.Context) (Machine, error) {
		<-gate
		return under, nil
	})
	d.OnOpen(func(context.Context, Machine) error { hooks.Add(1); return nil })
	d.Start()
	if err := d.Release(ctx, false); err != nil {
		t.Fatalf("an idle release: %v", err)
	}
	released := make(chan error, 1)
	go func() { released <- d.Release(ctx, true) }()
	select {
	case err := <-released:
		t.Fatalf("the end did not wait for the open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if len(under.released) != 1 || !under.released[0] || hooks.Load() != 0 {
		t.Fatalf("released %v, %d hooks", under.released, hooks.Load())
	}
	d.Start()
	if _, err := d.Exec(ctx, ExecRequest{}); !errors.Is(err, ErrReleased) {
		t.Fatalf("a call after the end: %v", err)
	}
}
