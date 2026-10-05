// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package harness is one agent's loop over a session (spec 005): it
// folds the log, builds each step's request, streams it through a
// models.Model, decides and runs the calls through the machine, and
// appends every event through the Log it is handed. It dials nothing.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
	"latere.ai/x/pkg/llmdialect/tokencount"
	"latere.ai/x/pkg/retry"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Log is the harness's view of a session's store, held by the runner.
type Log interface {
	// Append appends a batch after the session's last sequence, setting
	// each event's Seq and SessionID in place, and returns the events
	// others appended since the harness last saw the log. An error stops
	// the turn at once.
	Append(ctx context.Context, batch []session.Event) ([]session.Event, error)
	PutBlob(ctx context.Context, r io.Reader) (session.Digest, error)
	Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error)
}

// Delta is one streamed fragment of a response, for attached clients.
// Deltas are never appended: the agent.message that follows is the
// record.
type Delta struct {
	Thread     string
	Turn, Step int
	Event      ir.Event
}

// Observer receives deltas as a response arrives, and a reset when a
// retried request discards a step's partial output.
type Observer interface {
	OnDelta(Delta)
	OnReset(thread string, turn, step int)
}

// DefaultRetry is spec 005's retry policy.
var DefaultRetry = retry.Policy{MaxAttempts: 6, Base: 2 * time.Second, Max: 60 * time.Second, Jitter: 0.2}

// DownRetry is the policy of a request whose model cannot serve now
// (models.Down, spec 051) on a turn that cannot move to another model: one
// quick retry, a second after the failure, in place of DefaultRetry's
// minute of backoff, since a gateway that answers so has already tried
// every target of the model. A Retry-After longer than its wait ends the
// attempts at once. A turn that can move asks for another model instead,
// with no retry of this one.
var DownRetry = retry.Policy{MaxAttempts: 2, Base: time.Second, Max: time.Second, Jitter: -1}

// MaxModelSwitches is how many times one turn moves to another model when
// the one it runs cannot serve (spec 051): enough to pass three entries of
// a routed name that fail together, as free models behind one provider
// account do, and reach a fourth. A failure past it takes DownRetry's
// quick retry and then ends the turn.
const MaxModelSwitches = 3

// MaxContinuations is how many truncated responses in a row a turn
// continues before it ends with output_limit.
const MaxContinuations = 2

// OutputCap is the max_tokens a step's request asks first, or the
// model's output limit when that is lower. A gateway reserves a
// request's max_tokens at the output price against the caller's budget
// before it runs, so asking the whole output limit on every request
// holds back many times what a step spends; a response that stops at the
// cap is sent again at the model's output limit.
const OutputCap = 8192

// MaxParallel is how many parallel calls of a step run at once.
const MaxParallel = 8

// Config is what a harness runs with.
type Config struct {
	Model      models.Model
	Connection models.Connection
	// Entry is the model's resolved figures, the catalog's overlaid by
	// the agent's own; its output limit bounds every request's
	// max_tokens, which asks OutputCap first.
	Entry   models.Entry
	Machine machine.Machine
	Tools   *tools.Registry
	Policy  Policy
	// Decider decides each validated call (spec 037); nil decides by the
	// rules of spec 012 over Policy.
	Decider Decider
	// Name is the agent's name, which a message it sends a thread carries.
	Name string
	// Instructions are the agent's own instructions.
	Instructions string
	// Subagents are the agents this agent's threads may spawn (spec 013).
	Subagents map[string]Subagent
	// Advisor is the model the advisor tool asks (spec 013); nil offers
	// no advisor.
	Advisor *Subagent
	// Question offers the question tool to the session's own thread (spec
	// 039): the runner sets it when the agent's tools name the tool.
	Question bool
	// MaxDepth bounds how deep threads nest: zero is 2, at most 4.
	MaxDepth int
	// MaxConcurrent bounds the threads running at once in a session:
	// zero is 8.
	MaxConcurrent int
	Effort        string
	// PromptVersion is the harness prompt version; zero is the current.
	PromptVersion int
	Prompt        prompts.HarnessOptions
	Retry         retry.Policy
	// TurnTimeout bounds a turn's wall clock; zero is the session's
	// limit, then the default.
	TurnTimeout time.Duration
	Clock       func() time.Time
	// CompactAt is the fraction of the input window past which the
	// context is cleared and then compacted (spec 010): zero is 0.8, and
	// it is held between 0.5 and 0.95.
	CompactAt float64
	// Checkpoint takes the turn's checkpoint of the working directory
	// (spec 034), chained to previous; nil takes none. It returns nil for
	// a machine that keeps no checkpoints.
	Checkpoint func(ctx context.Context, turn int, previous string) (*session.CheckpointRef, error)
	// Interrupt returns the current turn's interrupt channel, closed when a
	// person interrupts the turn (spec 005): an in-flight request and the
	// running calls are canceled at once. Nil sees interrupts only at step
	// boundaries, through the log.
	Interrupt func() <-chan struct{}
	// Sleep waits between attempts; nil waits on a timer.
	Sleep    func(ctx context.Context, d time.Duration) error
	Observer Observer
	// Connect resolves a model the session switched to (spec 015): its
	// stream, its connection and its figures, the way the agent's own was
	// resolved. RunTurn asks it when the session names a model other than
	// the connection's, runs the whole turn on the answer, and keeps it
	// for the turns after. Nil runs every turn on the configured model.
	Connect func(ctx context.Context, name string) (models.Model, models.Connection, models.Entry, error)
	// Failover asks which model a turn continues on when the model it runs
	// cannot serve now (spec 051), for a session on a routed name alone:
	// failed is the model the turn ran, with the name it was picked for as
	// via, and detail the gateway's developer detail of the failure, ""
	// for none, from which an installation tells a provider's rate limit
	// from its outage. The answer names the model to run in its place with
	// the same via, at the reasoning level it answers, the agent's own
	// resolved; an answer that names the failed model or none moves
	// nothing. The runner asks the installation's authorizer. Nil asks
	// nothing, and such a failure ends the turn after DownRetry.
	Failover func(ctx context.Context, failed session.ModelRef, detail string) (session.ModelRef, error)

	// ceiling is the stricter of the modes the agents above a thread name,
	// empty for the session's own thread: a change of the session's mode
	// moves a thread no further than its own agents allow (spec 041).
	ceiling Mode
}

// Harness runs turns of one agent.
type Harness struct {
	c      Config
	prompt string
	// switched is the model the session last switched to as Connect
	// answered it, shared by the copies a turn makes.
	switched *switchedModel
}

// switchedModel is one model Connect answered.
type switchedModel struct {
	mu    sync.Mutex
	name  string
	model models.Model
	conn  models.Connection
	entry models.Entry
}

// The error codes of spec 005.
const (
	CodeModelError      = "model_error"
	CodeOutputTruncated = "output_truncated"
	CodeInternal        = "internal"
	// CodeModelBusy ends a turn whose model could not serve now and that
	// could not move to another model, or ran out of moves (spec 051). The
	// session.error carries MessageModelBusy, and the gateway's answer in
	// its detail; a person's next message starts a turn as any does.
	CodeModelBusy = "model_busy"
)

// MessageModelBusy is the one sentence of CodeModelBusy, for a person.
const MessageModelBusy = "The model is busy right now. Send your message again in a moment."

// New checks the configuration: a model, a valid connection, a machine,
// a registry, and a catalog entry with an output limit.
func New(c Config) (*Harness, error) {
	switch {
	case c.Model == nil:
		return nil, errors.New("harness: no model")
	case c.Machine == nil:
		return nil, errors.New("harness: no machine")
	case c.Tools == nil:
		return nil, errors.New("harness: no tool registry")
	}
	if err := c.Connection.Validate(); err != nil {
		return nil, err
	}
	if c.Entry.MaxOutputTokens <= 0 || c.Entry.InputWindow <= 0 {
		return nil, &models.Coded{Code: models.CodeUnknown, Message: fmt.Sprintf("no input window and output limit for %q", c.Connection.Model)}
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	if c.Retry == (retry.Policy{}) {
		c.Retry = DefaultRetry
	}
	if c.Sleep == nil {
		c.Sleep = sleep
	}
	p, err := harnessPrompt(c.PromptVersion, c.Prompt)
	if err != nil {
		return nil, err
	}
	if c.PromptVersion == 0 {
		c.PromptVersion = prompts.HarnessCurrent
	}
	return &Harness{c: c, prompt: p, switched: &switchedModel{}}, nil
}

// on points the harness's configuration at the model name, which the
// session switched to, asking Connect the first time the name is asked.
// A model that cannot be connected, or that has no input window or
// output limit, is an error with its code.
func (h *Harness) on(ctx context.Context, name string) error {
	sw := h.switched
	sw.mu.Lock()
	defer sw.mu.Unlock()
	if sw.name != name {
		if h.c.Connect == nil {
			return &models.Coded{Code: models.CodeUnavailable, Message: fmt.Sprintf("the session runs %q, and this runner connects %q alone", name, h.c.Connection.Model)}
		}
		model, conn, entry, err := h.c.Connect(ctx, name)
		if err != nil {
			return err
		}
		if err := conn.Validate(); err != nil {
			return err
		}
		if entry.MaxOutputTokens <= 0 || entry.InputWindow <= 0 {
			return &models.Coded{Code: models.CodeUnknown, Message: fmt.Sprintf("no input window and output limit for %q", name)}
		}
		if model == nil {
			model = h.c.Model
		}
		sw.name, sw.model, sw.conn, sw.entry = name, model, conn, entry
	}
	h.c.Model, h.c.Connection, h.c.Entry = sw.model, sw.conn, sw.entry
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Outcome is how a turn ended. Pending reports input a person appended
// after the turn's last request was built, which the model has not seen:
// the runner starts the next turn at once.
type Outcome struct {
	Status     session.Status
	StopReason session.StopReason
	Detail     string
	Pending    bool
}

// RunTurn runs one turn of the session's own thread and returns when the
// session is idle or ended. The runner has claimed the session and
// appended its session.status running before calling. RunTurn reads
// nothing but its arguments and the machine, and appends only through l.
func (h *Harness) RunTurn(ctx context.Context, s session.Session, log []session.Event, l Log) (Outcome, error) {
	// A session that records its merged policy is decided by it, and so
	// is every thread the turn spawns, rather than by the agent's own.
	if s.Policy != nil {
		scoped := *h
		scoped.c.Policy = h.c.Policy.Under(*s.Policy)
		h = &scoped
	}
	// A session that switched its model runs the turn on it, from the
	// turn's first request to its last (spec 015); a switch appended
	// while the turn runs waits for the next one.
	var unconnected error
	if want := s.Model; want != nil && want.Name != h.c.Connection.Model {
		scoped := *h
		unconnected = scoped.on(ctx, want.Name)
		h = &scoped
	}
	// A session that changed its reasoning level runs the turn at it
	// (spec 015), and so does a thread whose agent names none; an empty
	// one is a header from before a change carried a level, which runs at
	// the agent's.
	if want := s.Model; want != nil && want.Level() != "" && want.Level() != h.c.Effort {
		scoped := *h
		scoped.c.Effort = want.Level()
		h = &scoped
	}
	t := &turn{h: h, s: s, sh: &shared{events: append([]session.Event(nil), log...)}, l: l, root: h.c.Tools, num: s.Turn + 1, start: h.c.Clock()}
	if s.Model != nil {
		t.model = *s.Model
	}
	var err error
	if t.reg, err = t.registry(h.c.Tools.Names()); err != nil {
		return Outcome{}, err
	}
	if unconnected != nil {
		return t.unconnected(ctx, unconnected)
	}
	if n := len(log); n > 0 {
		t.startSeq = log[n-1].Seq
	}
	t.seen = t.startSeq
	limit := h.c.TurnTimeout
	if limit == 0 {
		if d, err := time.ParseDuration(s.Limits.TurnTimeout); err == nil && d > 0 {
			limit = d
		} else {
			limit = session.DefaultTurnTimeout
		}
	}
	t.deadline = t.start.Add(limit)
	out, err := t.run(ctx)
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// unconnected closes a turn whose session's model could not be had with
// a session.error of the model's code and an idle error, before any
// request: the session waits for its next switch or message.
func (t *turn) unconnected(ctx context.Context, cause error) (Outcome, error) {
	code := models.CodeUnavailable
	if mc, ok := errors.AsType[*models.Coded](cause); ok {
		code = mc.Code
	}
	se, err := t.sessionError(code, cause.Error(), models.Retryable(cause), "")
	if err != nil {
		return Outcome{}, err
	}
	var stop *errStop
	if err := t.finish(ctx, session.StopError, code, se); !errors.As(err, &stop) {
		return Outcome{}, err
	}
	return stop.out, nil
}

// errStop ends a turn with an outcome that has been appended.
type errStop struct{ out Outcome }

func (e *errStop) Error() string { return "harness: the turn stopped: " + string(e.out.StopReason) }

// turn is one turn in progress.
type turn struct {
	h  *Harness
	s  session.Session
	sh *shared
	l  Log
	// root is the session's own registry; reg is this thread's, narrowed
	// from it, with the thread tools it is offered.
	root     *tools.Registry
	reg      *tools.Registry
	depth    int
	num      int
	step     int
	start    time.Time
	deadline time.Time
	startSeq uint64
	// seen is the last sequence the turn's latest request was built from.
	seen   uint64
	thread string

	continuations int
	// lastPrompt is the last response's prompt and output tokens, the
	// budget's estimate of the next request's input.
	lastPrompt int64
	// lastEstimate is the token estimate of the last request sent, and
	// bias how far the provider's count was from it.
	lastEstimate int64
	bias         int64
	// cut is set when an interrupt canceled the current step.
	cut atomic.Bool
	// model is the session's model as the turn runs it, its via the routed
	// name it was picked for, and switches the moves the turn made off a
	// model that could not serve (spec 051). A thread's turn holds neither:
	// it runs its own agent's model, which no router picked.
	model    session.ModelRef
	switches int
}

func (t *turn) event(typ session.Type, payload any) (session.Event, error) {
	e, err := session.NewEvent(typ, payload, t.h.c.Clock())
	if err != nil {
		return session.Event{}, err
	}
	e.Turn, e.Step, e.Thread = t.num, t.step, t.thread
	return e, nil
}

// commit appends a batch and folds in what others appended before it.
func (t *turn) commit(ctx context.Context, batch ...session.Event) error {
	t.sh.mu.Lock()
	defer t.sh.mu.Unlock()
	// Work that happened is always recorded, a cancel notwithstanding; a
	// lost lease still refuses the append.
	foreign, err := t.l.Append(context.WithoutCancel(ctx), batch)
	if err != nil {
		return &appendError{err}
	}
	t.sh.events = append(t.sh.events, foreign...)
	t.sh.events = append(t.sh.events, batch...)
	return nil
}

// shared is the session's log as the turns of every thread see it. A
// commit appends and records under one lock, so the events stay in
// sequence order while several threads run at once.
type shared struct {
	mu     sync.Mutex
	events []session.Event
	// running counts the threads running a turn now.
	running int
	// delivering serializes the threads' writes of a message's files,
	// and undeliverable holds the files this turn could not write.
	delivering    sync.Mutex
	undeliverable map[string]bool
}

// events is a snapshot of the log.
func (t *turn) events() []session.Event {
	t.sh.mu.Lock()
	defer t.sh.mu.Unlock()
	return slices.Clone(t.sh.events)
}

// finish appends the turn's closing session.status and returns the
// outcome as an errStop, so every path out of a step ends the same way.
func (t *turn) finish(ctx context.Context, reason session.StopReason, detail string, before ...session.Event) error {
	if t.thread != "" {
		// A thread's turn ends with the result of the call that drove it,
		// not with a status of the session.
		if len(before) > 0 {
			if err := t.commit(ctx, before...); err != nil {
				return err
			}
		}
		return &errStop{Outcome{Status: session.StatusIdle, StopReason: reason, Detail: detail}}
	}
	status := session.StatusIdle
	if t.s.EndOnIdle && reason == session.StopEndTurn {
		status, reason = session.StatusEnded, session.StopCompleted
	}
	cp, cerr := t.checkpoint(ctx)
	if cerr != nil {
		se, err := t.sessionError(CodeCheckpointFailed, cerr.Error(), true, "")
		if err != nil {
			return err
		}
		before = append(before, se)
	}
	e, err := t.event(session.TypeSessionStatus, session.SessionStatus{Status: status, StopReason: reason, Detail: detail, Checkpoint: cp})
	if err != nil {
		return err
	}
	if err := t.commit(ctx, append(before, e)...); err != nil {
		return err
	}
	return &errStop{Outcome{Status: status, StopReason: reason, Detail: detail, Pending: t.pending()}}
}

// CodeCheckpointFailed records a turn whose checkpoint could not be
// taken; the turn still ends.
const CodeCheckpointFailed = "checkpoint_failed"

// checkpoint takes the turn's checkpoint, chained to the thread's last
// one in the log.
func (t *turn) checkpoint(ctx context.Context) (*session.CheckpointRef, error) {
	if t.h.c.Checkpoint == nil {
		return nil, nil
	}
	return t.h.c.Checkpoint(context.WithoutCancel(ctx), t.num, LastCheckpoint(t.events(), t.thread))
}

// LastCheckpoint is the commit of the thread's latest checkpoint in the
// log, or empty.
func LastCheckpoint(log []session.Event, thread string) string {
	for _, e := range slices.Backward(log) {
		if e.Type != session.TypeSessionStatus || e.Thread != thread || e.Redacted() {
			continue
		}
		var p session.SessionStatus
		if e.Decode(&p) == nil && p.Checkpoint != nil {
			return p.Checkpoint.Commit
		}
	}
	return ""
}

// pending reports a person's event after the last sequence a request
// was built from, an answer to a question among them, and an interrupt
// that closed a question whose result is still owed: the runner claims
// again at once, and the claim closes the call.
func (t *turn) pending() bool {
	evs := t.events()
	for _, e := range evs {
		if e.Seq <= t.seen {
			continue
		}
		switch e.Type {
		case session.TypeUserMessage, session.TypeUserToolConfirmation, session.TypeUserToolResult, session.TypeUserAnswer:
			return true
		}
	}
	return session.Dismissed(evs)
}

func (t *turn) sessionError(code, message string, retryable bool, detail string) (session.Event, error) {
	return t.event(session.TypeSessionError, session.SessionError{Code: code, Message: message, Retryable: retryable, Detail: detail})
}

// appendError is a failed append: a lost lease or a store failure. It
// stops the turn at once with nothing more appended (spec 005).
type appendError struct{ err error }

func (e *appendError) Error() string { return "harness: append: " + e.err.Error() }
func (e *appendError) Unwrap() error { return e.err }

func (t *turn) run(ctx context.Context) (Outcome, error) {
	err := t.resume(ctx)
	for err == nil {
		err = t.stepOnce(ctx)
	}
	var stop *errStop
	var lost *appendError
	switch {
	case errors.As(err, &stop):
		return stop.out, nil
	case errors.As(err, &lost):
		return Outcome{}, err
	}
	// Every other way out of a turn still closes it, so the header never
	// stays running: a cancel as interrupted, a failure of the harness as
	// error with its session.error.
	closing := context.WithoutCancel(ctx)
	if ctx.Err() != nil {
		err = t.finish(closing, session.StopInterrupted, "canceled")
	} else {
		code := CodeInternal
		switch {
		case errors.Is(err, session.ErrSchemaTooNew):
			code = "schema_too_new"
		case errors.Is(err, session.ErrRedactionUncompacted):
			code = "redaction_uncompacted"
		}
		se, eerr := t.sessionError(code, err.Error(), false, "")
		if eerr != nil {
			return Outcome{}, errors.Join(err, eerr)
		}
		err = t.finish(closing, session.StopError, code, se)
	}
	if errors.As(err, &stop) {
		return stop.out, nil
	}
	return Outcome{}, err
}

// resume settles the calls the log left without a result (spec 005 step
// 2, spec 016's recovery), in three passes, so that a call a person
// answered is settled in the claim that reads the answer, whatever else
// of the step still waits (spec 012). First every call is settled that
// needs nothing run: a denied one is answered, by its confirmation or by
// the person's message that stood in for one, and a call no rule can
// repeat is closed as unknown_effect. Then the confirmed calls that no
// earlier runner could have started run, with the repeatable built-ins,
// and the threads the open spawn, message and advisor calls drove
// continue. Only then does the session go idle on what still waits: an
// ask nobody answered, a question nothing closed, a client's call, a
// thread's pause.
//
// A question call runs nothing, so it is settled in the first pass (spec
// 039): one an answer or a person's message closed gets its result, one
// an interrupt closed is canceled and the turn ends interrupted with no
// request, and one nothing closed keeps the session waiting, or, in a
// session nobody attends, is answered at once.
func (t *turn) resume(ctx context.Context) error {
	open := openCalls(t.events(), t.thread)
	var w waits
	var run, threads []pendingCall
	interrupted := false
	for _, c := range open {
		switch {
		case c.use.Client:
			w.result = true
		case isQuestion(c.use):
			by, err := t.settleQuestion(ctx, c.use.ToolUseID)
			if err != nil {
				return err
			}
			switch {
			case by == session.ClosedByInterrupt:
				interrupted = true
			case by != "":
			case t.s.Attended:
				w.question = true
			default:
				if err := t.result(ctx, c.use.ToolUseID, unattended(), 0); err != nil {
					return err
				}
			}
		case c.use.Verdict == string(VerdictAsk):
			if c.confirmation != nil {
				t.forwardAnswer(ctx, c)
			}
			switch {
			case c.message != nil:
				text := prompts.Render(prompts.CallDenied, prompts.Data{"Note": note(*c.message)})
				if err := t.result(ctx, c.use.ToolUseID, tools.Text(tools.OutcomeDenied, text), 0); err != nil {
					return err
				}
			case c.confirmation == nil:
				w.confirmation = true
			case c.confirmation.Decision == session.DecisionDeny:
				text := prompts.Render(prompts.CallDenied, prompts.Data{"Note": c.confirmation.Note})
				if err := t.result(ctx, c.use.ToolUseID, tools.Text(tools.OutcomeDenied, text), 0); err != nil {
					return err
				}
			case c.runsAfter > 1:
				if err := t.result(ctx, c.use.ToolUseID, unknownEffect(), 0); err != nil {
					return err
				}
			default:
				run = append(run, c)
			}
		case c.use.Name == ToolSpawn || c.use.Name == ToolMessage || c.use.Name == ToolAdvisor:
			threads = append(threads, c)
		case c.use.Repeatable:
			run = append(run, c)
		default:
			if err := t.result(ctx, c.use.ToolUseID, unknownEffect(), 0); err != nil {
				return err
			}
		}
	}
	calls := make([]plannedCall, 0, len(run))
	for _, c := range run {
		tool, ok := t.reg.Get(c.use.Name)
		if !ok {
			if err := t.result(ctx, c.use.ToolUseID, tools.Text(tools.OutcomeUnknownTool, prompts.Render(prompts.CallUnknownTool, prompts.Data{"Name": c.use.Name})), 0); err != nil {
				return err
			}
			continue
		}
		calls = append(calls, plannedCall{id: c.use.ToolUseID, tool: tool, input: c.use.Input})
	}
	if err := t.runCalls(ctx, calls, &w); err != nil {
		return t.callsStopped(ctx, err)
	}
	for _, c := range threads {
		res, err := t.resumeThread(ctx, c)
		if isPause(err) {
			w.paused(err)
			continue
		}
		if err != nil {
			return err
		}
		if err := t.result(ctx, c.use.ToolUseID, res, 0); err != nil {
			return err
		}
	}
	if interrupted {
		return t.finish(ctx, session.StopInterrupted, "")
	}
	if reason, waiting := w.reason(); waiting {
		return t.finish(ctx, reason, "")
	}
	return nil
}

func unknownEffect() tools.Result {
	return tools.Text(tools.OutcomeUnknownEffect, prompts.Text(prompts.CallUnknownEffect))
}

// stepOnce runs one step: the boundary checks, the request, the first
// commit point, and the calls.
func (t *turn) stepOnce(ctx context.Context) error {
	t.step++
	if t.interrupted() || t.signaled() {
		return t.finish(ctx, session.StopInterrupted, "")
	}
	if !t.h.c.Clock().Before(t.deadline) {
		return t.finish(ctx, session.StopTurnLimit, "")
	}
	evs := t.events()
	tr, err := session.Fold(evs, t.thread)
	if errors.Is(err, session.ErrRedactionUncompacted) {
		if err := t.compactRedaction(ctx); err != nil {
			return err
		}
		evs = t.events()
		tr, err = session.Fold(evs, t.thread)
	}
	if err != nil {
		return err
	}
	if err := tr.Check(); err != nil {
		return err
	}
	if len(evs) > 0 {
		t.seen = evs[len(evs)-1].Seq
	}
	if err := t.checkBudget(ctx); err != nil {
		return err
	}
	if err := t.deliverAttachments(ctx); err != nil {
		return err
	}
	req, toolsSHA, err := t.request(ctx, tr)
	if err != nil {
		return err
	}
	req, toolsSHA, err = t.manageContext(ctx, req, toolsSHA)
	if err != nil {
		return err
	}
	t.lastEstimate = tokencount.Estimate(&req)
	sctx, stop := t.interruptible(ctx)
	defer stop()
	for {
		limit := t.h.c.Entry.MaxOutputTokens
		began := t.h.c.Clock()
		moves := t.switchable()
		res, attempts, err := t.send(sctx, req, moves)
		si := sendInfo{maxTokens: *req.MaxTokens, toolsSHA: toolsSHA, attempts: attempts, latency: t.h.c.Clock().Sub(began)}
		if err != nil {
			if t.cut.Load() {
				return t.canceledRequest(ctx, si)
			}
			why := ""
			if moves && models.Down(err) {
				ferr := t.failover(sctx, err, si, res.RawResponse)
				var stay *stayed
				switch {
				case ferr == nil:
					// The step runs again on the model the turn moved to:
					// its request is built for that model's connection and
					// window, and its price is held to the budget again.
					if err := t.checkBudget(ctx); err != nil {
						return err
					}
					if req, toolsSHA, err = t.request(ctx, tr); err != nil {
						return err
					}
					if req, toolsSHA, err = t.manageContext(ctx, req, toolsSHA); err != nil {
						return err
					}
					t.lastEstimate = tokencount.Estimate(&req)
					continue
				case t.cut.Load():
					return t.canceledRequest(ctx, si)
				case errors.As(ferr, &stay):
					why = stay.why
				default:
					return ferr
				}
			}
			return t.modelFailed(ctx, err, si, res.RawResponse, why)
		}
		if res.StopReason != ir.StopMaxTokens || si.maxTokens >= limit || cutCall(res) != nil {
			return t.commitStep(sctx, res, si)
		}
		// The response stopped at the cap: the same request goes again
		// at the model's output limit, once, in its place.
		if err := t.escalate(sctx, res, si); err != nil {
			return err
		}
		req.MaxTokens = &limit
	}
}

// signaled reports whether the turn's interrupt channel is closed.
func (t *turn) signaled() bool {
	if t.h.c.Interrupt == nil {
		return false
	}
	select {
	case <-t.h.c.Interrupt():
		return true
	default:
		return false
	}
}

// interruptible is the context one step's request and calls run under:
// an interrupt cancels it and marks the step cut.
func (t *turn) interruptible(ctx context.Context) (context.Context, func()) {
	sctx, cancel := context.WithCancel(ctx)
	t.cut.Store(false)
	if t.h.c.Interrupt == nil {
		return sctx, cancel
	}
	ch := t.h.c.Interrupt()
	done := make(chan struct{})
	go func() {
		select {
		case <-ch:
			t.cut.Store(true)
			cancel()
		case <-done:
		}
	}()
	return sctx, func() {
		close(done)
		cancel()
	}
}

// canceledRequest records a request an interrupt cut short: its
// model.request with outcome canceled and no agent.message, and the turn
// stops interrupted.
func (t *turn) canceledRequest(ctx context.Context, si sendInfo) error {
	p := t.sentRequest(si)
	p.Outcome = "canceled"
	mr, err := t.event(session.TypeModelRequest, p)
	if err != nil {
		return err
	}
	return t.finish(ctx, session.StopInterrupted, "", mr)
}

// interrupted reports a user.interrupt appended since the turn began.
func (t *turn) interrupted() bool {
	for _, e := range t.events() {
		if e.Seq > t.startSeq && e.Type == session.TypeUserInterrupt {
			return true
		}
	}
	return false
}

// checkBudget is the pre-request check of spec 007: the spend so far
// plus the next request's input, priced at the input rate, against the
// session's ceiling.
func (t *turn) checkBudget(ctx context.Context) error {
	max := t.s.Budget.MaxCostUSDMicro
	if max == nil {
		return nil
	}
	est, err := models.EstimateInput(t.lastPrompt, t.h.c.Entry)
	if err != nil {
		e, eerr := t.sessionError(models.CodeUnpriced, "a budget applies and the model cannot be priced", false, "")
		if eerr != nil {
			return eerr
		}
		return t.finish(ctx, session.StopError, models.CodeUnpriced, e)
	}
	if session.Spent(t.events())+est >= *max {
		return t.finish(ctx, session.StopBudget, "")
	}
	return nil
}

// sendInfo is how one request was sent, as its model.request records
// it: the max_tokens it asked, the hash of its tool definitions, and the
// attempts it took and their time.
type sendInfo struct {
	maxTokens int64
	toolsSHA  string
	attempts  int
	latency   time.Duration
}

// sentRequest is the model.request of a sent request, before its
// response and outcome.
func (t *turn) sentRequest(si sendInfo) session.ModelRequest {
	c := t.h.c.Connection
	return session.ModelRequest{
		Model: c.Model, Family: c.Family, Dialect: string(c.EffectiveDialect()), PromptVersion: prompts.HarnessVersion(t.h.c.PromptVersion),
		ToolsSHA256: si.toolsSHA, MaxTokens: si.maxTokens, LatencyMS: si.latency.Milliseconds(), Attempts: si.attempts,
	}
}

// maxTokens is the max_tokens the thread's next request asks: OutputCap,
// or the model's output limit when that is lower, and the output limit
// while the turn continues a response that stopped at it.
func (t *turn) maxTokens() int64 {
	limit := t.h.c.Entry.MaxOutputTokens
	if t.continuations > 0 {
		return limit
	}
	return min(OutputCap, limit)
}

// request builds the step's IR request from the fold.
func (t *turn) request(ctx context.Context, tr session.Transcript) (ir.Request, string, error) {
	system, err := systemBlocks(ctx, t.h.prompt, t.h.c.Instructions, withRoute(withRepositories(tr.System, t.s), t.s), t.l)
	if err != nil {
		return ir.Request{}, "", err
	}
	defs := luxTools(t.reg.Definitions())
	sum, err := toolsHash(defs)
	if err != nil {
		return ir.Request{}, "", err
	}
	messages := tr.Messages
	if !t.h.c.Entry.Supports.Images {
		messages = withoutImages(messages)
	}
	req, err := buildRequest(requestParts{
		Model: t.h.c.Connection.Model, System: system, Messages: messages, Tools: defs,
		MaxTokens: t.maxTokens(), Effort: t.h.c.Effort, CacheKey: t.s.ID,
		ReasoningReplay: t.h.c.Connection.EffectiveDialect() == ir.DialectOpenAIResponses,
	})
	return req, sum, err
}

// send streams the request with retry: a retryable failure waits the
// policy's delay, raised to the server's Retry-After, and a wait that
// would pass the turn deadline ends the attempts. A model that cannot
// serve now (models.Down) is not retried when the caller moves the turn
// to another model on such a failure, which moves says, and is retried
// by DownRetry otherwise (spec 051). A failure returns the last attempt's
// result, which holds the bytes it received.
func (t *turn) send(ctx context.Context, req ir.Request, moves bool) (models.Result, int, error) {
	policy := t.h.c.Retry
	downs := 0
	for attempt := 1; ; attempt++ {
		res, err := t.stream(ctx, req)
		if err == nil {
			return res, attempt, nil
		}
		var d time.Duration
		switch {
		case models.Down(err):
			downs++
			d = DownRetry.Delay(downs)
			if moves || downs >= DownRetry.Attempts() || models.RetryAfter(err) > d {
				return res, attempt, err
			}
		case !models.Retryable(err) || attempt >= policy.Attempts():
			return res, attempt, err
		default:
			d = max(policy.Delay(attempt), models.RetryAfter(err))
		}
		if o := t.h.c.Observer; o != nil {
			o.OnReset(t.thread, t.num, t.step)
		}
		if !t.h.c.Clock().Add(d).Before(t.deadline) {
			return res, attempt, err
		}
		if err := t.h.c.Sleep(ctx, d); err != nil {
			return res, attempt, err
		}
	}
}

func (t *turn) stream(ctx context.Context, req ir.Request) (models.Result, error) {
	s, err := t.h.c.Model.Stream(ctx, models.Request{IR: req, Connection: t.h.c.Connection, Capture: t.s.Capture.Requests})
	if err != nil {
		return models.Result{}, err
	}
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return s.Result(), s.Close()
		}
		if err != nil {
			return s.Result(), errors.Join(err, s.Close())
		}
		t.startMachine(ev)
		if o := t.h.c.Observer; o != nil {
			o.OnDelta(Delta{Thread: t.thread, Turn: t.num, Step: t.step, Event: ev})
		}
	}
}

// startMachine starts a machine opened on demand when the response begins
// a call of a tool that acts on it (spec 048): the block's start names
// the tool before its arguments stream, so the machine comes up while
// they do, and a response that calls no such tool starts none. A name
// this thread's registry does not hold starts nothing; the call is
// refused as unknown before anything would run on the machine.
func (t *turn) startMachine(ev ir.Event) {
	if ev.Type != ir.EventBlockStart || ev.Block == nil || ev.Block.ToolUse == nil {
		return
	}
	if tool, ok := t.reg.Get(ev.Block.ToolUse.Name); ok && opensMachine(tool.Properties()) {
		machine.Start(t.h.c.Machine)
	}
}

// opensMachine reports whether a call of a tool with these properties
// acts on the machine, which the harness opens before running it: every
// tool with an effect, and no tool a client runs, since that call never
// runs here.
func opensMachine(p tools.Properties) bool {
	return !p.Client && p.Effect != tools.EffectNone
}

// modelFailed records a request that failed after its attempts and ends
// the turn with error, keeping every earlier event of the turn. The
// bytes the last attempt received, raw, are its response blob, so a
// stream that failed part way keeps what the model sent. A model that
// could not serve now ends the turn with CodeModelBusy and its one
// sentence (spec 051), the gateway's answer and why, what kept the turn
// from moving to another model, in the detail.
func (t *turn) modelFailed(ctx context.Context, err error, si sendInfo, raw []byte, why string) error {
	if ctx.Err() != nil {
		return err
	}
	mr, eerr := t.failedRequest(ctx, err, si, raw)
	if eerr != nil {
		return eerr
	}
	detail := httpDetail(err)
	if code, spent := models.SpendRefused(err); spent {
		// The gateway's budget for this caller is spent: the turn stops
		// with budget, which a message resumes once the budget is raised.
		se, eerr := t.sessionError(code, err.Error(), false, detail)
		if eerr != nil {
			return eerr
		}
		return t.finish(ctx, session.StopBudget, code, mr, se)
	}
	if models.Down(err) {
		detail = models.Described(err)
		if why != "" {
			detail += "; " + why
		}
		se, eerr := t.sessionError(CodeModelBusy, MessageModelBusy, true, detail)
		if eerr != nil {
			return eerr
		}
		return t.finish(ctx, session.StopError, CodeModelBusy, mr, se)
	}
	se, eerr := t.sessionError(CodeModelError, err.Error(), models.Retryable(err), detail)
	if eerr != nil {
		return eerr
	}
	return t.finish(ctx, session.StopError, CodeModelError, mr, se)
}

// failedRequest is the model.request of a request that failed after its
// attempts, outcome error, with the bytes the last attempt received as
// its response blob.
func (t *turn) failedRequest(ctx context.Context, err error, si sendInfo, raw []byte) (session.Event, error) {
	p := t.sentRequest(si)
	p.Outcome, p.Error = "error", models.Described(err)
	if len(raw) > 0 {
		blob, perr := t.l.PutBlob(ctx, bytes.NewReader(raw))
		if perr != nil {
			return session.Event{}, errors.Join(err, fmt.Errorf("harness: store the failed response: %w", perr))
		}
		p.ResponseBlob = blob
	}
	return t.event(session.TypeModelRequest, p)
}

// httpDetail is a model server's error answer as a session.error's
// detail, its status and type, and "" for another failure.
func httpDetail(err error) string {
	var he *models.HTTPError
	if errors.As(err, &he) {
		return fmt.Sprintf("HTTP %d %s", he.Status, he.Type)
	}
	return ""
}

// switchable reports whether a failure of the turn's model to serve now
// moves the turn to another model (spec 051): the session's own thread,
// on a routed name, with a router to ask and moves left.
func (t *turn) switchable() bool {
	return t.thread == "" && t.h.c.Failover != nil && t.model.Via != "" && t.model.Name != "" && t.switches < MaxModelSwitches
}

// failover moves the turn off a model that could not serve now (spec
// 051). It asks Config.Failover, connects the model answered as a switch
// between turns connects one, and records in one batch the failed
// request and the session.model_changed the service made, with
// session.ReasonModelBusy; the step then sends its request again on the
// new model, at the level answered. It is a *stayed, with nothing
// recorded, when the question fails, the answer names no other model, or
// the model answered cannot be connected; any other error is a failure to
// record, which stops the turn at once.
func (t *turn) failover(ctx context.Context, cause error, si sendInfo, raw []byte) error {
	next, err := t.h.c.Failover(ctx, t.model, models.GatewayDetail(cause))
	switch {
	case err != nil:
		return &stayed{why: "no other model was named: " + err.Error()}
	case next.Name == "" || next.Name == t.model.Name:
		return &stayed{why: "no other model was named"}
	}
	scoped := *t.h
	if err := scoped.on(ctx, next.Name); err != nil {
		return &stayed{why: fmt.Sprintf("%s was named and could not be connected: %v", next.Name, err)}
	}
	scoped.c.Effort = next.Level()
	mr, err := t.failedRequest(ctx, cause, si, raw)
	if err != nil {
		return err
	}
	change, err := t.event(session.TypeModelChanged, session.ModelChanged{
		By: session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService}, Old: t.model, New: next,
		Reason: session.ReasonModelBusy, Detail: models.Described(cause),
	})
	if err != nil {
		return err
	}
	if err := t.commit(ctx, mr, change); err != nil {
		return err
	}
	t.h, t.model = &scoped, next
	t.switches++
	if o := t.h.c.Observer; o != nil {
		o.OnReset(t.thread, t.num, t.step)
	}
	return nil
}

// stayed is a failover that left the turn on its model (spec 051): why
// says what kept it there, for the turn's error.
type stayed struct{ why string }

func (s *stayed) Error() string { return "harness: the turn stayed on its model: " + s.why }

// commitStep is commit point one and what follows it: model.request,
// agent.message and the agent.tool_use of every valid call are durable
// before any call runs.
func (t *turn) commitStep(ctx context.Context, res models.Result, si sendInfo) error {
	mrEvent, err := t.modelRequest(ctx, res, si, "ok")
	if err != nil {
		return err
	}
	// A response cut inside a call's arguments is not continued: the
	// step ends with the call answered, as cutCall says.
	truncated := res.StopReason == ir.StopMaxTokens && cutCall(res) == nil
	msg := session.AgentMessage{Message: res.Message, StopReason: res.StopReason, Request: mrEvent.ID, Truncated: truncated}
	if t.continuations > 0 {
		msg.ContinuationOf = t.lastMessage()
	}
	msgEvent, err := t.event(session.TypeAgentMessage, msg)
	if err != nil {
		return err
	}
	batch := []session.Event{mrEvent, msgEvent}
	if truncated {
		t.continuations++
		if t.continuations > MaxContinuations {
			se, err := t.sessionError(CodeOutputTruncated, fmt.Sprintf("the response stopped at the output limit %d times in a row", MaxContinuations+1), true, "")
			if err != nil {
				return err
			}
			return t.finish(ctx, session.StopOutputLimit, "", append(batch, se)...)
		}
		return t.commit(ctx, batch...)
	}
	t.continuations = 0

	planned, answered, extra, err := t.plan(ctx, res, si.maxTokens)
	if err != nil {
		return err
	}
	batch = append(batch, extra...)
	if err := t.commit(ctx, batch...); err != nil {
		return err
	}
	for _, a := range answered {
		if err := t.result(ctx, a.id, a.res, 0); err != nil {
			return err
		}
	}
	if len(planned.run) == 0 && len(planned.ask) == 0 && len(planned.client) == 0 && len(answered) == 0 {
		detail := ""
		if res.StopReason == ir.StopRefusal {
			detail = "refusal"
		}
		return t.finish(ctx, session.StopEndTurn, detail)
	}
	// A call that waits for a person leaves the step's other calls to
	// run, and the step goes idle once they have: on an ask, on a client's
	// call, on a question, or on the pause a thread's call returned.
	w := waits{confirmation: len(planned.ask) > 0, result: len(planned.client) > 0}
	if err := t.runCalls(ctx, planned.run, &w); err != nil {
		return t.callsStopped(ctx, err)
	}
	interrupted := t.cut.Load()
	if w.question {
		// What closes a question counts from its agent.tool_use, so an
		// answer, a message or an interrupt that arrived while the step's
		// other calls ran has closed it already: the call gets its result
		// here, and the session does not go idle on it.
		by, err := t.settleQuestion(ctx, planned.question)
		if err != nil {
			return err
		}
		w.question = by == ""
		interrupted = interrupted || by == session.ClosedByInterrupt
	}
	if interrupted {
		return t.finish(ctx, session.StopInterrupted, "")
	}
	if reason, waiting := w.reason(); waiting {
		return t.finish(ctx, reason, "")
	}
	return nil
}

// cutCall is the call a response that stopped at max_tokens was cut
// inside: its last block, when that is a call whose arguments are not
// one JSON value. Such a response is neither sent again at the output
// limit nor continued, as a model that runs away inside an argument
// runs on to any limit; the step ends as one with an invalid call, the
// call answered and the model asked to send it again at the cap. A
// response cut in text or thinking, or between calls, has no cut call.
func cutCall(res models.Result) *lux.ToolUse {
	n := len(res.Message.Blocks)
	if res.StopReason != ir.StopMaxTokens || n == 0 {
		return nil
	}
	last := res.Message.Blocks[n-1]
	if last.Type != ir.BlockToolUse || last.ToolUse == nil {
		return nil
	}
	if _, broken := res.InvalidArgs[last.ToolUse.ID]; !broken {
		return nil
	}
	return last.ToolUse
}

// escalate records a response that stopped at a max_tokens below the
// model's output limit, before its request is sent again at the limit:
// its model.request with outcome escalated, so its cost is spent, and no
// agent.message, so the fold never sees the partial response and the
// request sent again is not a continuation. The Observer discards the
// step's partial output.
func (t *turn) escalate(ctx context.Context, res models.Result, si sendInfo) error {
	mr, err := t.modelRequest(ctx, res, si, "escalated")
	if err != nil {
		return err
	}
	if err := t.commit(ctx, mr); err != nil {
		return err
	}
	if o := t.h.c.Observer; o != nil {
		o.OnReset(t.thread, t.num, t.step)
	}
	return nil
}

// modelRequest stores a response's raw body (and its request, while
// capture is on) and returns the model.request event that records it,
// with its cost and outcome. It also calibrates the next request's
// estimate.
func (t *turn) modelRequest(ctx context.Context, res models.Result, si sendInfo, outcome string) (session.Event, error) {
	respBlob, err := t.l.PutBlob(ctx, bytes.NewReader(res.RawResponse))
	if err != nil {
		return session.Event{}, fmt.Errorf("harness: store the response: %w", err)
	}
	var reqBlob session.Digest
	if t.s.Capture.Requests && res.RequestBytes != nil {
		if reqBlob, err = t.l.PutBlob(ctx, bytes.NewReader(res.RequestBytes)); err != nil {
			return session.Event{}, fmt.Errorf("harness: store the request: %w", err)
		}
	}
	mr := t.sentRequest(si)
	mr.Codec, mr.RequestSHA256, mr.RequestBytes = res.Codec, res.RequestSHA256, res.RequestSize
	mr.FoldSeq, mr.RequestBlob, mr.ResponseBlob = t.seen, reqBlob, respBlob
	mr.Usage, mr.FirstTokenMS, mr.StopReason, mr.Loss = &res.Usage, res.FirstToken.Milliseconds(), res.StopReason, res.Loss
	mr.Outcome = outcome
	if cost, src, err := models.Cost(models.FromLux(res.Usage), t.h.c.Entry); err == nil {
		mr.CostUSDMicro, mr.CostSource = &cost, src
	}
	prompt := promptTokens(res.Usage)
	t.lastPrompt = prompt + res.Usage.OutputTokens
	if t.lastEstimate > 0 {
		t.bias = prompt - t.lastEstimate
	}
	return t.event(session.TypeModelRequest, mr)
}

// lastMessage is the id of the thread's latest agent.message.
func (t *turn) lastMessage() string {
	for _, e := range slices.Backward(t.events()) {
		if e.Type == session.TypeAgentMessage && e.Thread == t.thread {
			return e.ID
		}
	}
	return ""
}

type plannedCall struct {
	id    string
	tool  tools.Tool
	input []byte
}

type answeredCall struct {
	id  string
	res tools.Result
}

type stepPlan struct {
	run    []plannedCall
	ask    []string
	client []string
	// question is the tool_use id of the step's question call, when it
	// has one that runs.
	question string
}

// plan validates, scores and decides each call of the response. Valid
// calls get an agent.tool_use for the batch; invalid and blocked ones an
// answer appended after it. A call whose arguments were not JSON is
// validated against the text the model sent, not the {} the log holds
// for it, so its answer names what was wrong, and the call the output
// limit, limit tokens, cut off is answered as cut.
func (t *turn) plan(ctx context.Context, res models.Result, limit int64) (stepPlan, []answeredCall, []session.Event, error) {
	var p stepPlan
	var answered []answeredCall
	var uses []session.Event
	remembered := rememberedPatterns(t.events())
	kind := t.h.c.Machine.Info().Kind
	// The step's calls are decided under the mode in force as the step
	// plans them, a person's change included (spec 041).
	mode := t.mode()
	decider := t.h.decider()
	// asked reports that the step already holds a question call that is
	// asked; each later one is refused.
	asked := false
	for _, b := range res.Message.Blocks {
		if b.Type != ir.BlockToolUse || b.ToolUse == nil {
			continue
		}
		id, name, input := b.ToolUse.ID, b.ToolUse.Name, []byte(b.ToolUse.Args)
		if raw, broken := res.InvalidArgs[id]; broken {
			input = []byte(raw)
		}
		if cut := cutCall(res); cut != nil && cut.ID == id {
			answered = append(answered, answeredCall{id, tools.CutInput(name, input, limit)})
			continue
		}
		tool, bad := t.reg.Validate(name, input)
		if bad != nil {
			answered = append(answered, answeredCall{id, *bad})
			continue
		}
		props := tool.Properties()
		policy := t.h.c.Policy
		policy.Mode = mode
		_, question := tool.(questionTool)
		if question {
			// The rules of a question the schema cannot state are checked
			// here, before the call is recorded, so an agent.tool_use of
			// the question tool always holds questions a person can be
			// shown.
			if bad := checkQuestion(input, asked); bad != nil {
				answered = append(answered, answeredCall{id, *bad})
				continue
			}
			asked, policy = true, withoutConfirm(policy)
		}
		risk, d, err := decider.Decide(ctx, Call{
			Policy: policy, Session: t.s, ToolUseID: id, Name: name, Props: props, Input: input, MachineKind: kind, Remembered: remembered,
		})
		if err != nil {
			return stepPlan{}, nil, nil, err
		}
		if question && d.Verdict == VerdictAsk {
			// A decider that asks all the same is overruled: the call is
			// itself put to a person, and is never held for a confirmation.
			d = Decision{Verdict: VerdictAllow, Reason: "a question is itself put to a person"}
		}
		if d.Verdict.Shown() && !(d.ReviewProbability > 0) {
			d.ReviewProbability = 1
		}
		review := d.ReviewProbability
		use := session.AgentToolUse{
			ToolUseID: id, Name: name, Input: input, Risk: &risk, Verdict: string(d.Verdict), Reason: d.Reason,
			Mode: string(mode), Client: props.Client, Repeatable: props.Repeatable,
			ReviewProbability: &review, Draw: d.Draw, Suggestion: d.Suggestion,
		}
		e, err := t.event(session.TypeAgentToolUse, use)
		if err != nil {
			return stepPlan{}, nil, nil, err
		}
		uses = append(uses, e)
		switch {
		case d.Verdict == VerdictBlock:
			answered = append(answered, answeredCall{id, tools.Text(tools.OutcomeBlocked, d.Reason)})
		case d.Verdict == VerdictAsk:
			p.ask = append(p.ask, id)
		case props.Client:
			p.client = append(p.client, id)
		default:
			p.run = append(p.run, plannedCall{id: id, tool: tool, input: input})
			if question {
				p.question = id
			}
		}
	}
	return p, answered, uses, nil
}

// runCalls runs a step's calls: each maximal run of consecutive parallel
// tools concurrently, at most MaxParallel at once, every other call alone
// in order. Each result is appended as its call returns. A call that
// waits for a person, a thread's or a question, keeps no result and
// leaves the rest of the step's calls to run: its wait is recorded in w,
// each of them, since several may wait at once. A call a core refused
// for spend leaves the rest to run too, and is returned after them.
func (t *turn) runCalls(ctx context.Context, calls []plannedCall, w *waits) error {
	var spent error
	keep := func(err error) error {
		switch {
		case isSpent(err):
			spent = err
		case isPause(err):
			w.paused(err)
		default:
			return err
		}
		return nil
	}
	for i := 0; i < len(calls); {
		if !calls[i].tool.Properties().Parallel {
			if err := t.call(ctx, calls[i]); err != nil {
				if err := keep(err); err != nil {
					return err
				}
			}
			i++
			continue
		}
		j := i
		for j < len(calls) && calls[j].tool.Properties().Parallel {
			j++
		}
		if err := t.parallel(ctx, calls[i:j], w); err != nil {
			if err := keep(err); err != nil {
				return err
			}
		}
		i = j
	}
	return spent
}

// callsStopped ends a step whose calls stopped it with an error that is
// not a pause: a core's refusal for spend ends it with budget and a
// session.error naming the refusal, as a model gateway's refusal does
// (spec 007). Any other error is returned.
func (t *turn) callsStopped(ctx context.Context, err error) error {
	code, spent := models.SpendRefused(err)
	if !spent {
		return err
	}
	var detail string
	if se, ok := errors.AsType[*models.SpendError](err); ok {
		detail = se.Core
	}
	e, eerr := t.sessionError(code, err.Error(), false, detail)
	if eerr != nil {
		return eerr
	}
	return t.finish(ctx, session.StopBudget, code, e)
}

func isPause(err error) bool {
	var p *errPause
	return errors.As(err, &p)
}

// isSpent reports whether a call's error is a core's refusal for spend.
func isSpent(err error) bool {
	_, spent := models.SpendRefused(err)
	return spent
}

// pauseReason is the stop reason a paused call leaves the step with.
func pauseReason(err error) session.StopReason {
	var p *errPause
	if errors.As(err, &p) {
		return p.reason
	}
	return ""
}

func (t *turn) parallel(ctx context.Context, calls []plannedCall, w *waits) error {
	type done struct {
		id  string
		res tools.Result
		err error
		dur time.Duration
	}
	results := make(chan done, len(calls))
	sem := make(chan struct{}, MaxParallel)
	state := t.toolState()
	for _, c := range calls {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			start := t.h.c.Clock()
			res, err := t.execute(ctx, c, state)
			results <- done{c.id, res, err, t.h.c.Clock().Sub(start)}
		}()
	}
	var errs []error
	var spent error
	for range calls {
		d := <-results
		switch {
		case isPause(d.err):
			w.paused(d.err)
		case d.err != nil && !isSpent(d.err):
			errs = append(errs, d.err)
		case len(errs) == 0:
			if err := t.result(ctx, d.id, d.res, d.dur); err != nil {
				errs = append(errs, err)
			}
			if d.err != nil {
				spent = d.err
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return spent
}

// toolState is what the thread's tools know from the log. A fork's first
// machine is its own (spec 017), so bash does not start in a directory
// its copied log names: that one was reported on its parent's machine.
func (t *turn) toolState() tools.State {
	st := tools.StateOf(t.events(), t.thread)
	if st.Dir != "" && t.s.Parent != nil && st.DirSeq <= t.s.Parent.Seq {
		st.Dir = ""
	}
	return st
}

func (t *turn) call(ctx context.Context, c plannedCall) error {
	start := t.h.c.Clock()
	res, err := t.execute(ctx, c, t.toolState())
	if err != nil && !isSpent(err) {
		return err
	}
	if rerr := t.result(ctx, c.id, res, t.h.c.Clock().Sub(start)); rerr != nil {
		return rerr
	}
	return err
}

// execute runs one call. A tool's Go error is a failure of the tool
// side and becomes the outcome error with its message; a call a cancel
// cut short is canceled. Two errors pass through instead: a thread that
// waits for a person, whose driving call keeps no result, and a failed
// append inside a thread's turn, which stops the turn. A core's refusal
// for spend is both: the call's result says it was refused, and the
// refusal returns beside it to stop the turn once the step's calls are
// answered.
func (t *turn) execute(ctx context.Context, c plannedCall, state tools.State) (tools.Result, error) {
	// A tool that acts on the machine opens a machine opened on demand
	// first, so its paths resolve against the working directory of the
	// machine that exists; a tool of no effect never opens one.
	if opensMachine(c.tool.Properties()) {
		if err := machine.Open(ctx, t.h.c.Machine); err != nil {
			return t.unopened(ctx, err)
		}
	}
	res, err := c.tool.Run(ctx, tools.Call{ID: c.id, Input: c.input, Machine: t.h.c.Machine, State: state})
	var lost *appendError
	if isPause(err) || errors.As(err, &lost) {
		return tools.Result{}, err
	}
	if isSpent(err) {
		return settle(ctx, res, err), err
	}
	return settle(ctx, res, err), nil
}

// unopened answers a call whose machine could not be opened. A core's
// refusal for spend stops the turn with budget as a tool's does; any
// other failure is appended as a session.error with its code and is the
// call's result, so the model can go on without the machine or try
// again.
func (t *turn) unopened(ctx context.Context, err error) (tools.Result, error) {
	if isSpent(err) {
		return settle(ctx, tools.Result{}, err), err
	}
	code := machine.CodeUnavailable
	if oe, ok := errors.AsType[*machine.OpenError](err); ok {
		code = oe.Code
	}
	e, eerr := t.sessionError(code, err.Error(), false, "")
	if eerr != nil {
		return tools.Result{}, eerr
	}
	if cerr := t.commit(ctx, e); cerr != nil {
		return tools.Result{}, cerr
	}
	return tools.Text(tools.OutcomeError, prompts.Render(prompts.CallMachineUnavailable, prompts.Data{"Error": err.Error()})), nil
}

// settle turns what a tool returned into the result the model sees: a
// cancel that cut the call short is canceled, a tool's own error is the
// outcome error with its message, and anything else keeps its outcome.
func settle(ctx context.Context, res tools.Result, err error) tools.Result {
	switch {
	case ctx.Err() != nil && (err != nil || len(res.Content) == 0):
		return tools.Text(tools.OutcomeCanceled, prompts.Text(prompts.CallCanceled))
	case err != nil:
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.CallFailed, prompts.Data{"Error": err.Error()}))
	}
	if res.Outcome == "" {
		res.Outcome = tools.OutcomeOK
	}
	return res
}

// result appends one tool.result, commit point two.
func (t *turn) result(ctx context.Context, id string, res tools.Result, dur time.Duration) error {
	p := session.ToolResult{ToolUseID: id, Content: res.Content, IsError: res.IsError(), Outcome: res.Outcome, DurationMS: dur.Milliseconds(), Spill: res.Spill, CostUSDMicro: res.CostUSDMicro}
	if res.Meta != nil {
		b, err := session.Marshal(res.Meta)
		if err != nil {
			return fmt.Errorf("harness: encode the result's meta: %w", err)
		}
		p.Meta = b
	}
	e, err := t.event(session.TypeToolResult, p)
	if err != nil {
		return err
	}
	return t.commit(context.WithoutCancel(ctx), e)
}
