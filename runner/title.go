// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"latere.ai/x/topos/harness"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// Titler is a lease that asks the title question itself (spec 083): a
// runner process's, which reaches toposd over its internal listener. The
// answer is as Options.Title's.
type Titler interface {
	Title(ctx context.Context, asked string) (session.ModelRef, error)
}

// titleQuestion is the title question for a drive holding lease on the
// session id: the lease's own, Options.Title's, or nil.
func (r *Runner) titleQuestion(id string, lease session.Lease) func(context.Context, string) (session.ModelRef, error) {
	if q, ok := lease.(Titler); ok {
		return q.Title
	}
	if r.o.Title == nil {
		return nil
	}
	return func(ctx context.Context, asked string) (session.ModelRef, error) {
		return r.o.Title(ctx, id, asked)
	}
}

// titleSetting is spec.title of the latest version of s's agent, as
// LatestTitle reads it, or of the version s runs when LatestTitle is
// unset or cannot answer: nil when the agent sets none.
func (r *Runner) titleSetting(ctx context.Context, s session.Session) (*v1.Title, error) {
	if r.o.LatestTitle != nil {
		set, err := r.o.LatestTitle(ctx, s.Agent.ID)
		if err == nil {
			return set, nil
		}
		r.o.Log.DebugContext(ctx, "title: the agent's latest version could not be read; the session's own is read", "session", s.ID, "err", err)
	}
	res, ok, err := Agent(ctx, r.o.Store, s)
	if err != nil || !ok {
		return nil, err
	}
	return res.Agent.Spec.Title, nil
}

// The ends of one session's title step.
type titleOutcome int

const (
	// titleSkipped asked nothing: the title is not due, or there is no
	// question to ask.
	titleSkipped titleOutcome = iota
	// titleNamed wrote a new title.
	titleNamed
	// titleKept recorded a request that wrote no title.
	titleKept
	// titleFailed was denied, found no model, or its request failed.
	titleFailed
)

// afterTurn names the session after a turn that left it idle with
// nothing pending, when its title is due, while the drive still holds its
// lease, and reports whether input arrived since, which the drive's next
// turn takes on the same lease rather than the queue's next poll (spec
// 083). The title is due when the turn's input held a person's message,
// the rules of blocked hold none against it, and the latest version of
// the session's agent sets spec.title. A title that fails, is denied or
// writes nothing is logged and changes nothing else; only a read of the
// session that fails is an error.
func (r *Runner) afterTurn(ctx context.Context, h *harness.Harness, cfg harness.Config, id string, lease session.Lease, log *Log, out harness.Outcome) (bool, error) {
	if out.Status != session.StatusIdle || out.StopReason == session.StopBudget {
		return false, nil
	}
	s, evs, err := r.read(ctx, id)
	if err != nil {
		return false, err
	}
	if session.HasPendingInput(evs) {
		return true, nil
	}
	if !startedByPerson(evs) || blocked(s, evs) != "" {
		return false, nil
	}
	set, err := r.titleSetting(ctx, s)
	if err != nil {
		r.o.Log.WarnContext(ctx, "title: the agent's setting could not be read", "session", id, "err", err)
		return false, nil
	}
	if set == nil {
		return false, nil
	}
	if r.title(ctx, h, cfg, lease, log, s, evs, set) == titleSkipped {
		return false, nil
	}
	if evs, err = r.o.Store.Events(ctx, id, 1, 0); err != nil {
		return false, err
	}
	return session.HasPendingInput(evs), nil
}

// read is the session's header and log as they stand.
func (r *Runner) read(ctx context.Context, id string) (session.Session, []session.Event, error) {
	s, err := r.o.Store.Get(ctx, id)
	if err != nil {
		return session.Session{}, nil, err
	}
	evs, err := r.o.Store.Events(ctx, id, 1, 0)
	if err != nil {
		return session.Session{}, nil, err
	}
	return s, evs, nil
}

// title asks the title question and, when it is allowed, the harness for
// the session's title (spec 083), and logs how it ended with the session
// and the cause, never a word of the conversation or the title. The
// question is asked as the session's initiator with the name the request
// asks for; the request runs on the model the allow names, else on that
// name, and, when the agent names no title model, on the model the
// session stands on last.
func (r *Runner) title(ctx context.Context, h *harness.Harness, cfg harness.Config, lease session.Lease, log *Log, s session.Session, evs []session.Event, set *v1.Title) titleOutcome {
	ask := r.titleQuestion(s.ID, lease)
	if ask == nil {
		r.o.Log.DebugContext(ctx, "title: this runner has no title question to ask", "session", s.ID)
		return titleSkipped
	}
	asked, names := titleNames(set, s, cfg.Connection.Model)
	allowed, err := ask(ctx, asked)
	if err != nil {
		r.o.Log.InfoContext(ctx, "title: not asked", "session", s.ID, "model", asked, "err", err)
		return titleFailed
	}
	if allowed.Name != "" {
		names = slices.Insert(slices.DeleteFunc(names, func(n string) bool { return n == allowed.Name }), 0, allowed.Name)
	}
	res, err := h.Title(ctx, s, evs, log, names)
	switch {
	case err != nil:
		r.o.Log.WarnContext(ctx, "title: failed", "session", s.ID, "model", res.Model, "err", err)
		return titleFailed
	case res.Kept != "":
		r.o.Log.InfoContext(ctx, "title: kept", "session", s.ID, "model", res.Model, "kept", res.Kept)
		return titleKept
	}
	r.o.Log.InfoContext(ctx, "title: named", "session", s.ID, "model", res.Model)
	return titleNamed
}

// titleNames are the name a title request asks for and the models it may
// run on, in order (spec 083). The name asked is the agent's spec.title
// model, else the name the session's model was asked by, else that model,
// agent being the agent's own where the session never changed it. The
// session's model is the last of the models only where the agent names no
// title model, so a title never runs on the session's model, which may
// cost far more, behind an agent that named another.
func titleNames(set *v1.Title, s session.Session, agent string) (string, []string) {
	current := agent
	if s.Model != nil && s.Model.Name != "" {
		current = s.Model.Name
	}
	asked := current
	if s.Model != nil && s.Model.Via != "" {
		asked = s.Model.Via
	}
	if set.Model != nil {
		return strings.TrimSpace(*set.Model), []string{strings.TrimSpace(*set.Model)}
	}
	return asked, slices.Compact([]string{asked, current})
}

// startedByPerson reports whether the input of the session's last turn
// held a person's message: a user.message of kind person that no request
// before the turn was built from, after the fold_seq of the last request
// of the session's own thread before the turn opened, and before the turn
// closed. A message that arrived while the turn before ran, after its
// last request was built, is this turn's input, as the harness reads it
// pending. A trigger's message starts a turn that keeps the name its
// routine gave the session.
func startedByPerson(evs []session.Event) bool {
	closed, opened := -1, -1
	for i, e := range slices.Backward(evs) {
		var p session.SessionStatus
		if e.Type != session.TypeSessionStatus || e.Thread != "" || e.Decode(&p) != nil {
			continue
		}
		if closed < 0 && p.Status != session.StatusRunning {
			closed = i
		} else if closed >= 0 && p.Status == session.StatusRunning {
			opened = i
			break
		}
	}
	if closed < 0 || opened < 0 {
		return false
	}
	var after uint64
	for _, e := range slices.Backward(evs[:opened]) {
		var mr session.ModelRequest
		if e.Type != session.TypeModelRequest || e.Thread != "" || e.Decode(&mr) != nil || mr.Purpose != "" {
			continue
		}
		after = cmp.Or(mr.FoldSeq, e.Seq)
		break
	}
	for _, e := range evs[:closed] {
		var p session.UserMessage
		if e.Seq > after && e.Type == session.TypeUserMessage && !e.Redacted() && e.Decode(&p) == nil && p.Sender.Kind == session.SenderPerson {
			return true
		}
	}
	return false
}

// blocked is why a session is not named now whatever its agent sets, ""
// when nothing keeps it from it (spec 083): it is not idle, input is
// pending, a person changed its title, a fork's copied change included,
// since a person who named the conversation keeps the name, or its
// budget, where it has one, is spent.
func blocked(s session.Session, evs []session.Event) string {
	switch {
	case s.Status != session.StatusIdle:
		return "the session is " + string(s.Status)
	case session.HasPendingInput(evs):
		return "input is pending"
	case renamedByPerson(evs):
		return "a person named the session"
	case s.Budget.MaxCostUSDMicro != nil && session.Spent(evs)-s.Budget.CarriedCostUSDMicro >= *s.Budget.MaxCostUSDMicro:
		return "the session's budget is spent"
	}
	return ""
}

// renamedByPerson reports whether a person changed the session's title.
func renamedByPerson(evs []session.Event) bool {
	return slices.ContainsFunc(evs, func(e session.Event) bool {
		var p session.TitleChanged
		return e.Type == session.TypeTitleChanged && !e.Redacted() && e.Decode(&p) == nil && p.By.Kind == session.SenderPerson
	})
}

// TitlePassDelay is how long toposd waits after it starts before the pass
// that names the sessions an agent ran before (spec 083), so a rolling
// deploy has settled first.
const TitlePassDelay = time.Minute

// TitlePassInterval is how long the pass waits after a session it asked
// about before the next one.
const TitlePassInterval = 2 * time.Second

// TitleCounts are how a pass ended for each session it read: named, a
// title written; kept, a request recorded that wrote none; skipped, a
// session not due, or whose lease another holder had; and failed, a
// question denied or not answered, no model to run on, or a request or a
// read that failed.
type TitleCounts struct {
	Named, Kept, Skipped, Failed int
}

// TitlePass names the idle hosted sessions of each agent of agents that
// were never named (spec 083), one at a time, waiting interval after each
// it asked about: a session is due when its log holds a person's
// message, no title request and no change of the title by a person, a
// fork's copied events included, so the continuation of a named
// conversation is not named again, and the rules of blocked hold none
// against it. Each is named as after a turn, with the same question and
// the same rules, on its latest exchanges, under its lease, taken as a
// claim takes it: a session whose lease another holder has, a running
// turn's among them, is skipped. A session that failed is due again at
// the next pass. agents are the agents whose latest version sets
// spec.title.
func (r *Runner) TitlePass(ctx context.Context, agents []string, interval time.Duration) (TitleCounts, error) {
	var c TitleCounts
	for _, agent := range agents {
		cursor := ""
		for {
			page, next, err := r.o.Store.List(ctx, session.ListOptions{AgentID: agent, Status: session.StatusIdle, Runner: session.RunnerHosted, Cursor: cursor, Limit: 200})
			if err != nil {
				return c, err
			}
			for _, s := range page {
				out, err := r.passOne(ctx, s)
				if err != nil {
					r.o.Log.WarnContext(ctx, "title: the pass failed on a session", "session", s.ID, "err", err)
					out = titleFailed
				}
				switch out {
				case titleNamed:
					c.Named++
				case titleKept:
					c.Kept++
				case titleFailed:
					c.Failed++
				default:
					c.Skipped++
					continue
				}
				if sleep(ctx, interval); ctx.Err() != nil {
					return c, ctx.Err()
				}
			}
			if next == "" {
				break
			}
			cursor = next
		}
	}
	return c, nil
}

// passOne names one session of a pass when it is due, under its lease.
func (r *Runner) passOne(ctx context.Context, listed session.Session) (_ titleOutcome, err error) {
	if listed.Writer != nil && listed.Writer.Kind != session.RunnerHosted {
		return titleSkipped, nil
	}
	evs, err := r.o.Store.Events(ctx, listed.ID, 1, 0)
	if err != nil || !unnamed(listed, evs) {
		return titleSkipped, err
	}
	lease, err := r.o.Store.Acquire(ctx, listed.ID, r.holder())
	if errors.Is(err, session.ErrLocked) || errors.Is(err, session.ErrNotFound) {
		return titleSkipped, nil
	}
	if err != nil {
		return titleFailed, err
	}
	defer func() { err = errors.Join(err, lease.Release()) }()
	// The session may have moved between the list and the lease.
	s, evs, err := r.read(ctx, listed.ID)
	if err != nil || !unnamed(s, evs) {
		return titleSkipped, err
	}
	set, err := r.titleSetting(ctx, s)
	if err != nil || set == nil {
		return titleSkipped, err
	}
	built, _ := r.tokens(ctx, s.ID, lease)
	cfg, err := r.o.Harness(built, s)
	if err != nil {
		return titleFailed, err
	}
	defer func() { err = errors.Join(err, cfg.Machine.Release(context.WithoutCancel(ctx), false)) }()
	h, err := harness.New(cfg)
	if err != nil {
		return titleFailed, err
	}
	log := NewLog(r.o.Store, s.ID, s.LastSeq)
	log.lost = lease.Lost()
	if f, ok := lease.(session.Fence); ok {
		log.fence = f
	}
	return r.title(ctx, h, cfg, lease, log, s, evs, set), nil
}

// unnamed reports whether a session is due for the pass: its log holds a
// person's message and no title request, and blocked holds nothing
// against it.
func unnamed(s session.Session, evs []session.Event) bool {
	person, asked := false, false
	for _, e := range evs {
		var p session.UserMessage
		switch {
		case session.Purpose(e) == session.PurposeTitle:
			asked = true
		case e.Type == session.TypeUserMessage && !e.Redacted() && e.Decode(&p) == nil && p.Sender.Kind == session.SenderPerson:
			person = true
		}
	}
	return person && !asked && blocked(s, evs) == ""
}
