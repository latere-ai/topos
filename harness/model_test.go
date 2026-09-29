// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// switchTo appends the session.model_changed a person's switch appends.
func (e *env) switchTo(ctx context.Context, name string) {
	e.t.Helper()
	ev, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: e.s.Initiator, Old: session.ModelRef{Name: model}, New: session.ModelRef{Name: name}}, t0)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, ev)
}

// TestATurnRunsOnTheSessionsModel: a session that switched its model
// runs its turn on the connection and figures Connect answers, asked
// once for the turns after; a model Connect cannot have closes the turn
// with the model's code before any request.
func TestATurnRunsOnTheSessionsModel(t *testing.T) {
	const other = "other-model"
	var asked []string
	e := setup(t, func(c *Config) {
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			asked = append(asked, name)
			if name != other {
				return nil, models.Connection{}, models.Entry{}, &models.Coded{Code: models.CodeUnknown, Message: "no figures for " + name}
			}
			return &dialect.Model{}, models.Connection{BaseURL: c.Connection.BaseURL, Model: other, Family: models.FamilyAnthropic},
				models.Entry{Name: other, InputWindow: 30_000, MaxOutputTokens: 2_000}, nil
		}
	})
	ctx := t.Context()
	answer := func(text string) luxstub.Reply {
		return luxstub.Reply{Response: ir.Response{Model: other, Blocks: []ir.Block{{Type: ir.BlockText, Text: text}}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}}}
	}
	e.stub.Script(other, answer("First."), answer("Second."))
	e.switchTo(ctx, other)
	for _, msg := range []string{"One.", "Two."} {
		e.send(ctx, msg)
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("outcome %+v", out)
		}
	}
	for _, ev := range e.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := ev.Decode(&p); err != nil || p.Model != other || p.MaxTokens != 2_000 {
			t.Fatalf("model.request %+v, %v; want %s asked at its output limit", p, err, other)
		}
	}
	if reqs := e.stub.Requests(); len(reqs) != 2 || reqs[0].Request.Model != other {
		t.Fatalf("%d requests", len(reqs))
	}
	if len(asked) != 1 {
		t.Fatalf("Connect was asked %v; once is enough for the turns after", asked)
	}

	e.switchTo(ctx, "no-such-model")
	e.send(ctx, "Three.")
	out := e.turn(ctx)
	if out.Status != session.StatusIdle || out.StopReason != session.StopError || out.Detail != models.CodeUnknown {
		t.Fatalf("outcome %+v", out)
	}
	errs := e.events(ctx, session.TypeSessionError)
	var se session.SessionError
	if len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != models.CodeUnknown {
		t.Fatalf("session.error %+v", errs)
	}
	if n := len(e.stub.Requests()); n != 2 {
		t.Fatalf("a turn on a model that cannot be had sent a request: %d", n)
	}
}
