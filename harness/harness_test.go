// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

var t0 = time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

// storeLog is a Log over a session.Store, as the runner provides one.
type storeLog struct {
	st   session.Store
	id   string
	mu   sync.Mutex
	last uint64
}

func (l *storeLog) Append(ctx context.Context, batch []session.Event) ([]session.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	foreign, err := l.st.Events(ctx, l.id, l.last+1, 0)
	if err != nil {
		return nil, err
	}
	if n := len(foreign); n > 0 {
		l.last = foreign[n-1].Seq
	}
	session.Stamp(l.id, l.last, batch)
	last, err := l.st.Append(ctx, l.id, l.last, batch)
	if err != nil {
		return nil, err
	}
	l.last = last
	return foreign, nil
}

func (l *storeLog) PutBlob(ctx context.Context, r io.Reader) (session.Digest, error) {
	return l.st.PutBlob(ctx, l.id, r)
}

func (l *storeLog) Blob(ctx context.Context, d session.Digest) (io.ReadCloser, error) {
	return l.st.Blob(ctx, l.id, d)
}

// fakeTool is a tool whose behavior a test sets.
type fakeTool struct {
	name  string
	props tools.Properties
	run   func(c tools.Call) (tools.Result, error)
	mu    sync.Mutex
	calls []string
}

func (f *fakeTool) Definition() tools.Definition {
	return tools.Definition{Name: f.name, Description: "a test tool", InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"},"command":{"type":"string"}},"additionalProperties":false}`)}
}
func (f *fakeTool) Properties() tools.Properties { return f.props }
func (f *fakeTool) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c.ID)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(c)
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, err
	}
	return tools.Text(tools.OutcomeOK, "echo "+in.Text), nil
}

func (f *fakeTool) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// fakeMachine is enough of a machine for the loop; the tools here do not
// touch it.
type fakeMachine struct {
	machine.Machine
	kind string
}

func (m fakeMachine) Info() machine.Info { return machine.Info{Kind: m.kind, Workdir: "/work"} }

type env struct {
	t     *testing.T
	stub  *luxstub.Server
	store session.Store
	s     session.Session
	log   *storeLog
	h     *Harness
	cfg   Config
	echo  *fakeTool
	write *fakeTool
	hist  int
}

const model = "builder-model"

func price(s string) *models.Price {
	p, err := models.ParsePrice(s)
	if err != nil {
		panic(err)
	}
	return &p
}

func setup(t *testing.T, mut func(*Config)) *env {
	t.Helper()
	e := &env{t: t, stub: luxstub.New(t), store: session.NewMemoryStore()}
	e.echo = &fakeTool{name: "echo", props: tools.Properties{Parallel: true, Effect: tools.EffectRead}}
	e.write = &fakeTool{name: "bash", props: tools.Properties{Effect: tools.EffectWrite}}
	reg := tools.NewRegistry()
	for _, ft := range []*fakeTool{e.echo, e.write} {
		if err := reg.AddBuiltin(ft); err != nil {
			t.Fatal(err)
		}
	}
	e.cfg = Config{
		Model:      &dialect.Model{},
		Connection: models.Connection{BaseURL: e.stub.URL() + "/anthropic", Model: model, Family: models.FamilyAnthropic},
		Entry:      models.Entry{Name: model, InputWindow: 200_000, MaxOutputTokens: 64_000, Pricing: &models.Pricing{Input: price("1"), Output: price("5")}},
		Machine:    fakeMachine{kind: machine.KindCella},
		Tools:      reg,
		Policy:     Policy{Mode: ModeConfirm},
		Clock:      func() time.Time { return t0 },
		Sleep:      func(context.Context, time.Duration) error { return nil },
	}
	if mut != nil {
		mut(&e.cfg)
	}
	h, err := New(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	e.s = session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1},
		session.Sender{Subject: "usr_ada", Name: "Ada", Kind: session.SenderPerson}, session.RunnerHosted,
		session.Machine{Kind: machine.KindCella}, t0)
	if err := e.store.Create(t.Context(), e.s, nil); err != nil {
		t.Fatal(err)
	}
	e.log = &storeLog{st: e.store, id: e.s.ID}
	return e
}

// send appends a user message and a running status, as a client and the
// runner do before a turn.
func (e *env) send(ctx context.Context, text string) {
	e.t.Helper()
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, msg)
	e.running(ctx)
}

func (e *env) running(ctx context.Context) {
	e.t.Helper()
	run, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "run_1", Kind: session.RunnerHosted}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, run)
}

func (e *env) appendEvents(ctx context.Context, evs ...session.Event) {
	e.t.Helper()
	if _, err := e.log.Append(ctx, evs); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) turn(ctx context.Context) Outcome {
	e.t.Helper()
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) events(ctx context.Context, typ session.Type) []session.Event {
	e.t.Helper()
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []session.Event
	for _, ev := range evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func text(s string) ir.Block { return ir.Block{Type: ir.BlockText, Text: s} }

func call(id, name, args string) ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: name, Args: json.RawMessage(args)}}
}

func reply(stop ir.StopReason, blocks ...ir.Block) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: model, Blocks: blocks, StopReason: stop, Usage: ir.Usage{InputTokens: 100, OutputTokens: 10}}}
}

func TestATurnRunsToolsAndEnds(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, text("Checking."), call("toolu_a", "echo", `{"text":"a"}`), call("toolu_b", "echo", `{"text":"b"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Done.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if len(last.Blocks) != 2 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.ToolUseID != "toolu_a" {
				return fmt.Errorf("the results did not come back in tool_use order: %+v", last)
			}
			return nil
		}},
	)
	e.send(ctx, "Echo two things.")
	out := e.turn(ctx)
	if out.Status != session.StatusIdle || out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.echo.ran(); len(got) != 2 {
		t.Fatalf("echo ran %v", got)
	}
	reqs := e.events(ctx, session.TypeModelRequest)
	if len(reqs) != 2 {
		t.Fatalf("%d model requests", len(reqs))
	}
	var mr session.ModelRequest
	if err := reqs[0].Decode(&mr); err != nil {
		t.Fatal(err)
	}
	if mr.Outcome != "ok" || mr.PromptVersion != "harness/1" || mr.RequestSHA256 == "" || mr.ResponseBlob == "" || mr.CostUSDMicro == nil || *mr.CostUSDMicro != 150 || mr.CostSource != models.CostCatalog {
		t.Fatalf("model.request %+v", mr)
	}
	if rc, err := e.store.Blob(ctx, e.s.ID, mr.ResponseBlob); err != nil {
		t.Fatalf("the raw response blob: %v", err)
	} else if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	h, err := e.store.Get(ctx, e.s.ID)
	if err != nil || h.Status != session.StatusIdle || h.Turn != 1 {
		t.Fatalf("header %+v, %v", h, err)
	}
	first := e.stub.Requests()[0].Request
	if first.MaxTokens == nil || *first.MaxTokens != 64_000 {
		t.Fatalf("max_tokens %v, want the catalog's output limit", first.MaxTokens)
	}
	if len(first.System) == 0 || !strings.Contains(first.System[0].Text, "You are an agent") || !first.System[len(first.System)-1].CacheHint {
		t.Fatalf("system prompt %+v", first.System)
	}
}

func TestNoStepCap(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	const steps = 200
	for i := range steps {
		e.stub.Script(model, reply(ir.StopToolUse, call(fmt.Sprintf("toolu_%d", i), "echo", `{"text":"x"}`)))
	}
	e.stub.Script(model, reply(ir.StopEndTurn, text("All done.")))
	e.send(ctx, "Loop.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.echo.ran()); n != steps {
		t.Fatalf("%d calls ran, want %d", n, steps)
	}
}

func TestATruncatedCallNeverRuns(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopMaxTokens, text("Writing"), call("toolu_cut", "echo", `{"text":"x"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Continued and done.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			if !strings.Contains(last.Blocks[len(last.Blocks)-1].Text, "cut off at the output limit") {
				return errors.New("no continuation prompt")
			}
			for _, m := range r.Messages {
				for _, b := range m.Blocks {
					if b.Type == ir.BlockToolUse {
						return errors.New("the truncated tool_use was replayed")
					}
				}
			}
			return nil
		}},
	)
	e.send(ctx, "Write it.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.echo.ran()); n != 0 {
		t.Fatalf("a truncated call ran %d times", n)
	}
	msgs := e.events(ctx, session.TypeAgentMessage)
	var second session.AgentMessage
	if err := msgs[1].Decode(&second); err != nil || second.ContinuationOf != msgs[0].ID {
		t.Fatalf("continuation_of %q, want %s", second.ContinuationOf, msgs[0].ID)
	}
}

func TestThreeTruncationsEndWithOutputLimit(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	for range 3 {
		e.stub.Script(model, reply(ir.StopMaxTokens, text("more")))
	}
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopOutputLimit {
		t.Fatalf("outcome %+v", out)
	}
	errs := e.events(ctx, session.TypeSessionError)
	var se session.SessionError
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeOutputTruncated {
		t.Fatalf("session.error %+v", errs)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	r := reply(ir.StopEndTurn, text("ok"))
	r.Fail = &luxstub.Failure{Status: 529, Times: 2}
	e.stub.Script(model, r)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[0].Decode(&mr); err != nil || mr.Attempts != 3 {
		t.Fatalf("attempts %d, %v", mr.Attempts, err)
	}
}

func TestAFailureKeepsEarlierEvents(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	fail := reply(ir.StopEndTurn, text("never"))
	fail.Fail = &luxstub.Failure{Status: 400, Times: 99, Body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"kept"}`)), fail)
	e.send(ctx, "Go.")
	out := e.turn(ctx)
	if out.StopReason != session.StopError || out.Detail != CodeModelError {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.events(ctx, session.TypeToolResult)); n != 1 {
		t.Fatalf("%d earlier results kept", n)
	}
	reqs := e.events(ctx, session.TypeModelRequest)
	var mr session.ModelRequest
	if err := reqs[len(reqs)-1].Decode(&mr); err != nil || mr.Outcome != "error" || mr.Attempts != 1 {
		t.Fatalf("the failed request %+v, %v", mr, err)
	}
	var se session.SessionError
	if err := e.events(ctx, session.TypeSessionError)[0].Decode(&se); err != nil || se.Code != CodeModelError || se.Retryable {
		t.Fatalf("session.error %+v", se)
	}
}

func TestThinkingIsReplayedWithItsSignature(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, ir.Block{Type: ir.BlockThinking, Text: "plan", Signature: "sig-1"}, call("toolu_1", "echo", `{"text":"x"}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("done")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			for _, m := range r.Messages {
				for _, b := range m.Blocks {
					if b.Type == ir.BlockThinking && b.Signature == "sig-1" && b.Text == "plan" {
						return nil
					}
				}
			}
			return errors.New("the thinking block was not replayed with its signature")
		}},
	)
	e.send(ctx, "Think.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
}

func TestTheBudgetStopsTheTurn(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	limit := int64(200)
	e.s.Budget.MaxCostUSDMicro = &limit
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(ctx, "Spend.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Budget.MaxCostUSDMicro = &limit
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil || out.StopReason != session.StopBudget {
		t.Fatalf("outcome %+v, %v", out, err)
	}
	if n := len(e.stub.Requests()); n != 1 {
		t.Fatalf("%d requests; the second should be refused before it is sent", n)
	}
}

func TestConfirmationsAndDenials(t *testing.T) {
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`), call("toolu_r", "echo", `{"text":"r"}`)),
		reply(ir.StopToolUse, call("toolu_w2", "bash", `{"command":"make install"}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	if got := e.echo.ran(); len(got) != 1 || len(e.write.ran()) != 0 {
		t.Fatalf("before the confirmation: echo %v, shell %v", got, e.write.ran())
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_w", Decision: session.DecisionAllow, Remember: "bash(make*)"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, conf)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the confirmation %+v", out)
	}
	if got := e.write.ran(); len(got) != 2 {
		t.Fatalf("shell ran %v; the remembered pattern should allow the second call", got)
	}
	var use session.AgentToolUse
	if err := e.events(ctx, session.TypeAgentToolUse)[0].Decode(&use); err != nil || use.Verdict != "ask" || use.Risk == nil || use.Risk.Source != RiskSource {
		t.Fatalf("agent.tool_use %+v", use)
	}

	d := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	d.stub.Script(model, reply(ir.StopToolUse, call("toolu_x", "bash", `{"command":"rm -rf /"}`)), reply(ir.StopEndTurn, text("ok")))
	d.send(ctx, "Clean.")
	if out := d.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	deny, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: d.s.Initiator, ToolUseID: "toolu_x", Decision: session.DecisionDeny, Note: "not that"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	d.appendEvents(ctx, deny)
	d.running(ctx)
	if out := d.turn(ctx); out.StopReason != session.StopEndTurn || len(d.write.ran()) != 0 {
		t.Fatalf("after a denial %+v, ran %v", out, d.write.ran())
	}
	var res session.ToolResult
	if err := d.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeDenied || !strings.Contains(res.Content[0].Text, "not that") {
		t.Fatalf("denied result %+v", res)
	}
}

func TestAnUnansweredAskKeepsWaiting(t *testing.T) {
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation || len(e.stub.Requests()) != 1 {
		t.Fatalf("a claim with no answer: %+v, %d requests", out, len(e.stub.Requests()))
	}
}

func TestPlanModeBlocksWrites(t *testing.T) {
	e := setup(t, func(c *Config) { c.Policy.Mode = ModePlan })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`)), reply(ir.StopEndTurn, text("ok")))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.write.ran()) != 0 {
		t.Fatalf("plan mode %+v, ran %v", out, e.write.ran())
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeBlocked {
		t.Fatalf("blocked result %+v", res)
	}
}

func TestInvalidCallsAreAnsweredWithoutRunning(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_u", "nope", `{}`), call("toolu_i", "echo", `{"text":5}`)),
		reply(ir.StopEndTurn, text("ok")),
	)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	results := e.events(ctx, session.TypeToolResult)
	var a, b session.ToolResult
	if err := errors.Join(results[0].Decode(&a), results[1].Decode(&b)); err != nil {
		t.Fatal(err)
	}
	if a.Outcome != tools.OutcomeUnknownTool || b.Outcome != tools.OutcomeInvalidInput || !strings.Contains(b.Content[0].Text, "/text") {
		t.Fatalf("results %+v %+v", a, b)
	}
	if n := len(e.events(ctx, session.TypeAgentToolUse)); n != 0 {
		t.Fatalf("%d agent.tool_use for invalid calls", n)
	}
}

func TestAnInterruptStopsAtTheNextStep(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		in, err := session.NewEvent(session.TypeUserInterrupt, session.UserInterrupt{Sender: e.s.Initiator}, t0)
		if err != nil {
			return tools.Result{}, err
		}
		evs, err := e.store.Events(context.Background(), e.s.ID, 1, 0)
		if err != nil {
			return tools.Result{}, err
		}
		in.SessionID = e.s.ID
		in.Seq = evs[len(evs)-1].Seq + 1
		if _, err := e.store.Append(context.Background(), e.s.ID, in.Seq-1, []session.Event{in}); err != nil {
			return tools.Result{}, err
		}
		return tools.Text(tools.OutcomeOK, "done"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopInterrupted {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.stub.Requests()); n != 1 {
		t.Fatalf("%d requests after an interrupt", n)
	}
}

func TestResumeClosesACallWithNoResult(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_lost", Name: "bash", Input: json.RawMessage(`{}`), Verdict: "allow"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_lost", Name: "bash", Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.send(ctx, "Go.")
	e.appendEvents(ctx, msg, use)
	e.running(ctx)
	e.stub.Script(model, luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("inspected")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		last := r.Messages[len(r.Messages)-1]
		if last.Blocks[0].ToolResult == nil || !last.Blocks[0].ToolResult.IsError {
			return errors.New("the lost call was not closed as an error result")
		}
		return nil
	}})
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.write.ran()) != 0 {
		t.Fatalf("outcome %+v, reran %v", out, e.write.ran())
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeUnknownEffect {
		t.Fatalf("result %+v", res)
	}
}

func TestClientToolsWaitForTheirResult(t *testing.T) {
	e := setup(t, nil)
	client := &fakeTool{name: "pick_color", props: tools.Properties{Client: true, Effect: tools.EffectNone}}
	if err := e.cfg.Tools.Add(client); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_c", "pick_color", `{}`)), reply(ir.StopEndTurn, text("blue it is")))
	e.send(ctx, "Pick.")
	if out := e.turn(ctx); out.StopReason != session.StopToolResult || len(client.ran()) != 0 {
		t.Fatalf("outcome %+v, ran %v", out, client.ran())
	}
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopToolResult {
		t.Fatalf("a claim before the client answered: %+v", out)
	}
	ans, err := session.NewEvent(session.TypeUserToolResult, session.UserToolResult{Sender: e.s.Initiator, ToolUseID: "toolu_c", Content: []lux.Block{{Type: ir.BlockText, Text: "blue"}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, ans)
	e.running(ctx)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("after the client answered %+v", out)
	}
}

func TestNewRefusesAnIncompleteConfig(t *testing.T) {
	good := setup(t, nil).cfg
	for name, mut := range map[string]func(*Config){
		"no model":      func(c *Config) { c.Model = nil },
		"no machine":    func(c *Config) { c.Machine = nil },
		"no tools":      func(c *Config) { c.Tools = nil },
		"bad conn":      func(c *Config) { c.Connection.BaseURL = "" },
		"unknown model": func(c *Config) { c.Entry.MaxOutputTokens = 0 },
		"bad prompt":    func(c *Config) { c.PromptVersion = 99 },
	} {
		c := good
		mut(&c)
		if _, err := New(c); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	var coded *models.Coded
	c := good
	c.Entry.InputWindow = 0
	if _, err := New(c); !errors.As(err, &coded) || coded.Code != models.CodeUnknown {
		t.Fatalf("an unknown model: %v", err)
	}
}

type recorder struct {
	mu     sync.Mutex
	deltas int
	resets int
}

func (r *recorder) OnDelta(Delta) { r.mu.Lock(); r.deltas++; r.mu.Unlock() }
func (r *recorder) OnReset(string, int, int) {
	r.mu.Lock()
	r.resets++
	r.mu.Unlock()
}

func TestObserverSeesDeltasAndResets(t *testing.T) {
	rec := &recorder{}
	e := setup(t, func(c *Config) { c.Observer = rec })
	ctx := t.Context()
	r := reply(ir.StopEndTurn, text("hello"))
	r.Fail = &luxstub.Failure{Status: 503}
	e.stub.Script(model, r)
	e.send(ctx, "Hi.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if rec.deltas == 0 || rec.resets != 1 {
		t.Fatalf("deltas %d, resets %d", rec.deltas, rec.resets)
	}
}

func TestEndOnIdleAndRefusal(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopRefusal, text("I can't help with that.")))
	e.send(ctx, "Do the thing.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.EndOnIdle = true
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil || out.Status != session.StatusEnded || out.StopReason != session.StopCompleted || out.Detail != "refusal" {
		t.Fatalf("outcome %+v, %v", out, err)
	}
}

func TestTheTurnDeadline(t *testing.T) {
	now := t0
	e := setup(t, func(c *Config) {
		c.TurnTimeout = time.Minute
		c.Clock = func() time.Time { return now }
	})
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) {
		now = now.Add(2 * time.Minute)
		return tools.Text(tools.OutcomeOK, "slow"), nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopTurnLimit {
		t.Fatalf("outcome %+v", out)
	}

	w := setup(t, func(c *Config) { c.TurnTimeout = time.Second })
	r := reply(ir.StopEndTurn, text("never"))
	r.Fail = &luxstub.Failure{Status: 503, RetryAfter: "30", Times: 5}
	w.stub.Script(model, r)
	w.send(ctx, "Go.")
	if out := w.turn(ctx); out.StopReason != session.StopError {
		t.Fatalf("a retry that would pass the deadline: %+v", out)
	}
	if n := len(w.stub.Requests()); n != 1 {
		t.Fatalf("%d attempts; the wait would pass the deadline", n)
	}
}

func TestAToolFailureIsAResult(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.echo.run = func(tools.Call) (tools.Result, error) { return tools.Result{}, errors.New("disk full") }
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("noted")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeError || !res.IsError || !strings.Contains(res.Content[0].Text, "disk full") {
		t.Fatalf("result %+v", res)
	}
}

func TestMetaIsRecordedAndFoldedIntoState(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	var seen []tools.State
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		seen = append(seen, c.State)
		r := tools.Text(tools.OutcomeOK, "ok")
		r.Meta = &tools.Meta{Path: "/work/a.txt", SHA256: "abc"}
		return r, nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)),
		reply(ir.StopToolUse, call("toolu_2", "echo", `{"text":"y"}`)),
		reply(ir.StopEndTurn, text("done")),
	)
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if len(seen) != 2 || seen[1].Hashes["/work/a.txt"] != "abc" {
		t.Fatalf("state %+v", seen)
	}
}

func TestAnUnpricedModelUnderABudgetIsRefused(t *testing.T) {
	e := setup(t, func(c *Config) { c.Entry.Pricing = nil })
	ctx := t.Context()
	e.send(ctx, "Go.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(1000)
	s.Budget.MaxCostUSDMicro = &limit
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil || out.StopReason != session.StopError || out.Detail != models.CodeUnpriced || len(e.stub.Requests()) != 0 {
		t.Fatalf("outcome %+v, %v, %d requests", out, err, len(e.stub.Requests()))
	}
}

func TestSleepAndStopText(t *testing.T) {
	if err := sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled sleep: %v", err)
	}
	if !strings.Contains((&errStop{Outcome{StopReason: session.StopBudget}}).Error(), "budget") {
		t.Fatal("errStop text")
	}
}

type failingLog struct{ *storeLog }

func (failingLog) Append(context.Context, []session.Event) ([]session.Event, error) {
	return nil, session.ErrLocked
}

func TestALostLeaseStopsTheTurn(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Go.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.RunTurn(ctx, s, evs, failingLog{e.log}); !errors.Is(err, session.ErrLocked) {
		t.Fatalf("a failing append: %v", err)
	}
}

func TestResumeRunsARepeatableCallAgain(t *testing.T) {
	e := setup(t, nil)
	sync := &fakeTool{name: "memory_sync", props: tools.Properties{Repeatable: true, Effect: tools.EffectWrite}}
	if err := e.cfg.Tools.AddBuiltin(sync); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_s", Name: "memory_sync", Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_s", Name: "memory_sync", Input: json.RawMessage(`{"text":"x"}`), Verdict: "allow", Repeatable: true}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.send(ctx, "Sync.")
	e.appendEvents(ctx, msg, use)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("synced")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(sync.ran()) != 1 {
		t.Fatalf("outcome %+v, ran %v", out, sync.ran())
	}
}

func TestACancelDuringACallIsCanceled(t *testing.T) {
	e := setup(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e.echo.run = func(tools.Call) (tools.Result, error) {
		cancel()
		return tools.Result{Content: []lux.Block{{Type: ir.BlockText, Text: "partial"}}}, nil
	}
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("never")))
	e.send(t.Context(), "Go.")
	s, err := e.store.Get(t.Context(), e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.store.Events(t.Context(), e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.h.RunTurn(ctx, s, evs, e.log)
	if err != nil || out.StopReason != session.StopInterrupted {
		t.Fatalf("a canceled turn: %+v, %v", out, err)
	}
	var res session.ToolResult
	if err := e.events(t.Context(), session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeOK || res.Content[0].Text != "partial" {
		t.Fatalf("a call that finished before the cancel keeps its result: %+v, %v", res, err)
	}
	h, err := e.store.Get(t.Context(), e.s.ID)
	if err != nil || h.Status != session.StatusIdle {
		t.Fatalf("the header after a cancel: %+v, %v", h, err)
	}
}

func TestAConfirmedCallAnEarlierRunnerMayHaveStarted(t *testing.T) {
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_w", "bash", `{"command":"make"}`), call("toolu_gone", "bash", `{"command":"make"}`)))
	e.send(ctx, "Build.")
	if out := e.turn(ctx); out.StopReason != session.StopToolConfirmation {
		t.Fatalf("outcome %+v", out)
	}
	for _, id := range []string{"toolu_w", "toolu_gone"} {
		c, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: id, Decision: session.DecisionAllow}, t0)
		if err != nil {
			t.Fatal(err)
		}
		e.appendEvents(ctx, c)
	}
	e.running(ctx)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("inspected")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn || len(e.write.ran()) != 0 {
		t.Fatalf("outcome %+v, ran %v", out, e.write.ran())
	}
	for _, ev := range e.events(ctx, session.TypeToolResult) {
		var res session.ToolResult
		if err := ev.Decode(&res); err != nil || res.Outcome != tools.OutcomeUnknownEffect {
			t.Fatalf("result %+v, %v", res, err)
		}
	}
}

func TestAConfirmedCallForAToolThatIsGone(t *testing.T) {
	e := setup(t, func(c *Config) { c.Machine = fakeMachine{kind: machine.KindHost} })
	ctx := t.Context()
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_old", Name: "retired", Input: json.RawMessage(`{}`), Verdict: "ask"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_old", Name: "retired", Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := session.NewEvent(session.TypeUserToolConfirmation, session.UserToolConfirmation{Sender: e.s.Initiator, ToolUseID: "toolu_old", Decision: session.DecisionAllow}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.send(ctx, "Go.")
	e.appendEvents(ctx, msg, use, conf)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("ok")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	var res session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&res); err != nil || res.Outcome != tools.OutcomeUnknownTool {
		t.Fatalf("result %+v", res)
	}
}

func TestCaptureKeepsTheRequestBytes(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopEndTurn, text("hi")))
	e.send(ctx, "Go.")
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	s.Capture.Requests = true
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.RunTurn(ctx, s, evs, e.log); err != nil {
		t.Fatal(err)
	}
	var mr session.ModelRequest
	if err := e.events(ctx, session.TypeModelRequest)[0].Decode(&mr); err != nil || mr.RequestBlob == "" {
		t.Fatalf("model.request %+v, %v", mr, err)
	}
	rc, err := e.store.Blob(ctx, e.s.ID, mr.RequestBlob)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if cerr := rc.Close(); err != nil || cerr != nil {
		t.Fatal(errors.Join(err, cerr))
	}
	if session.DigestOf(b) != mr.RequestBlob || string(e.stub.Requests()[0].Body) != string(b) {
		t.Fatal("the captured request is not the bytes sent")
	}
}

func TestATurnRefusesALogItCannotFold(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.send(ctx, "Go.")
	future, err := session.NewEvent("future.kind", map[string]int{"n": 1}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, future)
	s, err := e.store.Get(ctx, e.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := e.h.RunTurn(ctx, s, evs, e.log); err != nil || out.StopReason != session.StopError || out.Detail != "schema_too_new" {
		t.Fatalf("an unknown event type: %+v, %v", out, err)
	}
	if h, err := e.store.Get(ctx, e.s.ID); err != nil || h.Status != session.StatusIdle || len(e.events(ctx, session.TypeSessionError)) != 1 {
		t.Fatalf("the header after a refused fold: %+v, %v", h, err)
	}

	r := setup(t, nil)
	r.send(ctx, "my token is abc")
	first, err := r.store.Events(ctx, r.s.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.Redact(ctx, r.s.ID, first[0].ID, r.s.Initiator, "leaked"); err != nil {
		t.Fatal(err)
	}
	s, err = r.store.Get(ctx, r.s.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs, err = r.store.Events(ctx, r.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	r.log.last = s.LastSeq
	if out, err := r.h.RunTurn(ctx, s, evs, r.log); err != nil || out.Detail != "redaction_uncompacted" {
		t.Fatalf("an uncompacted redaction: %+v, %v", out, err)
	}
}

func TestAHarnessFailureClosesTheTurn(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.send(ctx, "Go.")
	missing := session.DigestOf([]byte("never stored"))
	m, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Reason: "attached", Instructions: []session.Instructions{{Path: "/work/AGENTS.md", Blob: missing}}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, m)
	out := e.turn(ctx)
	if out.StopReason != session.StopError || out.Detail != CodeInternal {
		t.Fatalf("outcome %+v", out)
	}
	var se session.SessionError
	if err := e.events(ctx, session.TypeSessionError)[0].Decode(&se); err != nil || se.Code != CodeInternal || !strings.Contains(se.Message, "instructions") {
		t.Fatalf("session.error %+v, %v", se, err)
	}
	if h, err := e.store.Get(ctx, e.s.ID); err != nil || h.Status != session.StatusIdle || h.StopReason != session.StopError {
		t.Fatalf("header %+v, %v", h, err)
	}
}
