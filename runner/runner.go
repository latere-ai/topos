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
	"slices"
	"strings"
	"time"

	"latere.ai/x/topos/harness"
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
	Clock                func() time.Time
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

// Drive runs one session in process until it is idle or ended: its
// lease is the store's own, held for the whole drive.
func (r *Runner) Drive(ctx context.Context, id string) (out harness.Outcome, err error) {
	st := r.o.Store
	lease, err := st.Acquire(ctx, id, session.Holder{Runner: r.o.ID, AcquiredAt: r.o.Clock()})
	if err != nil {
		return harness.Outcome{}, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	s, err := st.Get(ctx, id)
	if err != nil {
		return harness.Outcome{}, err
	}
	if s.Status == session.StatusEnded {
		return harness.Outcome{}, fmt.Errorf("%w: %s", ErrEnded, id)
	}
	log := NewLog(st, id, s.LastSeq)
	if err := r.running(ctx, log); err != nil {
		return harness.Outcome{}, err
	}
	cfg, err := r.o.Harness(ctx, s)
	if err != nil {
		return harness.Outcome{}, err
	}
	if err := r.attach(ctx, cfg, log); err != nil {
		return harness.Outcome{}, err
	}
	evs, err := st.Events(ctx, id, 1, 0)
	if err != nil {
		return harness.Outcome{}, err
	}
	git, memory := attached(evs)
	cfg.Prompt.Git = cfg.Prompt.Git || git
	cfg.Prompt.Memory = cfg.Prompt.Memory || memory
	h, err := harness.New(cfg)
	if err != nil {
		return harness.Outcome{}, err
	}
	for {
		s, err = st.Get(ctx, id)
		if err != nil {
			return harness.Outcome{}, err
		}
		evs, err := st.Events(ctx, id, 1, 0)
		if err != nil {
			return harness.Outcome{}, err
		}
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

// attach appends session.machine when the session has none for this
// machine yet: a first attachment, or a machine other than the last one.
func (r *Runner) attach(ctx context.Context, cfg harness.Config, log *Log) error {
	evs, err := r.o.Store.Events(ctx, log.id, 1, 0)
	if err != nil {
		return err
	}
	info := cfg.Machine.Info()
	reason := "attached"
	for _, ev := range slices.Backward(evs) {
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
	a, err := attach(ctx, cfg.Machine, log, attachOptions{PersonalInstructions: r.o.PersonalInstructions, PersonalSkills: r.o.PersonalSkills, Now: r.o.Clock()})
	if err != nil {
		return err
	}
	e, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{
		Machine: a.Machine, Reason: reason, Context: a.Context, Instructions: a.Instructions, Skills: a.Skills,
	}, r.o.Clock())
	if err != nil {
		return err
	}
	_, err = log.Append(ctx, []session.Event{e})
	return err
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
