// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
)

// Opener is the optional interface of a machine opened on demand (spec
// 009): Open makes it exist, and is a no-op once it does. The harness
// opens it before a call of a tool that acts on the machine, so a
// session whose agent never runs a command or touches a file never has
// one.
type Opener interface {
	Open(ctx context.Context) error
}

// Open opens m when it is opened on demand; any other machine exists
// already.
func Open(ctx context.Context, m Machine) error {
	if o, ok := m.(Opener); ok {
		return o.Open(ctx)
	}
	return nil
}

// OpenError is a machine opened on demand that could not be had. Code is
// the error code the session's session.error carries: machine_unavailable
// for the machine itself (spec 009), or the code of what failed right
// after it opened, such as repository_unavailable (spec 019).
type OpenError struct {
	Code string
	Err  error
}

func (e *OpenError) Error() string { return e.Code + ": " + e.Err.Error() }

func (e *OpenError) Unwrap() error { return e.Err }

// errNoFetch is a fetch on a deferred machine whose machine cannot fetch.
var errNoFetch = errors.New("machine: this machine does not fetch")

// Deferred is a session's machine that exists only once something acts
// on it. Info, Roots and SpillDir answer without opening it, Info with
// the kind alone until then; every other operation opens it first. The
// open and the hook OnOpen sets run under the context Defer was given,
// the drive's, rather than the call's that asked for them, so a call
// canceled mid-open leaves neither a half-made sandbox nor a half-done
// hook; the call waits for both.
type Deferred struct {
	ctx  context.Context
	kind string
	open func(ctx context.Context) (Machine, error)

	// opening serializes the open and its hook, so parallel calls wait
	// for the machine the first one opens.
	opening sync.Mutex
	hook    func(ctx context.Context, m Machine) error

	mu       sync.Mutex
	m        Machine
	released bool
}

// Defer is a machine of kind that open makes on first use, under ctx.
func Defer(ctx context.Context, kind string, open func(ctx context.Context) (Machine, error)) *Deferred {
	return &Deferred{ctx: ctx, kind: kind, open: open}
}

// OnOpen sets the hook that runs once, right after the machine opens and
// before the operation that opened it: the runner records the machine in
// the session there.
func (d *Deferred) OnOpen(hook func(ctx context.Context, m Machine) error) {
	d.opening.Lock()
	defer d.opening.Unlock()
	d.hook = hook
}

// Open opens the machine unless it is open. A failed open is an
// OpenError, machine_unavailable unless the opener named a code of its
// own, and a later call tries again. A failed hook is returned to the
// call that opened the machine, which stays open: what the hook could
// not do is the session's to see, not a reason to make another machine.
func (d *Deferred) Open(ctx context.Context) error {
	_, err := d.machine()
	return err
}

// Opened is the open machine, nil until the first open.
func (d *Deferred) Opened() Machine {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m
}

// current is the open machine, or ErrReleased after Release(ctx, true).
func (d *Deferred) current() (Machine, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.released {
		return nil, ErrReleased
	}
	return d.m, nil
}

// machine is the open machine, opening it and running the hook the first
// time. The machine is published only once the hook returned, so a
// parallel call never acts on it before the session records it.
func (d *Deferred) machine() (Machine, error) {
	if m, err := d.current(); m != nil || err != nil {
		return m, err
	}
	d.opening.Lock()
	defer d.opening.Unlock()
	if m, err := d.current(); m != nil || err != nil {
		return m, err
	}
	m, err := d.open(d.ctx)
	if err != nil {
		if _, coded := errors.AsType[*OpenError](err); coded {
			return nil, err
		}
		return nil, &OpenError{Code: CodeUnavailable, Err: err}
	}
	var herr error
	if d.hook != nil {
		herr = d.hook(d.ctx, m)
	}
	d.mu.Lock()
	d.m = m
	d.mu.Unlock()
	if herr != nil {
		return nil, herr
	}
	return m, nil
}

// Info is the open machine's, or the kind alone before it opens.
func (d *Deferred) Info() Info {
	if m := d.Opened(); m != nil {
		return m.Info()
	}
	return Info{Kind: d.kind}
}

// Roots are the open machine's, none before it opens.
func (d *Deferred) Roots() []string {
	if m := d.Opened(); m != nil {
		return m.Roots()
	}
	return nil
}

// SpillDir is the open machine's, empty before it opens; a tool that
// spills opens the machine first.
func (d *Deferred) SpillDir() string {
	if m := d.Opened(); m != nil {
		return m.SpillDir()
	}
	return ""
}

func (d *Deferred) Exec(ctx context.Context, r ExecRequest) (ExecResult, error) {
	m, err := d.machine()
	if err != nil {
		return ExecResult{}, err
	}
	return m.Exec(ctx, r)
}

func (d *Deferred) ExecStream(ctx context.Context, r ExecRequest) (ExecStream, error) {
	m, err := d.machine()
	if err != nil {
		return nil, err
	}
	return m.ExecStream(ctx, r)
}

func (d *Deferred) ReadFile(ctx context.Context, path string) (io.ReadCloser, error) {
	m, err := d.machine()
	if err != nil {
		return nil, err
	}
	return m.ReadFile(ctx, path)
}

func (d *Deferred) WriteFile(ctx context.Context, path string, r io.Reader, mode fs.FileMode) error {
	m, err := d.machine()
	if err != nil {
		return err
	}
	return m.WriteFile(ctx, path, r, mode)
}

func (d *Deferred) Stat(ctx context.Context, path string) (FileInfo, error) {
	m, err := d.machine()
	if err != nil {
		return FileInfo{}, err
	}
	return m.Stat(ctx, path)
}

func (d *Deferred) List(ctx context.Context, path string) ([]FileInfo, error) {
	m, err := d.machine()
	if err != nil {
		return nil, err
	}
	return m.List(ctx, path)
}

func (d *Deferred) Remove(ctx context.Context, path string) error {
	m, err := d.machine()
	if err != nil {
		return err
	}
	return m.Remove(ctx, path)
}

func (d *Deferred) Rename(ctx context.Context, from, to string) error {
	m, err := d.machine()
	if err != nil {
		return err
	}
	return m.Rename(ctx, from, to)
}

func (d *Deferred) Search(ctx context.Context, q SearchRequest) (SearchResult, error) {
	m, err := d.machine()
	if err != nil {
		return SearchResult{}, err
	}
	return m.Search(ctx, q)
}

// Fetch fetches through the open machine, so a fetch on a Cella machine
// still runs in its sandbox.
func (d *Deferred) Fetch(ctx context.Context, r FetchRequest) (FetchResult, error) {
	m, err := d.machine()
	if err != nil {
		return FetchResult{}, err
	}
	f, ok := m.(Fetcher)
	if !ok {
		return FetchResult{}, errNoFetch
	}
	return f.Fetch(ctx, r)
}

// Release releases the open machine. A machine that never opened has
// nothing to let go of; after Release(ctx, true) every operation answers
// ErrReleased either way.
func (d *Deferred) Release(ctx context.Context, end bool) error {
	d.mu.Lock()
	m := d.m
	d.mu.Unlock()
	var err error
	if m != nil {
		err = m.Release(ctx, end)
	}
	if end && err == nil {
		d.mu.Lock()
		d.released = true
		d.mu.Unlock()
	}
	return err
}

var (
	_ Machine = (*Deferred)(nil)
	_ Opener  = (*Deferred)(nil)
	_ Fetcher = (*Deferred)(nil)
)
