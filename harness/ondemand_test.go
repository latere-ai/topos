// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
)

// onDemand is a harness whose machine opens on demand through open, with
// a tool of no effect beside the fixture's own.
func onDemand(t *testing.T, open func(context.Context) (machine.Machine, error)) (*env, *fakeTool) {
	t.Helper()
	note := &fakeTool{name: "note", props: tools.Properties{Parallel: true, Effect: tools.EffectNone}}
	e := setup(t, func(c *Config) {
		c.Machine = machine.Defer(t.Context(), machine.KindCella, open)
		if err := c.Tools.AddBuiltin(note); err != nil {
			t.Fatal(err)
		}
	})
	return e, note
}

// TestAMachineOpensAtTheFirstToolThatActsOnIt: a call of a tool of no
// effect opens no machine; the first call of a tool that acts on one
// opens it, and the tool runs on the machine that exists, once for the
// whole turn.
func TestAMachineOpensAtTheFirstToolThatActsOnIt(t *testing.T) {
	var opens atomic.Int32
	e, note := onDemand(t, func(context.Context) (machine.Machine, error) {
		opens.Add(1)
		return fakeMachine{kind: machine.KindCella}, nil
	})
	ctx := t.Context()
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		return tools.Text(tools.OutcomeOK, "on "+c.Machine.Info().Workdir), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_a", "note", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("Noted.")),
	)
	e.send(ctx, "Note it.")
	e.turn(ctx)
	if opens.Load() != 0 || len(note.ran()) != 1 {
		t.Fatalf("a tool of no effect opened %d machines", opens.Load())
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_b", "echo", `{"text":"b"}`), call("toolu_c", "echo", `{"text":"c"}`)),
		reply(ir.StopToolUse, call("toolu_d", "echo", `{"text":"d"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "Look.")
	e.turn(ctx)
	if opens.Load() != 1 {
		t.Fatalf("%d opens, want one", opens.Load())
	}
	for _, r := range e.events(ctx, session.TypeToolResult)[1:] {
		var p session.ToolResult
		if err := r.Decode(&p); err != nil || p.IsError || !strings.Contains(p.Content[0].Text, "on /work") {
			t.Fatalf("the tool did not run on the open machine: %+v %v", p, err)
		}
	}
}

// TestAMachineThatCannotOpenIsTheCallsResult: a machine that cannot be
// opened answers the call with the reason and a session.error naming its
// code, the tool never runs, and the turn goes on.
func TestAMachineThatCannotOpenIsTheCallsResult(t *testing.T) {
	e, _ := onDemand(t, func(context.Context) (machine.Machine, error) {
		return nil, &machine.OpenError{Code: "repository_unavailable", Err: errors.New("the clone failed")}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_a", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("I cannot reach the machine.")),
	)
	e.send(ctx, "Look.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if len(e.echo.ran()) != 0 {
		t.Fatal("the tool ran without a machine")
	}
	var p session.ToolResult
	if err := e.events(ctx, session.TypeToolResult)[0].Decode(&p); err != nil || p.Outcome != tools.OutcomeError || !strings.Contains(p.Content[0].Text, "could not be started") || !strings.Contains(p.Content[0].Text, "the clone failed") {
		t.Fatalf("result %+v, %v", p, err)
	}
	errs := e.events(ctx, session.TypeSessionError)
	var se session.SessionError
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != "repository_unavailable" {
		t.Fatalf("session.error %+v", errs)
	}
}

// TestACoreRefusingTheMachineForSpendStopsTheTurn: a machine whose core
// refuses it for spend stops the turn with budget, as a call the core
// refuses does.
func TestACoreRefusingTheMachineForSpendStopsTheTurn(t *testing.T) {
	e, _ := onDemand(t, func(context.Context) (machine.Machine, error) {
		return nil, &models.SpendError{Core: machine.KindCella, Code: "budget_exhausted", Err: errors.New("the sandbox allowance is spent")}
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_a", "echo", `{"text":"a"}`)))
	e.send(ctx, "Look.")
	if out := e.turn(ctx); out.StopReason != session.StopBudget || out.Detail != "budget_exhausted" {
		t.Fatalf("outcome %+v", out)
	}
}

// TestASpillOpensTheMachine: a result past its limit is spilled to the
// machine, which a machine opened on demand opens for it.
func TestASpillOpensTheMachine(t *testing.T) {
	ctx := t.Context()
	written := ""
	d := machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) {
		return spillMachine{kind: machine.KindCella, wrote: &written}, nil
	})
	text, spill, err := tools.Cap(ctx, d, "toolu_1", strings.Repeat("x", 100), 10)
	if err != nil || spill == nil || written != "/spill/tool-toolu_1.txt" || !strings.Contains(text, written) {
		t.Fatalf("%q %+v %v, wrote %q", text, spill, err, written)
	}
	failed := machine.Defer(ctx, machine.KindCella, func(context.Context) (machine.Machine, error) { return nil, errors.New("gone") })
	if _, _, err := tools.Cap(ctx, failed, "toolu_2", strings.Repeat("x", 100), 10); err == nil {
		t.Fatal("spilled to a machine that could not open")
	}
}

// spillMachine records the path it was asked to write.
type spillMachine struct {
	fakeMachine
	wrote *string
}

func (m spillMachine) SpillDir() string { return "/spill" }

func (m spillMachine) WriteFile(_ context.Context, p string, _ io.Reader, _ fs.FileMode) error {
	*m.wrote = p
	return nil
}

// TestTheMachineStartsWhenAToolCallBegins: a response that only talks, one
// that calls a tool of no effect and one that calls a tool the thread does
// not have start no machine; a response that calls a tool that acts on the
// machine starts it as the call's block begins, before its arguments have
// streamed, and the call runs on the machine that start opened (spec 048).
func TestTheMachineStartsWhenAToolCallBegins(t *testing.T) {
	began := make(chan struct{})
	var opens atomic.Int32
	e, note := onDemand(t, func(context.Context) (machine.Machine, error) {
		if opens.Add(1) == 1 {
			close(began)
		}
		return fakeMachine{kind: machine.KindCella}, nil
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, text("Let me note that."), call("toolu_a", "note", `{"text":"a"}`)),
		reply(ir.StopToolUse, call("toolu_b", "nosuch", `{}`)),
		reply(ir.StopEndTurn, text("Noted.")),
	)
	e.send(ctx, "Note it.")
	e.turn(ctx)
	if opens.Load() != 0 || len(note.ran()) != 1 {
		t.Fatalf("a turn with no call that acts on the machine opened %d machines", opens.Load())
	}
	var early atomic.Bool
	first := reply(ir.StopToolUse, call("toolu_c", "echo", `{"text":"c"}`))
	first.Hold = func(ev ir.Event) {
		if ev.Type != ir.EventArgsDelta {
			return
		}
		select {
		case <-began:
			early.Store(true)
		case <-time.After(2 * time.Second):
		}
	}
	e.stub.Script(model, first, reply(ir.StopEndTurn, text("Done.")))
	e.send(ctx, "Look.")
	e.turn(ctx)
	if !early.Load() {
		t.Fatal("the machine's open had not begun when the call's arguments streamed")
	}
	if opens.Load() != 1 || len(e.echo.ran()) != 1 {
		t.Fatalf("%d opens and %d echo calls, want one each", opens.Load(), len(e.echo.ran()))
	}
}

// TestOpensMachine: a call opens the machine when its tool has an effect
// and the runner, not a client, runs it.
func TestOpensMachine(t *testing.T) {
	for _, tc := range []struct {
		props tools.Properties
		want  bool
	}{
		{tools.Properties{Effect: tools.EffectNone}, false},
		{tools.Properties{Effect: tools.EffectRead}, true},
		{tools.Properties{Effect: tools.EffectWrite}, true},
		{tools.Properties{Effect: tools.EffectExternal}, true},
		{tools.Properties{Effect: tools.EffectWrite, Client: true}, false},
	} {
		if got := opensMachine(tc.props); got != tc.want {
			t.Errorf("opensMachine(%+v) = %v, want %v", tc.props, got, tc.want)
		}
	}
}
