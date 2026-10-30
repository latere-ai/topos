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
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/memory"
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
	// (spec 051), or it wrote a tool call as text (spec 062), standing,
	// failed, reason and detail as harness.Config.Failover says: toposd's
	// question to its authorizer, for the leases of its own runners. A
	// lease that asks it itself (Failover) is asked instead; with neither,
	// a drive asks nothing, and such a turn ends after harness.DownRetry,
	// with the model's error, or with the response written as its answer.
	Failover func(ctx context.Context, id string, standing, failed session.ModelRef, reason, detail string) (session.ModelRef, error)
	// Apps reads the commits an app is served at from the installation's
	// app host, with the drive's token source, nil when the drive reaches
	// no session credentials (spec 058). app is the app as the session was
	// given it, with its id when the allow named one (spec 071). Nil
	// starts the checkout of every app a session is attached at its
	// repository's default branch.
	Apps func(ctx context.Context, tokens *TokenSource, app session.ResourceApp) (AppCommits, error)
	// LatestTitle reads spec.title of the latest version of the agent of
	// that id, nil when it sets none (spec 083): toposd's agents store.
	// Unset, or when it cannot answer, the version the session runs is
	// read from the session's bundle.
	LatestTitle func(ctx context.Context, agentID string) (*v1.Title, error)
	// Title asks whether the session id may be named now, asked the name
	// its title request asks for (spec 083): toposd's question
	// session.title to its authorizer, for the leases of its own runners.
	// The answer is the model the allow names, with the route it names,
	// zero when it names none; a deny or a question that could not be
	// asked is an error, and the session keeps its title. A lease that asks
	// it itself (Titler) is asked instead; with neither, a drive names no
	// session.
	Title func(ctx context.Context, id, asked string) (session.ModelRef, error)
	// Triggers is the session id's reach to the triggers the server keeps,
	// which the trigger tool asks (spec 086): toposd's, for the leases of
	// its own runners. A lease that is a Triggers itself is asked instead;
	// with neither, the harness builder reads none and offers no trigger
	// tool.
	Triggers func(id string) Triggers
	// Conversations is the session id's reach to its folders of
	// conversations (spec 094): toposd's, for the leases of its own
	// runners. A lease that is a memory.Conversations itself is asked
	// instead; with neither, a session with such a folder cannot be
	// driven.
	Conversations func(id string) memory.Conversations
	// Log receives a line for each title a drive or a pass asked for, and
	// for each that it could not, with the session and the cause (spec
	// 083). Nil discards them.
	Log   *slog.Logger
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
// request (spec 051), or it wrote a tool call as text (spec 062): a runner
// process's, which reaches toposd over its internal listener.
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
	// settings are the title settings titleSetting read, for
	// TitleSettingTTL (spec 083).
	settings *settingCache
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
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Runner{o: o, settings: &settingCache{m: map[string]cachedSetting{}}}, nil
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
	// context; the fence above goes on reading the drive's own. A machine
	// opened on demand that begins to wait on a person's computer, for its
	// owner to allow the session, says so through the same context, and
	// the session records it beside the running turn, so a client shows
	// the person what it waits for while the call is held (spec 080).
	built, tokens := r.tokens(ctx, id, lease)
	if t := r.triggers(id, lease); t != nil {
		built = WithTriggers(built, t)
	}
	if c := r.conversations(id, lease); c != nil {
		built = WithConversations(built, c)
	}
	built = machine.WithWaiting(built, func(ctx context.Context, w machine.Waiting) error {
		return r.waiting(ctx, log, w)
	})
	cfg, err := r.o.Harness(built, s)
	if err != nil {
		return harness.Outcome{}, r.setupFailed(ctx, log, err)
	}
	if cfg.Failover == nil {
		cfg.Failover = r.failover(id, lease)
	}
	// A file an answer links is typed by the rule the files route answers
	// a file by (spec 064).
	if cfg.MediaType == nil {
		cfg.MediaType = MediaType
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
			return r.opened(ctx, s, m, log, first, true, tokens, cfg.Files)
		})
	default:
		if err := r.opened(ctx, s, cfg.Machine, log, first, false, tokens, cfg.Files); err != nil {
			return harness.Outcome{}, r.setupFailed(ctx, log, err)
		}
	}
	if evs, err = st.Events(ctx, id, 1, 0); err != nil {
		return harness.Outcome{}, err
	}
	git, memory := attached(evs)
	cfg.Prompt.Git = cfg.Prompt.Git || git || len(session.Repositories(s)) > 0
	cfg.Prompt.Memory = cfg.Prompt.Memory || memory
	// A device session keeps no checkpoint: its folder is the person's own
	// working copy, which a checkpoint's refs would write into and a
	// restore would write old files over (spec 080).
	if cfg.Checkpoint == nil && s.Machine.Kind == session.MachineDevice {
		cfg.Checkpoint = func(context.Context, int, string) (*session.CheckpointRef, error) { return nil, nil }
	}
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
	// The drive follows the log for the interrupts that cancel a turn, the
	// input that ends a title the drive asks for after one (spec 083), and
	// the scope changes that drop the drive's credentials.
	intr := newInterrupts()
	if cfg.Interrupt == nil {
		cfg.Interrupt = intr.current
	}
	if cfg.SendNow == nil {
		cfg.SendNow = intr.sentNow
	}
	// A scope change drops every credential the drive holds, so the next
	// call acts with one minted under the new scope.
	var scoped func()
	if tokens != nil {
		scoped = tokens.Drop
	}
	stop, err := intr.watch(ctx, st, id, log.Last()+1, scoped)
	if err != nil {
		return harness.Outcome{}, err
	}
	defer stop()
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
		// A turn that left the session idle with nothing pending may be
		// followed by its title, still under the lease; input that arrived
		// meanwhile starts the next turn here, as pending input does (spec
		// 083).
		if !out.Pending {
			again, err := r.afterTurn(ctx, h, cfg, id, lease, log, intr, out)
			if err != nil {
				return out, errors.Join(err, cfg.Machine.Release(ctx, false))
			}
			if !again {
				return out, cfg.Machine.Release(ctx, false)
			}
		}
		if err := r.running(ctx, log); err != nil {
			// A session the end route or the reaper ended while it was
			// idle lets its machine go as one its turn ended does.
			if errors.Is(err, ErrEnded) {
				return harness.Outcome{}, errors.Join(err, cfg.Machine.Release(ctx, true))
			}
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
	return log.claim(ctx, []session.Event{e})
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
func (r *Runner) opened(ctx context.Context, s session.Session, m machine.Machine, log *Log, first, beside bool, tokens *TokenSource, files harness.Files) error {
	// A device's folder is the person's own working copy: no repository
	// is delivered into it and no checkpoint restored over it (spec 080).
	device := s.Machine.Kind == session.MachineDevice
	var repos []session.DeliveredRepository
	var delivered error
	if first && len(session.Repositories(s)) > 0 && !device {
		repos, delivered = deliver(ctx, s, m, r.served(tokens))
	}
	// A fork's first machine gets the files of the turn it forked at,
	// over the repositories it was given (spec 017). Files it cannot get
	// leave the fork on its repositories, recorded beside the machine.
	var restored *session.CheckpointRef
	var missing error
	if first && s.Parent != nil && !device {
		restored, missing = r.restoreFork(ctx, s, m)
	}
	// A machine made new that no checkpoint filled gets back what the
	// session's log holds of its working directory, before the call that
	// opened it runs (spec 065).
	var rebuilt *session.Restore
	var unbuilt error
	if restored == nil {
		if rebuilt, unbuilt = r.rebuildNew(ctx, s, m, first, files); unbuilt != nil && first {
			// A fork's files that could not be written back are missing
			// as a checkpoint's are.
			missing = errors.Join(missing, unbuilt)
		}
	}
	if err := r.attach(ctx, s, m, log, beside, repos, restored, rebuilt, unbuilt); err != nil {
		return errors.Join(err, delivered)
	}
	if missing != nil {
		if err := r.checkpointMissing(ctx, log, beside, missing, rebuilt != nil); err != nil {
			return errors.Join(err, delivered)
		}
	}
	// A machine that restarts after this is recorded where it happens, in
	// a call of the turn the harness runs.
	if rec, ok := m.(machine.Recovering); ok {
		rec.OnRestart(func(ctx context.Context, rs machine.Restart) error {
			return r.restarted(ctx, s, m, log, true, rs)
		})
	}
	return errors.Join(r.attachments(ctx, m, log, files, beside), delivered)
}

// waiting appends the session.machine_waiting of a machine opened on
// demand that began to wait, beside the running turn (spec 080).
func (r *Runner) waiting(ctx context.Context, log *Log, w machine.Waiting) error {
	reason := session.MachineOffline
	if w.Reason == machine.WaitApproval {
		reason = session.MachineApproval
	}
	e, err := session.NewEvent(session.TypeMachineWaiting, session.SessionMachineWaiting{Reason: reason, Environment: w.Environment}, r.o.Clock())
	if err != nil {
		return err
	}
	return log.appendBeside(ctx, []session.Event{e})
}

// restarted appends the session.machine of a machine that stopped on a
// failure and runs again as the same machine (spec 009): reason
// restarted, and the cause its provider named. The record restates the
// context, the instruction files and the skills of the machine's last
// record: the files they were read from are kept, so the system prompt a
// provider cached holds, and a fold that takes the latest record for its
// system parts finds them there.
func (r *Runner) restarted(ctx context.Context, s session.Session, m machine.Machine, log *Log, beside bool, rs machine.Restart) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	info := m.Info()
	p := session.SessionMachine{Reason: session.MachineRestarted}
	if rs.OutOfMemory {
		p.Cause = session.MachineOutOfMemory
	}
	recorded := false
	for _, ev := range slices.Backward(evs) {
		if s.Copied(ev) {
			break
		}
		if ev.Type != session.TypeSessionMachine || ev.Redacted() {
			continue
		}
		var last session.SessionMachine
		if err := ev.Decode(&last); err != nil {
			return err
		}
		if last.Machine.Kind == info.Kind && last.Machine.ID == info.ID && last.Machine.Workdir == info.Workdir {
			p.Machine, p.Context, p.Instructions, p.Skills = last.Machine, last.Context, last.Instructions, last.Skills
			recorded = true
		}
		break
	}
	if !recorded {
		// The log holds no record of this machine to restate, so it is
		// read as at an attachment.
		a, err := attach(ctx, m, log, attachOptions{PersonalInstructions: r.o.PersonalInstructions, PersonalSkills: r.o.PersonalSkills})
		if err != nil {
			return err
		}
		p.Machine, p.Context, p.Instructions, p.Skills = a.Machine, a.Context, a.Instructions, a.Skills
	}
	e, err := session.NewEvent(session.TypeSessionMachine, p, r.o.Clock())
	if err != nil {
		return err
	}
	if beside {
		return log.appendBeside(ctx, []session.Event{e})
	}
	_, err = log.Append(ctx, []session.Event{e})
	return err
}

// The sentences of a session.error checkpoint_missing, for a person: a
// fork that starts on its repositories alone, and one whose working
// directory was rebuilt from its log in the checkpoint's place (spec
// 065).
const (
	MessageCheckpointMissing = "The files of the session this one continues could not be restored; it starts from its repositories."
	MessageCheckpointRebuilt = "The files of the session this one continues could not be restored from its checkpoint; its working directory was rebuilt from the conversation's record instead."
)

// checkpointMissing appends the session.error of a fork whose fork
// point's files could not be restored (spec 035): the fork goes on with
// its conversation and its repositories, or, when rebuilt says so, with
// the working directory its log holds, so the error is not retryable
// and stops nothing.
func (r *Runner) checkpointMissing(ctx context.Context, log *Log, beside bool, why error, rebuilt bool) error {
	message := MessageCheckpointMissing
	if rebuilt {
		message = MessageCheckpointRebuilt
	}
	e, err := session.NewEvent(session.TypeSessionError, session.SessionError{
		Code: checkpoint.CodeMissing, Message: message, Detail: why.Error(),
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
// has not been given, those of its folder of a files store from files
// (spec 068), and appends what it wrote and what it could not.
func (r *Runner) attachments(ctx context.Context, m machine.Machine, log *Log, files harness.Files, beside bool) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	written, _, err := harness.DeliverAttachments(ctx, m, log, files, evs, nil, r.o.Clock())
	if err != nil || len(written) == 0 {
		return err
	}
	if beside {
		return log.appendBeside(ctx, written)
	}
	_, err = log.Append(ctx, written)
	return err
}

// MessageMachineLost is the one sentence of a session.error machine_lost,
// for a person (spec 009).
const MessageMachineLost = "The session's machine was lost and replaced with a new one. The files that were in its working directory are gone."

// attach appends session.machine when the session has none for this
// machine yet, or when the machine says its open brought it back: a
// first attachment, a machine other than the last one, or the last one
// restarted. repos are the repositories delivered into the machine, set
// only at the session's first, and restored the checkpoint a fork's first
// machine was given, recorded with reason restored. A fork's copied
// machines are its parent's, so its first machine is a first attachment.
//
// A machine other than the last one is a handoff, the session moved,
// unless the machine says its open made it new in the place of one of
// its kind: the one the session had is gone and its files with it, which
// is recorded as replaced with a session.error machine_lost, so the log
// says the files are gone and the model reads it (spec 009).
//
// rebuilt is what the session's log gave back of the working directory
// of a machine made new (spec 065): the machine is then recorded as
// restored, with the rebuild's record and no machine_lost. unbuilt is
// why such a rebuild could not be written, which a replaced machine's
// machine_lost carries.
func (r *Runner) attach(ctx context.Context, s session.Session, m machine.Machine, log *Log, beside bool, repos []session.DeliveredRepository, restored *session.CheckpointRef, rebuilt *session.Restore, unbuilt error) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	info := m.Info()
	rec, recovering := m.(machine.Recovering)
	reason := session.MachineAttached
	if restored != nil {
		reason = session.MachineRestored
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
			if !recovering {
				return nil
			}
			if rs, ok := rec.Restarted(); ok {
				return r.restarted(ctx, s, m, log, beside, rs)
			}
			return nil
		}
		reason = session.MachineHandoff
		if recovering && rec.Created() && p.Machine.Kind == info.Kind {
			reason = session.MachineReplaced
		}
		break
	}
	if rebuilt != nil {
		reason = session.MachineRestored
	}
	a, err := attach(ctx, m, log, attachOptions{PersonalInstructions: r.o.PersonalInstructions, PersonalSkills: r.o.PersonalSkills})
	if err != nil {
		return err
	}
	e, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{
		Machine: a.Machine, Reason: reason, Context: a.Context, Instructions: a.Instructions, Skills: a.Skills, Repositories: repos, Checkpoint: restored, Restore: rebuilt,
	}, r.o.Clock())
	if err != nil {
		return err
	}
	batch := []session.Event{e}
	if reason == session.MachineReplaced {
		detail := fmt.Sprintf("the %s machine %s is new and empty; the one the session had is gone", info.Kind, info.ID)
		if unbuilt != nil {
			detail += "; " + unbuilt.Error()
		}
		lost, err := session.NewEvent(session.TypeSessionError, session.SessionError{
			Code: machine.CodeLost, Message: MessageMachineLost, Retryable: true, Detail: detail,
		}, r.o.Clock())
		if err != nil {
			return err
		}
		batch = append(batch, lost)
	}
	if beside {
		return log.appendBeside(ctx, batch)
	}
	_, err = log.Append(ctx, batch)
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
		if e.Redacted() || e.Subagent != "" {
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
		if e.Type == session.TypeSessionStatus && e.Subagent == "" && !e.Redacted() && e.Decode(&p) == nil && p.Checkpoint != nil {
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
// running calls at once (spec 005), sends on another each message a
// person sent to be read now (spec 098), and ends a title the drive asks
// for between turns when any input arrives after it began (spec 083).
type interrupts struct {
	mu     sync.Mutex
	ch     chan struct{}
	closed bool
	// now carries the turn's messages sent to be read now. A message it
	// has no room for is read at the step after, as any other.
	now chan session.Event
	// input is the sequence of the latest input event the watch saw; after
	// and onInput are the title waiting on input past after, ended at once
	// by onInput.
	input   uint64
	after   uint64
	onInput func()
}

// sendNowRoom is how many messages sent to be read now a turn's channel
// holds before the harness takes them.
const sendNowRoom = 8

func newInterrupts() *interrupts {
	return &interrupts{ch: make(chan struct{}), now: make(chan session.Event, sendNowRoom)}
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
			if isInput(e.Type) {
				i.arrived(e.Seq)
			}
			switch {
			case e.Type == session.TypeUserInterrupt:
				i.fire()
			case sentNow(e):
				i.sendNow(e)
			case e.Type == session.TypeScopeChanged && scoped != nil:
				scoped()
			}
		}
	}()
	return cancel, nil
}

// isInput reports whether an event of typ is input a runner acts on: a
// person's message, a confirmation, a client tool's result, an answer, an
// interrupt or a resume.
func isInput(typ session.Type) bool {
	switch typ {
	case session.TypeUserMessage, session.TypeUserToolConfirmation, session.TypeUserToolResult, session.TypeUserAnswer, session.TypeUserInterrupt, session.TypeSessionResumed:
		return true
	}
	return false
}

// arrived records an input event at seq, and ends the title waiting on
// input before it.
func (i *interrupts) arrived(seq uint64) {
	i.mu.Lock()
	i.input = max(i.input, seq)
	end := i.onInput
	if end != nil && seq > i.after {
		i.onInput = nil
	} else {
		end = nil
	}
	i.mu.Unlock()
	if end != nil {
		end()
	}
}

// endOnInput calls end at once when the watch saw, or sees, an input event
// past seq, and returns the function that stops waiting for one.
func (i *interrupts) endOnInput(seq uint64, end func()) func() {
	i.mu.Lock()
	if i.input > seq {
		i.mu.Unlock()
		end()
		return func() {}
	}
	i.after, i.onInput = seq, end
	i.mu.Unlock()
	return func() {
		i.mu.Lock()
		defer i.mu.Unlock()
		i.onInput = nil
	}
}

// reset starts a turn with fresh channels; an interrupt before the turn
// began does not stop it, and a message sent to be read now before it is
// read by its first request.
func (i *interrupts) reset() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		i.ch, i.closed = make(chan struct{}), false
	}
	i.now = make(chan session.Event, sendNowRoom)
}

// sentNow reports whether e is a message a person sent to the session's
// own thread to be read now.
func sentNow(e session.Event) bool {
	if e.Type != session.TypeUserMessage || e.Subagent != "" || e.Redacted() {
		return false
	}
	var p session.UserMessage
	return e.Decode(&p) == nil && p.SendNow && p.Sender.Kind == session.SenderPerson
}

func (i *interrupts) sendNow(e session.Event) {
	i.mu.Lock()
	defer i.mu.Unlock()
	select {
	case i.now <- e:
	default:
	}
}

func (i *interrupts) sentNow() <-chan session.Event {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.now
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
