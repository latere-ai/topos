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
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/tokencount"
	"latere.ai/x/pkg/retry"

	"latere.ai/x/topos/harness/prompt"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
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

// MaxContinuations is how many truncated responses in a row a turn
// continues before it ends with output_limit.
const MaxContinuations = 2

// MaxParallel is how many parallel calls of a step run at once.
const MaxParallel = 8

// Config is what a harness runs with.
type Config struct {
	Model      models.Model
	Connection models.Connection
	// Entry is the model's resolved catalog figures; its output limit is
	// every request's max_tokens.
	Entry   models.Entry
	Machine machine.Machine
	Tools   *tools.Registry
	Policy  Policy
	// Instructions are the agent's own instructions.
	Instructions string
	Effort       string
	// PromptVersion is the harness prompt version; zero is the current.
	PromptVersion int
	Prompt        prompt.Options
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
	// Sleep waits between attempts; nil waits on a timer.
	Sleep    func(ctx context.Context, d time.Duration) error
	Observer Observer
}

// Harness runs turns of one agent.
type Harness struct {
	c      Config
	prompt string
}

// The error codes of spec 005.
const (
	CodeModelError      = "model_error"
	CodeOutputTruncated = "output_truncated"
	CodeInternal        = "internal"
)

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
		c.PromptVersion = prompt.Current
	}
	return &Harness{c: c, prompt: p}, nil
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
	t := &turn{h: h, s: s, log: append([]session.Event(nil), log...), l: l, num: s.Turn + 1, start: h.c.Clock()}
	if len(t.log) > 0 {
		t.startSeq = t.log[len(t.log)-1].Seq
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

// errStop ends a turn with an outcome that has been appended.
type errStop struct{ out Outcome }

func (e *errStop) Error() string { return "harness: the turn stopped: " + string(e.out.StopReason) }

// turn is one turn in progress.
type turn struct {
	h        *Harness
	s        session.Session
	log      []session.Event
	l        Log
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
	foreign, err := t.l.Append(ctx, batch)
	if err != nil {
		return &appendError{err}
	}
	t.log = append(t.log, foreign...)
	t.log = append(t.log, batch...)
	return nil
}

// finish appends the turn's closing session.status and returns the
// outcome as an errStop, so every path out of a step ends the same way.
func (t *turn) finish(ctx context.Context, reason session.StopReason, detail string, before ...session.Event) error {
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
	return t.h.c.Checkpoint(context.WithoutCancel(ctx), t.num, LastCheckpoint(t.log, t.thread))
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
// was built from.
func (t *turn) pending() bool {
	for _, e := range t.log {
		if e.Seq <= t.seen {
			continue
		}
		switch e.Type {
		case session.TypeUserMessage, session.TypeUserToolConfirmation, session.TypeUserToolResult:
			return true
		}
	}
	return false
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
// 2, spec 016's recovery): a confirmed call that no earlier runner
// could have started runs, a denied one is answered, an unanswered ask
// or client call keeps the session waiting, a repeatable built-in runs
// again, and any other call is closed as unknown_effect.
func (t *turn) resume(ctx context.Context) error {
	open := openCalls(t.log, t.thread)
	var waitAsk, waitClient bool
	var run []pendingCall
	for _, c := range open {
		switch {
		case c.use.Client:
			waitClient = true
		case c.use.Verdict == string(VerdictAsk):
			switch {
			case c.confirmation == nil:
				waitAsk = true
			case c.confirmation.Decision == session.DecisionDeny:
				text := "A person denied this call."
				if c.confirmation.Note != "" {
					text += " Their note: " + c.confirmation.Note
				}
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
		case c.use.Repeatable:
			run = append(run, c)
		default:
			if err := t.result(ctx, c.use.ToolUseID, unknownEffect(), 0); err != nil {
				return err
			}
		}
	}
	if waitAsk {
		return t.finish(ctx, session.StopToolConfirmation, "")
	}
	if waitClient {
		return t.finish(ctx, session.StopToolResult, "")
	}
	calls := make([]plannedCall, 0, len(run))
	for _, c := range run {
		tool, ok := t.h.c.Tools.Get(c.use.Name)
		if !ok {
			if err := t.result(ctx, c.use.ToolUseID, tools.Text(tools.OutcomeUnknownTool, "No tool named "+c.use.Name+"."), 0); err != nil {
				return err
			}
			continue
		}
		calls = append(calls, plannedCall{id: c.use.ToolUseID, tool: tool, input: c.use.Input})
	}
	return t.runCalls(ctx, calls)
}

func unknownEffect() tools.Result {
	return tools.Text(tools.OutcomeUnknownEffect, "The runner stopped while this call ran. Its effects are unknown; inspect the machine before repeating it.")
}

// stepOnce runs one step: the boundary checks, the request, the first
// commit point, and the calls.
func (t *turn) stepOnce(ctx context.Context) error {
	t.step++
	if t.interrupted() {
		return t.finish(ctx, session.StopInterrupted, "")
	}
	if !t.h.c.Clock().Before(t.deadline) {
		return t.finish(ctx, session.StopTurnLimit, "")
	}
	tr, err := session.Fold(t.log, t.thread)
	if err != nil {
		return err
	}
	if err := tr.Check(); err != nil {
		return err
	}
	if n := len(t.log); n > 0 {
		t.seen = t.log[n-1].Seq
	}
	if err := t.checkBudget(ctx); err != nil {
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
	sent := t.h.c.Clock()
	res, attempts, err := t.send(ctx, req)
	latency := t.h.c.Clock().Sub(sent)
	if err != nil {
		return t.modelFailed(ctx, err, attempts, toolsSHA, latency)
	}
	return t.commitStep(ctx, res, attempts, toolsSHA, latency)
}

// interrupted reports a user.interrupt appended since the turn began.
func (t *turn) interrupted() bool {
	for _, e := range t.log {
		if e.Seq > t.startSeq && e.Type == session.TypeUserInterrupt {
			return true
		}
	}
	return false
}

// spent is the budget meter: the cost of every model.request of every
// thread.
func spent(log []session.Event) int64 {
	var total int64
	for _, e := range log {
		if e.Type != session.TypeModelRequest || e.Redacted() {
			continue
		}
		var p session.ModelRequest
		if e.Decode(&p) == nil && p.CostUSDMicro != nil {
			total += *p.CostUSDMicro
		}
	}
	return total
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
	if spent(t.log)+est >= *max {
		return t.finish(ctx, session.StopBudget, "")
	}
	return nil
}

// request builds the step's IR request from the fold.
func (t *turn) request(ctx context.Context, tr session.Transcript) (ir.Request, string, error) {
	system, err := systemBlocks(ctx, t.h.prompt, t.h.c.Instructions, tr.System, t.l)
	if err != nil {
		return ir.Request{}, "", err
	}
	defs := luxTools(t.h.c.Tools.Definitions())
	sum, err := toolsHash(defs)
	if err != nil {
		return ir.Request{}, "", err
	}
	req, err := buildRequest(requestParts{
		Model: t.h.c.Connection.Model, System: system, Messages: tr.Messages, Tools: defs,
		MaxTokens: t.h.c.Entry.MaxOutputTokens, Effort: t.h.c.Effort, CacheKey: t.s.ID,
	})
	return req, sum, err
}

// send streams the request with retry: a retryable failure waits the
// policy's delay, raised to the server's Retry-After, and a wait that
// would pass the turn deadline ends the attempts.
func (t *turn) send(ctx context.Context, req ir.Request) (models.Result, int, error) {
	policy := t.h.c.Retry
	for attempt := 1; ; attempt++ {
		res, err := t.stream(ctx, req)
		if err == nil {
			return res, attempt, nil
		}
		if !models.Retryable(err) || attempt >= policy.Attempts() {
			return models.Result{}, attempt, err
		}
		if o := t.h.c.Observer; o != nil {
			o.OnReset(t.thread, t.num, t.step)
		}
		d := max(policy.Delay(attempt), models.RetryAfter(err))
		if !t.h.c.Clock().Add(d).Before(t.deadline) {
			return models.Result{}, attempt, err
		}
		if err := t.h.c.Sleep(ctx, d); err != nil {
			return models.Result{}, attempt, err
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
			return models.Result{}, errors.Join(err, s.Close())
		}
		if o := t.h.c.Observer; o != nil {
			o.OnDelta(Delta{Thread: t.thread, Turn: t.num, Step: t.step, Event: ev})
		}
	}
}

// modelFailed records a request that failed after its attempts and ends
// the turn with error, keeping every earlier event of the turn.
func (t *turn) modelFailed(ctx context.Context, err error, attempts int, toolsSHA string, latency time.Duration) error {
	if ctx.Err() != nil {
		return err
	}
	c := t.h.c.Connection
	mr, eerr := t.event(session.TypeModelRequest, session.ModelRequest{
		Model: c.Model, Family: c.Family, Dialect: string(c.EffectiveDialect()), PromptVersion: prompt.Version(t.h.c.PromptVersion),
		ToolsSHA256: toolsSHA, LatencyMS: latency.Milliseconds(), Attempts: attempts, Outcome: "error", Error: err.Error(),
	})
	if eerr != nil {
		return eerr
	}
	var detail string
	var he *models.HTTPError
	if errors.As(err, &he) {
		detail = fmt.Sprintf("HTTP %d %s", he.Status, he.Type)
	}
	se, eerr := t.sessionError(CodeModelError, err.Error(), models.Retryable(err), detail)
	if eerr != nil {
		return eerr
	}
	return t.finish(ctx, session.StopError, CodeModelError, mr, se)
}

// commitStep is commit point one and what follows it: model.request,
// agent.message and the agent.tool_use of every valid call are durable
// before any call runs.
func (t *turn) commitStep(ctx context.Context, res models.Result, attempts int, toolsSHA string, latency time.Duration) error {
	mrEvent, err := t.modelRequest(ctx, res, attempts, toolsSHA, latency)
	if err != nil {
		return err
	}
	truncated := res.StopReason == ir.StopMaxTokens
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
			se, err := t.sessionError(CodeOutputTruncated, "the response stopped at the output limit three times in a row", true, "")
			if err != nil {
				return err
			}
			return t.finish(ctx, session.StopOutputLimit, "", append(batch, se)...)
		}
		return t.commit(ctx, batch...)
	}
	t.continuations = 0

	planned, answered, extra, err := t.plan(res)
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
	if err := t.runCalls(ctx, planned.run); err != nil {
		return err
	}
	switch {
	case len(planned.ask) > 0:
		return t.finish(ctx, session.StopToolConfirmation, "")
	case len(planned.client) > 0:
		return t.finish(ctx, session.StopToolResult, "")
	}
	return nil
}

// modelRequest stores a response's raw body (and its request, while
// capture is on) and returns the model.request event that records it,
// with its cost. It also calibrates the next request's estimate.
func (t *turn) modelRequest(ctx context.Context, res models.Result, attempts int, toolsSHA string, latency time.Duration) (session.Event, error) {
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
	c := t.h.c.Connection
	mr := session.ModelRequest{
		Model: c.Model, Family: c.Family, Dialect: string(c.EffectiveDialect()), Codec: res.Codec,
		PromptVersion: prompt.Version(t.h.c.PromptVersion), ToolsSHA256: toolsSHA,
		RequestSHA256: res.RequestSHA256, RequestBytes: res.RequestSize, RequestBlob: reqBlob, ResponseBlob: respBlob,
		Usage: &res.Usage, LatencyMS: latency.Milliseconds(), FirstTokenMS: res.FirstToken.Milliseconds(),
		StopReason: res.StopReason, Attempts: attempts, Outcome: "ok", Loss: res.Loss,
	}
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
	for _, e := range slices.Backward(t.log) {
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
}

// plan validates, scores and decides each call of the response. Valid
// calls get an agent.tool_use for the batch; invalid and blocked ones an
// answer appended after it.
func (t *turn) plan(res models.Result) (stepPlan, []answeredCall, []session.Event, error) {
	var p stepPlan
	var answered []answeredCall
	var uses []session.Event
	remembered := rememberedPatterns(t.log)
	kind := t.h.c.Machine.Info().Kind
	for _, b := range res.Message.Blocks {
		if b.Type != ir.BlockToolUse || b.ToolUse == nil {
			continue
		}
		id, name, input := b.ToolUse.ID, b.ToolUse.Name, []byte(b.ToolUse.Args)
		tool, bad := t.h.c.Tools.Validate(name, input)
		if bad != nil {
			answered = append(answered, answeredCall{id, *bad})
			continue
		}
		props := tool.Properties()
		risk := Score(name, props, input, kind, t.h.c.Policy.Egress)
		d := t.h.c.Policy.Decide(name, props, input, risk, kind, remembered)
		mode := t.h.c.Policy.Mode
		if mode == "" {
			mode = ModeConfirm
		}
		use := session.AgentToolUse{
			ToolUseID: id, Name: name, Input: input, Risk: &risk, Verdict: string(d.Verdict), Reason: d.Reason,
			Mode: string(mode), Client: props.Client, Repeatable: props.Repeatable,
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
		}
	}
	return p, answered, uses, nil
}

// runCalls runs a step's calls: each maximal run of consecutive parallel
// tools concurrently, at most MaxParallel at once, every other call alone
// in order. Each result is appended as its call returns.
func (t *turn) runCalls(ctx context.Context, calls []plannedCall) error {
	for i := 0; i < len(calls); {
		if !calls[i].tool.Properties().Parallel {
			if err := t.call(ctx, calls[i]); err != nil {
				return err
			}
			i++
			continue
		}
		j := i
		for j < len(calls) && calls[j].tool.Properties().Parallel {
			j++
		}
		if err := t.parallel(ctx, calls[i:j]); err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (t *turn) parallel(ctx context.Context, calls []plannedCall) error {
	type done struct {
		id  string
		res tools.Result
		dur time.Duration
	}
	results := make(chan done, len(calls))
	sem := make(chan struct{}, MaxParallel)
	state := tools.StateOf(t.log, t.thread)
	for _, c := range calls {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			start := t.h.c.Clock()
			res := t.execute(ctx, c, state)
			results <- done{c.id, res, t.h.c.Clock().Sub(start)}
		}()
	}
	var errs []error
	for range calls {
		d := <-results
		if len(errs) == 0 {
			if err := t.result(ctx, d.id, d.res, d.dur); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (t *turn) call(ctx context.Context, c plannedCall) error {
	start := t.h.c.Clock()
	res := t.execute(ctx, c, tools.StateOf(t.log, t.thread))
	return t.result(ctx, c.id, res, t.h.c.Clock().Sub(start))
}

// execute runs one call. A tool's Go error is a failure of the harness
// side and becomes the outcome error with its message; a call a cancel
// cut short is canceled.
func (t *turn) execute(ctx context.Context, c plannedCall, state tools.State) tools.Result {
	res, err := c.tool.Run(ctx, tools.Call{ID: c.id, Input: c.input, Machine: t.h.c.Machine, State: state})
	switch {
	case err != nil && ctx.Err() != nil, err == nil && ctx.Err() != nil && len(res.Content) == 0:
		return tools.Text(tools.OutcomeCanceled, "The call was canceled before it finished.")
	case err != nil:
		return tools.Text(tools.OutcomeError, "The tool failed: "+err.Error())
	}
	if res.Outcome == "" {
		res.Outcome = tools.OutcomeOK
	}
	return res
}

// result appends one tool.result, commit point two.
func (t *turn) result(ctx context.Context, id string, res tools.Result, dur time.Duration) error {
	p := session.ToolResult{ToolUseID: id, Content: res.Content, IsError: res.IsError(), Outcome: res.Outcome, DurationMS: dur.Milliseconds(), Spill: res.Spill}
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
