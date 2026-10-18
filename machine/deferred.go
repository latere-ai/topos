// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"
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

// Starter is the optional interface of a machine opened on demand that
// can begin its open ahead of the operation that needs it (spec 048):
// Start returns at once, and the open runs beside whatever the caller
// does next. The harness starts the machine when the model's response
// begins a call of a tool that acts on it, so the machine comes up while
// the call's arguments stream.
type Starter interface {
	Start()
}

// Start begins m's open in the background when m is opened on demand;
// any other machine exists already, and nothing is done.
func Start(m Machine) {
	if s, ok := m.(Starter); ok {
		s.Start()
	}
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
	// starting is closed once the open Start began has finished, and is
	// nil while none was begun; startErr is that open's failure, which
	// the next operation answers, and prepared the machine it opened,
	// which the next operation records through the hook and publishes.
	starting chan struct{}
	startErr error
	prepared Machine
	// held is the refusal or the wait of a person's computer the last open
	// answered (spec 080), which every later open answers without asking
	// the computer again, until AskAgain.
	held error
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

// Start opens the machine in the background, unless it is open, released
// or already starting, so the open runs beside other work: the harness
// starts a session's machine when the model's response begins a call of
// a tool that acts on it, beside the call's streaming arguments (specs
// 046 and 048). The hook does not run: the first operation that needs
// the machine runs it on the machine Start opened, and waits for an open
// still under way, so the session records its machine where it does
// without a start. A failed open is answered to that first operation, in
// the place of the open it would have made, and the next one tries
// again; a turn that never needs the machine never hears it.
func (d *Deferred) Start() {
	d.mu.Lock()
	if d.m != nil || d.prepared != nil || d.released || d.held != nil || inFlight(d.starting) {
		d.mu.Unlock()
		return
	}
	done := make(chan struct{})
	d.starting = done
	d.mu.Unlock()
	go func() {
		defer close(done)
		m, err := d.prepare()
		d.mu.Lock()
		d.startErr, d.prepared = err, m
		d.mu.Unlock()
	}()
}

// prepare opens the machine for Start, without the hook.
func (d *Deferred) prepare() (Machine, error) {
	d.opening.Lock()
	defer d.opening.Unlock()
	if m, err := d.current(); m != nil || err != nil {
		return nil, err
	}
	return d.openCoded()
}

// openCoded opens the machine, a failure as an OpenError. A person's
// computer that refused the session, or that the session waits for, is
// answered again without a second open until AskAgain: an open would ask
// its owner again, or wait for it again, once for each call.
func (d *Deferred) openCoded() (Machine, error) {
	d.mu.Lock()
	held := d.held
	d.mu.Unlock()
	if held != nil {
		return nil, held
	}
	m, err := d.open(d.ctx)
	if err == nil {
		return m, nil
	}
	if _, coded := errors.AsType[*OpenError](err); !coded {
		err = &OpenError{Code: CodeUnavailable, Err: err}
	}
	if holds(err) {
		d.mu.Lock()
		d.held = err
		d.mu.Unlock()
	}
	return nil, err
}

// holds reports whether an open's failure is one a later open answers
// without opening: a person's computer that refused the session, or that
// the session waits for (spec 080).
func holds(err error) bool {
	_, refused := errors.AsType[*Refused](err)
	_, waiting := errors.AsType[*Waiting](err)
	return refused || waiting
}

// AskAgain lets the next operation open the machine anew (spec 080): it
// forgets the refusal or the wait an open answered, and lets go of an
// open machine that is absent, so a turn that starts after its owner
// refused an earlier one, or after its computer came back, asks the
// computer again rather than answering what the last turn met. A runner's
// turn calls it as it starts.
func (d *Deferred) AskAgain() {
	d.opening.Lock()
	defer d.opening.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held = nil
	if holds(d.startErr) {
		d.startErr = nil
	}
	if d.m != nil && AbsentOf(d.m) != nil {
		d.m = nil
	}
}

// inFlight reports whether a start's channel is open.
func inFlight(starting chan struct{}) bool {
	if starting == nil {
		return false
	}
	select {
	case <-starting:
		return false
	default:
		return true
	}
}

// settle waits for the open Start began, if one runs, and answers its
// failure once.
func (d *Deferred) settle() error {
	d.mu.Lock()
	starting := d.starting
	d.mu.Unlock()
	if starting == nil {
		return nil
	}
	<-starting
	d.mu.Lock()
	defer d.mu.Unlock()
	err := d.startErr
	d.startErr = nil
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
// time, after the open Start began has finished; that open's failure is
// the answer of the first call after it.
func (d *Deferred) machine() (Machine, error) {
	if err := d.settle(); err != nil {
		return nil, err
	}
	return d.ensure()
}

// ensure is the open machine, opening it and running the hook the first
// time. The machine is published only once the hook returned, so a
// parallel call never acts on it before the session records it.
func (d *Deferred) ensure() (Machine, error) {
	if m, err := d.current(); m != nil || err != nil {
		return m, err
	}
	d.opening.Lock()
	defer d.opening.Unlock()
	if m, err := d.current(); m != nil || err != nil {
		return m, err
	}
	d.mu.Lock()
	m := d.prepared
	d.prepared = nil
	d.mu.Unlock()
	if m == nil {
		var err error
		if m, err = d.openCoded(); err != nil {
			return nil, err
		}
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

// Release releases the open machine, and the one Start opened that no
// operation took. An idle release does not wait for an open Start began:
// that open goes on under the drive's context, and a machine it leaves
// behind is the session's to find by name and stops by its own idle
// rule. A release at the session's end waits for it, so the end removes
// what the start made. A machine that never opened has nothing to let go
// of; after Release(ctx, true) every operation answers ErrReleased
// either way.
func (d *Deferred) Release(ctx context.Context, end bool) error {
	d.mu.Lock()
	starting := d.starting
	d.mu.Unlock()
	if end && starting != nil {
		<-starting
	}
	d.mu.Lock()
	m, prepared := d.m, d.prepared
	d.mu.Unlock()
	var err error
	if m != nil {
		err = m.Release(ctx, end)
	}
	if prepared != nil {
		err = errors.Join(err, prepared.Release(ctx, end))
	}
	if end && err == nil {
		d.mu.Lock()
		d.released, d.prepared = true, nil
		d.mu.Unlock()
	}
	return err
}

// ApplyNetwork applies the network to the open machine when it is
// networked, and to one a Start opened that no operation took yet, after
// an open Start began has finished, since that machine was opened with
// the network as it was then. A machine not opened yet has nothing to
// apply it to: its opener reads the session's network when it opens.
func (d *Deferred) ApplyNetwork(ctx context.Context, n Network) error {
	m, err := d.settled(ctx)
	if err != nil {
		return err
	}
	if nm, ok := m.(Networked); ok {
		return nm.ApplyNetwork(ctx, n)
	}
	return nil
}

// settled is the open machine, or the one a Start opened that no
// operation took yet, once an open Start began has finished; nil when
// neither is.
func (d *Deferred) settled(ctx context.Context) (Machine, error) {
	d.mu.Lock()
	starting := d.starting
	d.mu.Unlock()
	if inFlight(starting) {
		select {
		case <-starting:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.m != nil {
		return d.m, nil
	}
	return d.prepared, nil
}

// ApplyConnections gives the open machine the session's connections
// when it is connected, as ApplyNetwork gives it the network: to one a
// Start opened that no operation took yet, after an open Start began has
// finished. A machine not opened yet has nothing to apply them to: its
// opener reads the session's connections when it opens (spec 069).
func (d *Deferred) ApplyConnections(ctx context.Context, cs []HTTPConnection) error {
	m, err := d.settled(ctx)
	if err != nil {
		return err
	}
	if cm, ok := m.(Connected); ok {
		return cm.ApplyConnections(ctx, cs)
	}
	return nil
}

// ReachesConnections reports whether the open machine, or the one a
// Start opened that no operation took yet, reaches the connection
// service. It never opens the machine and never waits for an open: one
// not opened yet, or open and not connected, is taken to reach it, since
// only an open machine can say it does not, and an opener mounts the
// service's Secret whenever the sandbox can hold it.
func (d *Deferred) ReachesConnections() bool {
	d.mu.Lock()
	m := d.m
	if m == nil {
		m = d.prepared
	}
	d.mu.Unlock()
	cm, ok := m.(Connected)
	return !ok || cm.ReachesConnections()
}

// Absent is the open machine's, nil before it opens: a machine that
// cannot be opened says why itself, through the open's error (spec 080).
func (d *Deferred) Absent() *Waiting {
	if m := d.Opened(); m != nil {
		return AbsentOf(m)
	}
	return nil
}

// Refused are the open machine's refused connections, none before it
// opens or on a machine that is not networked.
func (d *Deferred) Refused(ctx context.Context, since time.Time) ([]Connection, error) {
	if nm, ok := d.Opened().(Networked); ok {
		return nm.Refused(ctx, since)
	}
	return nil, nil
}

var (
	_ Machine   = (*Deferred)(nil)
	_ Networked = (*Deferred)(nil)
	_ Connected = (*Deferred)(nil)
	_ Opener    = (*Deferred)(nil)
	_ Starter   = (*Deferred)(nil)
	_ Fetcher   = (*Deferred)(nil)
	_ Absent    = (*Deferred)(nil)
	_ AsksAgain = (*Deferred)(nil)
)
