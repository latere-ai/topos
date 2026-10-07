// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package runner drives sessions (spec 016): it holds a session's lease,
// appends session.status running, attaches the machine, runs turns
// through the harness, and lets go of the machine and the lease when the
// session is idle or ended.
package runner

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner/checkpoint"
	"latere.ai/x/topos/session"
)

// Runner kinds, recorded on session.status running.
const (
	KindLocal  = "local"
	KindServe  = "serve"
	KindRunner = "runner"
)

// Options configure a runner.
type Options struct {
	Store session.Store
	// Harness builds a session's harness configuration: its model, its
	// machine and its tools.
	Harness func(ctx context.Context, s session.Session) (harness.Config, error)
	// ID is unique per process.
	ID   string
	Kind string
	// PersonalInstructions and PersonalSkills are the person's own
	// instruction file and skills folder, read on the host.
	PersonalInstructions string
	PersonalSkills       string
	// CheckpointDir holds the session repositories of working
	// directories that are not checkouts (spec 034); empty takes no
	// checkpoints outside a repository.
	CheckpointDir string
	// CheckpointHost is the URL of the git host whose repositories keep
	// the checkpoints of the sessions that work in them past their
	// machines (spec 035): a session whose first repository's URL is
	// under it pushes each checkpoint there. Empty keeps every checkpoint
	// on its machine.
	CheckpointHost string
	// Credentials reaches the session's credentials for a lease that
	// does not reach them itself (spec 018): toposd's minter for the
	// leases of its own runners. Nil, and with such a lease, a drive
	// has no token source.
	Credentials func(id string, lease session.Lease) Credentials
	// Failover asks which model the session id's turn continues on when the
	// model it runs cannot serve now, or its provider rejected the request
	// (spec 051), standing, failed, reason and detail as
	// harness.Config.Failover says: toposd's question to its authorizer,
	// for the leases of its own runners. A lease that asks it itself
	// (Failover) is asked instead; with neither, a drive asks nothing, and
	// such a turn ends after harness.DownRetry or with the model's error.
	Failover func(ctx context.Context, id string, standing, failed session.ModelRef, reason, detail string) (session.ModelRef, error)
	// Apps reads the commits an app is served at from the installation's
	// app host, with the drive's token source, nil when the drive reaches
	// no session credentials (spec 058). Nil starts the checkout of every
	// app a session is attached at its repository's default branch.
	Apps  func(ctx context.Context, tokens *TokenSource, slug string) (AppCommits, error)
	Clock func() time.Time
}

// AppCommits are the commits an app is served at (spec 058): Live, the
// deploy at its public address, and Preview, its newest preview that is
// ready, each "" when it has none.
type AppCommits struct {
	Live    string
	Preview string
}

// Failover is a lease that asks which model its session's turn continues
// on when the model it runs cannot serve now, or its provider rejected the
// request (spec 051): a runner process's, which reaches toposd over its
// internal listener.
type Failover interface {
	Failover(ctx context.Context, standing, failed session.ModelRef, reason, detail string) (session.ModelRef, error)
}

// failover is the harness's Failover for a drive holding lease on the
// session id: the lease's own, Options.Failover's, or nil.
func (r *Runner) failover(id string, lease session.Lease) func(context.Context, session.ModelRef, session.ModelRef, string, string) (session.ModelRef, error) {
	if f, ok := lease.(Failover); ok {
		return f.Failover
	}
	if r.o.Failover == nil {
		return nil
	}
	return func(ctx context.Context, standing, failed session.ModelRef, reason, detail string) (session.ModelRef, error) {
		return r.o.Failover(ctx, id, standing, failed, reason, detail)
	}
}

// Runner drives sessions.
type Runner struct {
	o Options
}

// New checks the options.
func New(o Options) (*Runner, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("runner: no store")
	case o.Harness == nil:
		return nil, errors.New("runner: no harness configuration")
	case o.ID == "":
		return nil, errors.New("runner: no runner id")
	}
	if o.Kind == "" {
		o.Kind = KindLocal
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	return &Runner{o: o}, nil
}

// ErrEnded is a session that has ended.
var ErrEnded = errors.New("runner: the session has ended")

// ErrLeaseLost is an append after the runner's lease on the session
// ended: another runner may hold it now, so nothing more is written.
var ErrLeaseLost = errors.New("runner: the session's lease was lost")

// SetupError is a session a runner could not start a turn of: its
// harness configuration or its machine could not be had. Code is the
// error code the session's session.error carries.
type SetupError struct {
	Code string
	Err  error
}

func (e *SetupError) Error() string { return e.Code + ": " + e.Err.Error() }

func (e *SetupError) Unwrap() error { return e.Err }

// CodeSetupFailed is the code of a setup failure that names none.
const CodeSetupFailed = "runner_setup_failed"

// Drive runs one session in process until it is idle or ended: its
// lease is the store's own, held for the whole drive.
func (r *Runner) Drive(ctx context.Context, id string) (harness.Outcome, error) {
	lease, err := r.o.Store.Acquire(ctx, id, r.holder())
	if err != nil {
		return harness.Outcome{}, err
	}
	return r.drive(ctx, id, lease, false)
}

// holder is this runner as a lease names it.
func (r *Runner) holder() session.Holder {
	return session.Holder{Runner: r.o.ID, AcquiredAt: r.o.Clock()}
}

// drive runs a session whose lease the runner holds, and releases it.
// A lost lease cancels the turn and fences the log, so a runner that
// lost the session writes nothing more to it. A served drive also fences
// the log when its context ends: a server shutting down leaves the
// session running with its lease released, for the next runner to claim
// and resume, rather than closing the turn as interrupted. The end of a
// served drive's context therefore does not reach the turn directly: the
// fence closes first and then cancels the turn, so a harness that sees
// the cancel finds its closing append refused.
func (r *Runner) drive(ctx context.Context, id string, lease session.Lease, served bool) (out harness.Outcome, err error) {
	st := r.o.Store
	defer func() { err = errors.Join(err, lease.Release()) }()
	outer := ctx
	if served {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fence := make(chan struct{})
	go func() {
		defer cancel()
		stop := outer.Done()
		if !served {
			stop = nil
		}
		select {
		case <-lease.Lost():
		case <-stop:
		case <-ctx.Done():
			return
		}
		close(fence)
	}()
	s, err := st.Get(ctx, id)
	if err != nil {
		return harness.Outcome{}, err
	}
	if s.Status == session.StatusEnded {
		return harness.Outcome{}, fmt.Errorf("%w: %s", ErrEnded, id)
	}
	log := NewLog(st, id, s.LastSeq)
	log.lost = fence
	if f, ok := lease.(session.Fence); ok {
		log.fence = f
	}
	if err := r.running(ctx, log); err != nil {
		return harness.Outcome{}, err
	}
	// The harness builder reads the drive's token source from its
	// context; the fence above goes on reading the drive's own.
	built, tokens := r.tokens(ctx, id, lease)
	cfg, err := r.o.Harness(built, s)
	if err != nil {
		return harness.Outcome{}, r.setupFailed(ctx, log, err)
	}
	if cfg.Failover == nil {
		cfg.Failover = r.failover(id, lease)
	}
	evs, err := st.Events(ctx, id, 1, 0)
	if err != nil {
		return harness.Outcome{}, err
	}
	// A session's first machine gets its repositories (spec 019). A
	// machine opened on demand opens when a tool first acts on it and is
	// recorded then, beside the running turn, whether or not the session
	// had one before: its requests carry the context its log recorded,
	// so a turn that only talks starts no stopped sandbox, and no
	// ceiling on running sandboxes can refuse it (spec 048). The hook
	// records a sandbox that replaced the recorded one.
	first := !hasMachine(s, evs)
	deferred, onDemand := cfg.Machine.(*machine.Deferred)
	switch {
	case onDemand:
		deferred.OnOpen(func(ctx context.Context, m machine.Machine) error {
			return r.opened(ctx, s, m, log, first, true, tokens)
		})
	default:
		if err := r.opened(ctx, s, cfg.Machine, log, first, false, tokens); err != nil {
			return harness.Outcome{}, r.setupFailed(ctx, log, err)
		}
	}
	if evs, err = st.Events(ctx, id, 1, 0); err != nil {
		return harness.Outcome{}, err
	}
	git, memory := attached(evs)
	cfg.Prompt.Git = cfg.Prompt.Git || git || len(session.Repositories(s)) > 0
	cfg.Prompt.Memory = cfg.Prompt.Memory || memory
	if cfg.Checkpoint == nil {
		cp := r.checkpointer(cfg, s)
		cfg.Checkpoint = func(ctx context.Context, turn int, previous string) (*session.CheckpointRef, error) {
			// A machine not opened yet has no files to keep, and a
			// checkpoint must not open it.
			if onDemand && deferred.Opened() == nil {
				return nil, nil
			}
			previous, err := r.chainable(ctx, cp, s, evs, previous)
			if err != nil {
				return nil, err
			}
			ref, err := cp.Take(ctx, turn, previous)
			if errors.Is(err, checkpoint.ErrNoGit) || errors.Is(err, checkpoint.ErrNoRepository) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return &ref, nil
		}
	}
	intr := newInterrupts()
	if cfg.Interrupt == nil || tokens != nil {
		if cfg.Interrupt == nil {
			cfg.Interrupt = intr.current
		}
		// A scope change drops every credential the drive holds, so the
		// next call acts with one minted under the new scope.
		var scoped func()
		if tokens != nil {
			scoped = tokens.Drop
		}
		stop, err := intr.watch(ctx, st, id, log.Last()+1, scoped)
		if err != nil {
			return harness.Outcome{}, err
		}
		defer stop()
	}
	// A store that carries live deltas gets the drive's, for the streams
	// that follow the session on any replica the store reaches.
	if pub, ok := st.(session.DeltaPublisher); ok {
		fw := newForwarder(id, pub, cfg.Observer)
		cfg.Observer = fw
		defer fw.close()
	}
	h, err := harness.New(cfg)
	if err != nil {
		return harness.Outcome{}, err
	}
	for {
		intr.reset()
		s, err = st.Get(ctx, id)
		if err != nil {
			return harness.Outcome{}, err
		}
		evs, err := st.Events(ctx, id, 1, 0)
		if err != nil {
			return harness.Outcome{}, err
		}
		// A machine opened on demand starts when the model's response
		// begins a call of a tool that acts on it, which the harness sees
		// in the stream (spec 048): a turn that only talks starts none.
		out, err = h.RunTurn(ctx, s, evs, log)
		if err != nil {
			return harness.Outcome{}, err
		}
		if out.Status == session.StatusEnded {
			return out, cfg.Machine.Release(ctx, true)
		}
		if !out.Pending {
			return out, cfg.Machine.Release(ctx, false)
		}
		if err := r.running(ctx, log); err != nil {
			return harness.Outcome{}, err
		}
	}
}

// setupFailed closes a turn that could not start, with a session.error
// and an idle status, so the session waits for its next message instead
// of staying running with nobody driving it. A core's refusal for spend,
// such as Cella refusing the session's sandbox, stops the turn with
// budget as a model gateway's does (spec 007), so a resume continues it
// once the allowance is raised.
func (r *Runner) setupFailed(ctx context.Context, log *Log, cause error) error {
	code, stop := CodeSetupFailed, session.StopError
	if refusal, spent := models.SpendRefused(cause); spent {
		code, stop = refusal, session.StopBudget
	} else if se, ok := errors.AsType[*SetupError](cause); ok {
		code = se.Code
	} else if oe, ok := errors.AsType[*machine.OpenError](cause); ok {
		code = oe.Code
	} else if mc, ok := errors.AsType[*models.Coded](cause); ok {
		code = mc.Code
	}
	now := r.o.Clock()
	e1, err := session.NewEvent(session.TypeSessionError, session.SessionError{Code: code, Message: cause.Error()}, now)
	if err != nil {
		return errors.Join(cause, err)
	}
	e2, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusIdle, StopReason: stop, Detail: code}, now)
	if err != nil {
		return errors.Join(cause, err)
	}
	_, err = log.Append(context.WithoutCancel(ctx), []session.Event{e1, e2})
	return errors.Join(cause, err)
}

func (r *Runner) running(ctx context.Context, log *Log) error {
	e, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{
		Status: session.StatusRunning, Runner: &session.RunnerRef{ID: r.o.ID, Kind: r.o.Kind},
	}, r.o.Clock())
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, []session.Event{e})
	return err
}

// opened records a machine the session has: at the session's first
// machine it delivers the session's repositories into it, then appends
// session.machine when the session has none for this machine yet, beside
// the running turn when the machine opened on demand, naming the
// repositories delivered, and then writes the files of the session's
// messages the machine has not been given (spec 015). A repository that
// could not be delivered is reported after the machine is recorded, so
// the session keeps the machine and learns what is missing; a file that
// could not be written is a session.error, and the call that opened the
// machine runs.
func (r *Runner) opened(ctx context.Context, s session.Session, m machine.Machine, log *Log, first, beside bool, tokens *TokenSource) error {
	var repos []session.DeliveredRepository
	var delivered error
	if first && len(session.Repositories(s)) > 0 {
		repos, delivered = deliver(ctx, s, m, r.served(tokens))
	}
	// A fork's first machine gets the files of the turn it forked at,
	// over the repositories it was given (spec 017). Files it cannot get
	// leave the fork on its repositories, recorded beside the machine.
	var restored *session.CheckpointRef
	var missing error
	if first && s.Parent != nil {
		restored, missing = r.restoreFork(ctx, s, m)
	}
	if err := r.attach(ctx, s, m, log, beside, repos, restored); err != nil {
		return errors.Join(err, delivered)
	}
	if missing != nil {
		if err := r.checkpointMissing(ctx, log, beside, missing); err != nil {
			return errors.Join(err, delivered)
		}
	}
	return errors.Join(r.attachments(ctx, m, log, beside), delivered)
}

// checkpointMissing appends the session.error of a fork whose fork
// point's files could not be restored (spec 035): the fork goes on with
// its conversation and its repositories, so the error is not retryable
// and stops nothing.
func (r *Runner) checkpointMissing(ctx context.Context, log *Log, beside bool, why error) error {
	e, err := session.NewEvent(session.TypeSessionError, session.SessionError{
		Code: checkpoint.CodeMissing, Message: "The files of the session this one continues could not be restored; it starts from its repositories.", Detail: why.Error(),
	}, r.o.Clock())
	if err != nil {
		return err
	}
	if beside {
		return log.appendBeside(ctx, []session.Event{e})
	}
	_, err = log.Append(ctx, []session.Event{e})
	return err
}

// attachments writes the files of the session's messages the machine
// has not been given, and appends what it wrote and what it could not.
func (r *Runner) attachments(ctx context.Context, m machine.Machine, log *Log, beside bool) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	written, _, err := harness.DeliverAttachments(ctx, m, log, evs, nil, r.o.Clock())
	if err != nil || len(written) == 0 {
		return err
	}
	if beside {
		return log.appendBeside(ctx, written)
	}
	_, err = log.Append(ctx, written)
	return err
}

// attach appends session.machine when the session has none for this
// machine yet: a first attachment, or a machine other than the last one.
// repos are the repositories delivered into the machine, set only at the
// session's first, and restored the checkpoint a fork's first machine was
// given, recorded with reason restored. A fork's copied machines are its
// parent's, so its first machine is a first attachment.
func (r *Runner) attach(ctx context.Context, s session.Session, m machine.Machine, log *Log, beside bool, repos []session.DeliveredRepository, restored *session.CheckpointRef) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	info := m.Info()
	reason := "attached"
	if restored != nil {
		reason = "restored"
	}
	for _, ev := range slices.Backward(evs) {
		if s.Copied(ev) {
			break
		}
		if ev.Type != session.TypeSessionMachine || ev.Redacted() {
			continue
		}
		var p session.SessionMachine
		if err := ev.Decode(&p); err != nil {
			return err
		}
		if p.Machine.Kind == info.Kind && p.Machine.ID == info.ID && p.Machine.Workdir == info.Workdir {
			return nil
		}
		reason = "handoff"
		break
	}
	a, err := attach(ctx, m, log, attachOptions{PersonalInstructions: r.o.PersonalInstructions, PersonalSkills: r.o.PersonalSkills, Now: r.o.Clock()})
	if err != nil {
		return err
	}
	e, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{
		Machine: a.Machine, Reason: reason, Context: a.Context, Instructions: a.Instructions, Skills: a.Skills, Repositories: repos, Checkpoint: restored,
	}, r.o.Clock())
	if err != nil {
		return err
	}
	if beside {
		return log.appendBeside(ctx, []session.Event{e})
	}
	_, err = log.Append(ctx, []session.Event{e})
	return err
}

// hasMachine reports whether the log records a machine the session had;
// a fork's copied machines were its parent's.
func hasMachine(s session.Session, evs []session.Event) bool {
	for _, e := range evs {
		if e.Type == session.TypeSessionMachine && !e.Redacted() && !s.Copied(e) {
			return true
		}
	}
	return false
}

func (r *Runner) checkpointer(cfg harness.Config, s session.Session) *checkpoint.Checkpointer {
	return r.checkpointerOn(cfg.Machine, s)
}

// checkpointerOn takes the checkpoints of s's working directory on m,
// kept at the session's first repository when that is on the
// checkpoint host.
func (r *Runner) checkpointerOn(m machine.Machine, s session.Session) *checkpoint.Checkpointer {
	cp := &checkpoint.Checkpointer{Machine: m, SessionID: s.ID, AgentID: s.Agent.ID, Remote: r.keptAt(s)}
	if r.o.CheckpointDir != "" {
		cp.SessionRepo = r.sessionRepo(s.ID)
	}
	return cp
}

// keptAt is the repository that keeps s's checkpoints past its machine:
// the session's first repository, the one delivered into its working
// directory, when its URL is under CheckpointHost; empty otherwise
// (spec 035).
func (r *Runner) keptAt(s session.Session) string {
	repos := session.Repositories(s)
	host := strings.TrimRight(r.o.CheckpointHost, "/")
	if host == "" || len(repos) == 0 || !strings.HasPrefix(repos[0].URL, host+"/") {
		return ""
	}
	return repos[0].URL
}

// sessionRepo is the session repository of a working directory that is
// not a checkout, under CheckpointDir (spec 034).
func (r *Runner) sessionRepo(id string) string {
	return filepath.Join(r.o.CheckpointDir, id+".git")
}

// Rewind restores the working directory of an idle session to the files
// of a turn's checkpoint (spec 034). It first checkpoints the current
// state when it differs from the last one, so nothing is lost, and
// appends session.rewound. The conversation is not cut.
func (r *Runner) Rewind(ctx context.Context, id string, turn int, by session.Sender) (_ session.SessionRewound, err error) {
	st := r.o.Store
	lease, err := st.Acquire(ctx, id, session.Holder{Runner: r.o.ID, AcquiredAt: r.o.Clock()})
	if err != nil {
		return session.SessionRewound{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	s, err := st.Get(ctx, id)
	if err != nil {
		return session.SessionRewound{}, err
	}
	if s.Status != session.StatusIdle {
		return session.SessionRewound{}, &models.Coded{Code: checkpoint.CodeRewindNotIdle, Message: fmt.Sprintf("session %s is %s", id, s.Status)}
	}
	evs, err := st.Events(ctx, id, 1, 0)
	if err != nil {
		return session.SessionRewound{}, err
	}
	target, rewinds := checkpointOf(evs, turn)
	if target == nil {
		return session.SessionRewound{}, &models.Coded{Code: checkpoint.CodeMissing, Message: fmt.Sprintf("turn %d of %s has no checkpoint", turn, id)}
	}
	built, _ := r.tokens(ctx, id, lease)
	cfg, err := r.o.Harness(built, s)
	if err != nil {
		return session.SessionRewound{}, err
	}
	defer func() { err = errors.Join(err, cfg.Machine.Release(context.WithoutCancel(ctx), false)) }()
	cp := r.checkpointer(cfg, s)
	previous := harness.LastCheckpoint(evs, "")
	saved, made, err := cp.Save(ctx, rewinds+1, previous)
	if err != nil {
		return session.SessionRewound{}, err
	}
	savedCommit := previous
	rw := session.SessionRewound{ToTurn: turn, Checkpoint: *target, By: by}
	if made {
		savedCommit = saved.Commit
		rw.Saved = &saved
	} else if last := lastCheckpointRef(evs); last != nil {
		rw.Saved = last
	}
	if err := cp.Restore(ctx, target.Commit, savedCommit); err != nil {
		return session.SessionRewound{}, err
	}
	e, err := session.NewEvent(session.TypeSessionRewound, rw, r.o.Clock())
	if err != nil {
		return session.SessionRewound{}, err
	}
	if _, err := NewLog(st, id, s.LastSeq).Append(ctx, []session.Event{e}); err != nil {
		return session.SessionRewound{}, err
	}
	return rw, nil
}

// checkpointOf finds the checkpoint of a turn of the session's own
// thread, and counts the rewinds so far.
func checkpointOf(evs []session.Event, turn int) (*session.CheckpointRef, int) {
	var cp *session.CheckpointRef
	rewinds := 0
	for _, e := range evs {
		if e.Redacted() || e.Thread != "" {
			continue
		}
		switch e.Type {
		case session.TypeSessionRewound:
			rewinds++
		case session.TypeSessionStatus:
			var p session.SessionStatus
			if e.Turn == turn && e.Decode(&p) == nil && p.Checkpoint != nil {
				c := *p.Checkpoint
				cp = &c
			}
		}
	}
	return cp, rewinds
}

func lastCheckpointRef(evs []session.Event) *session.CheckpointRef {
	for _, e := range slices.Backward(evs) {
		var p session.SessionStatus
		if e.Type == session.TypeSessionStatus && e.Thread == "" && !e.Redacted() && e.Decode(&p) == nil && p.Checkpoint != nil {
			return p.Checkpoint
		}
	}
	return nil
}

// attached reads what the log's attachments say about the prompt's
// conditional sections: whether the latest machine is in a repository,
// and whether a memory store is attached.
func attached(evs []session.Event) (git, memory bool) {
	for _, e := range evs {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case session.TypeSessionMachine:
			var p session.SessionMachine
			if e.Decode(&p) == nil {
				git = strings.Contains(p.Context, "\nGit: ")
			}
		case session.TypeMemoryAttached:
			memory = true
		}
	}
	return git, memory
}

// interrupts closes one channel per turn when a person appends
// user.interrupt, so the harness cancels the turn's in-flight request and
// running calls at once (spec 005).
type interrupts struct {
	mu     sync.Mutex
	ch     chan struct{}
	closed bool
}

func newInterrupts() *interrupts {
	return &interrupts{ch: make(chan struct{})}
}

// watch follows the log from seq and fires on every user.interrupt, and
// calls scoped, when set, on every session.scope_changed, until the
// returned stop is called or ctx ends.
func (i *interrupts) watch(ctx context.Context, st session.Store, id string, seq uint64, scoped func()) (func(), error) {
	wctx, cancel := context.WithCancel(ctx)
	evs, err := st.Watch(wctx, id, seq)
	if err != nil {
		cancel()
		return nil, err
	}
	go func() {
		for e := range evs {
			switch {
			case e.Type == session.TypeUserInterrupt:
				i.fire()
			case e.Type == session.TypeScopeChanged && scoped != nil:
				scoped()
			}
		}
	}()
	return cancel, nil
}

// reset starts a turn with a fresh channel; an interrupt before the turn
// began does not stop it.
func (i *interrupts) reset() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		i.ch, i.closed = make(chan struct{}), false
	}
}

func (i *interrupts) fire() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.closed {
		close(i.ch)
		i.closed = true
	}
}

func (i *interrupts) current() <-chan struct{} {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ch
}
