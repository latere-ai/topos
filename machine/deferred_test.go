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
