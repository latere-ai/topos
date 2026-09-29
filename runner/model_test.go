// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// switcher is a tool that switches the session's model through the
// store while the turn runs, as a person's PATCH does.
type switcher struct {
	st session.Store
	id string
	to string
}

func (switcher) Definition() tools.Definition {
	return tools.Definition{Name: "switch", Description: "switches the model", InputSchema: json.RawMessage(`{"type":"object"}`)}
}
func (switcher) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectRead} }
func (sw switcher) Run(ctx context.Context, _ tools.Call) (tools.Result, error) {
	s, err := sw.st.Get(ctx, sw.id)
	if err != nil {
		return tools.Result{}, err
	}
	e, err := session.NewEvent(session.TypeModelChanged, session.ModelChanged{By: s.Initiator, Old: session.ModelRef{Name: model}, New: session.ModelRef{Name: sw.to}}, t0)
	if err != nil {
		return tools.Result{}, err
	}
	evs := []session.Event{e}
	session.Stamp(sw.id, s.LastSeq, evs)
	if _, err := sw.st.Append(ctx, sw.id, s.LastSeq, evs); err != nil {
		return tools.Result{}, err
	}
	return tools.Text(tools.OutcomeOK, "switched"), nil
}

// TestAModelSwitchAppliesToTheNextTurn: a switch appended while a turn
// runs leaves the rest of that turn on its model, and the session's
// next turn runs on the model it switched to, connected through the
// harness configuration's Connect.
func TestAModelSwitchAppliesToTheNextTurn(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	const other = "other-model"
	var connected []string
	base := f.r.o.Harness
	f.r.o.Harness = func(ctx context.Context, s session.Session) (harness.Config, error) {
		c, err := base(ctx, s)
		if err != nil {
			return c, err
		}
		c.Connect = func(_ context.Context, name string) (models.Model, models.Connection, models.Entry, error) {
			connected = append(connected, name)
			return &dialect.Model{}, models.Connection{BaseURL: f.stub.URL() + "/anthropic", Model: name, Family: models.FamilyAnthropic},
				models.Entry{Name: name, InputWindow: 50_000, MaxOutputTokens: 4_000}, nil
		}
		return c, c.Tools.AddBuiltin(switcher{st: f.store, id: s.ID, to: other})
	}
	f.stub.Script(model,
		reply(ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_s", Name: "switch", Args: json.RawMessage(`{}`)}}),
		reply(ir.Block{Type: ir.BlockText, Text: "Still the first model."}),
	)
	f.stub.Script(other, luxstub.Reply{Response: ir.Response{Model: other, Blocks: []ir.Block{{Type: ir.BlockText, Text: "The other model."}}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 10, OutputTokens: 2}}})
	f.message(ctx, "Switch, then go on.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil || s.Model == nil || s.Model.Name != other {
		t.Fatalf("the header's model is %+v after the switch, %v", s.Model, err)
	}
	f.message(ctx, "Now the next turn.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	var asked []string
	var turns []int
	for _, e := range f.events(ctx, session.TypeModelRequest) {
		var p session.ModelRequest
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		asked, turns = append(asked, p.Model), append(turns, e.Turn)
	}
	if len(asked) != 3 || asked[0] != model || asked[1] != model || asked[2] != other || turns[1] != 1 || turns[2] != 2 {
		t.Fatalf("the requests asked %v in turns %v; the switched turn's second step must stay on %s and the next turn run on %s", asked, turns, model, other)
	}
	if len(connected) != 1 || connected[0] != other {
		t.Fatalf("Connect was asked %v", connected)
	}
	if reqs := f.stub.Requests(); len(reqs) != 3 || reqs[2].Request.Model != other {
		t.Fatalf("the provider saw %d requests, the last for %q", len(reqs), reqs[len(reqs)-1].Request.Model)
	}
}
