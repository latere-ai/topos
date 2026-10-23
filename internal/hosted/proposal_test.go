// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/topos/internal/proposal"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// noTriggers is a server's reach that holds nothing, for a test that only
// reads what the harness offers.
type noTriggers struct{}

func (noTriggers) ListTriggers(context.Context) ([]runner.ListedTrigger, error) { return nil, nil }
func (noTriggers) CheckProposal(context.Context, runner.Proposal) error         { return nil }
func (noTriggers) ApplyProposal(context.Context, runner.Proposal) (runner.ListedTrigger, error) {
	return runner.ListedTrigger{}, nil
}
func (noTriggers) SetWake(context.Context, runner.Wake) (runner.ListedTrigger, error) {
	return runner.ListedTrigger{}, nil
}
func (noTriggers) CancelWake(context.Context, string) (string, error) { return "", nil }

// TestTheTriggerToolIsOfferedWhereTriggersAreKept: a session whose agent
// names trigger and wake is offered both tools when its drive reaches a
// server that keeps triggers, on a host as in a sandbox, and is not
// offered them on a runner that reaches none; an agent that names
// neither is offered none (spec 086).
func TestTheTriggerToolIsOfferedWhereTriggersAreKept(t *testing.T) {
	st := session.NewMemoryStore()
	var asked v1.Machine
	h, err := Harness(Options{Store: st, ModelsURL: luxstub.New(t).URL() + "/anthropic", ModelsKey: "k", Machines: hostMachines(t, &asked)})
	if err != nil {
		t.Fatal(err)
	}
	naming := newSession(t, st, strings.Replace(reviewer, "tools: [read, grep]", "tools: [read, trigger, wake]", 1))
	plain := newSession(t, st, reviewer)
	for name, c := range map[string]struct {
		s     session.Session
		reach runner.Triggers
		want  bool
	}{
		"an agent that names them, where triggers are kept":     {naming, noTriggers{}, true},
		"an agent that names them, on a runner that keeps none": {naming, nil, false},
		"an agent that names none":                              {plain, noTriggers{}, false},
	} {
		ctx := t.Context()
		if c.reach != nil {
			ctx = runner.WithTriggers(ctx, c.reach)
		}
		cfg, err := h(ctx, c.s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		t.Cleanup(func() { _ = cfg.Machine.Release(context.Background(), true) })
		if got := slices.Contains(cfg.Tools.Names(), proposal.Name) && slices.Contains(cfg.Tools.Names(), proposal.WakeName); got != c.want {
			t.Errorf("%s: offered %v", name, cfg.Tools.Names())
		}
	}
}
