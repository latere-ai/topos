// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/internal/store"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// budgetOf is the budget a trigger question carries, nil for none.
func budgetOf(r authz.Resource) map[string]any {
	b, _ := r.Fields["budget"].(map[string]any)
	return b
}

// perWeekOf is the per_week_usd_micro of a question's budget, -1 for none.
func perWeekOf(r authz.Resource) int64 {
	b := budgetOf(r)
	if b == nil {
		return -1
	}
	switch n := b["per_week_usd_micro"].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return -2
}

// TestTheTriggerQuestionsCarryTheAllowance (spec 087): trigger.create
// and trigger.update carry the trigger's budget when it names one, and a
// trigger without one asks without it; the trigger answers its allowance.
func TestTheTriggerQuestionsCarryTheAllowance(t *testing.T) {
	f := newTriggerFixture(t)
	var mu sync.Mutex
	asked := map[string][]authz.Resource{}
	f.authz.answer = func(req authz.Request) (authz.Decision, error) {
		if strings.HasPrefix(req.Action, "trigger.") {
			mu.Lock()
			asked[req.Action] = append(asked[req.Action], req.Resource)
			mu.Unlock()
		}
		return f.authz.next.Authorize(context.Background(), req)
	}
	tr := f.applyTrigger("alice", "watch", "schedule: '@daily', budget: {perWeek: 5000000}, session: {message: x}")
	if b := tr.Spec.Budget; b == nil || b.PerWeek != 5000000 {
		t.Fatalf("the trigger answers %+v", tr.Spec.Budget)
	}
	f.applyTrigger("alice", "watch", "schedule: '@daily', budget: {perWeek: 2000000}, session: {message: x}")
	f.applyTrigger("alice", "free", "schedule: '@daily', session: {message: x}")
	creates, updates := asked[authorizer.ActionTriggerCreate], asked[authorizer.ActionTriggerUpdate]
	if len(creates) != 2 || perWeekOf(creates[0]) != 5000000 || budgetOf(creates[1]) != nil {
		t.Fatalf("the creates: %+v", creates)
	}
	if len(updates) != 1 || perWeekOf(updates[0]) != 2000000 {
		t.Fatalf("the update: %+v", updates)
	}
	if a := f.do(http.MethodPut, "/v1/triggers/bad", "alice", triggerYAML("bad", "schedule: '@daily', budget: {perWeek: 0}, session: {message: x}")); a.status != http.StatusBadRequest || !strings.Contains(string(a.body), "spec.budget.perWeek") {
		t.Fatalf("an allowance of zero: %d %s", a.status, a.body)
	}
}

// TestAProposalsAllowanceIsTheTriggers (spec 087): a session's proposal
// with perWeekUsd is asked before the hold and on the allow with that
// budget, and applied as the trigger's spec.budget.perWeek; a change that
// names one replaces it, and a change that does not keeps it.
func TestAProposalsAllowanceIsTheTriggers(t *testing.T) {
	p := newProposing(t)
	if a := p.confirm("alice", session.DecisionDeny); a.status != http.StatusOK {
		t.Fatalf("the deny: %d %s", a.status, a.body)
	}
	p.runnerAppends(p.sess.ID, p.status(session.StatusIdle, session.StopEndTurn))
	p.questions()
	reach := p.api.SessionTriggers(p.sess.ID)
	offer := weekly
	offer.ToolUseID, offer.PerWeekUSDMicro = "toolu_b", new(int64(400000))
	p.hold("toolu_b", map[string]any{"action": "propose", "name": weekly.Name, "ask": weekly.Ask, "why": weekly.Why, "schedule": weekly.Schedule, "timeZone": weekly.TimeZone, "perWeekUsd": 0.4})
	if err := reach.CheckProposal(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	p.allow("toolu_b")
	made, err := reach.ApplyProposal(t.Context(), offer)
	if err != nil {
		t.Fatal(err)
	}
	asked := p.questions()
	if len(asked) != 2 || perWeekOf(asked[0].Resource) != 400000 || perWeekOf(asked[1].Resource) != 400000 || asked[1].Resource.Fields["origin"] == nil {
		t.Fatalf("the questions before the hold and on the allow: %+v", asked)
	}
	if made.PerWeekUSDMicro == nil || *made.PerWeekUSDMicro != 400000 {
		t.Fatalf("the listed trigger %+v", made)
	}
	if b := p.get("alice", made.ID).Spec.Budget; b == nil || b.PerWeek != 400000 {
		t.Fatalf("the applied trigger's budget %+v", b)
	}

	p.hold("toolu_c", map[string]any{"action": "change", "id": made.ID, "why": "Later.", "schedule": "30 7 * * 1", "timeZone": "UTC"})
	p.allow("toolu_c")
	if _, err := reach.ApplyProposal(t.Context(), runner.Proposal{ToolUseID: "toolu_c", Action: runner.TriggerChange, ID: made.ID, Why: "Later.", Schedule: "30 7 * * 1", TimeZone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	if b := p.get("alice", made.ID).Spec.Budget; b == nil || b.PerWeek != 400000 {
		t.Fatalf("a change without perWeekUsd kept %+v", b)
	}
	p.hold("toolu_r", map[string]any{"action": "change", "id": made.ID, "why": "More.", "perWeekUsd": 1.25})
	p.allow("toolu_r")
	if _, err := reach.ApplyProposal(t.Context(), runner.Proposal{ToolUseID: "toolu_r", Action: runner.TriggerChange, ID: made.ID, Why: "More.", PerWeekUSDMicro: new(int64(1250000))}); err != nil {
		t.Fatal(err)
	}
	if b := p.get("alice", made.ID).Spec.Budget; b == nil || b.PerWeek != 1250000 {
		t.Fatalf("a change of the allowance wrote %+v", b)
	}
}

// TestAFiringsMessageCarriesItsSpend (spec 087): a firing's message
// carries the spend of its trigger, the trigger answers status.spend and
// skippedBudget once its spend is at its allowance, and a wake's message
// carries the spend of the trigger that started its session.
func TestAFiringsMessageCarriesItsSpend(t *testing.T) {
	f := newTriggerFixture(t)
	f.applyTrigger("alice", "watch", "on: {product: github, verbs: ['*']}, budget: {perWeek: 1000}, session: {message: 'Look at {{event.resource}}.', policy: new, endOnIdle: false}, skipIfActive: false")
	first := f.fired("alice", "watch", f.envelope("1", "push", "o/r", "a"), store.OutcomeStarted)
	msgs := f.messages(first.SessionID)
	if len(msgs) != 1 || msgs[0].Spend == nil || msgs[0].Spend.TriggerID == "" || msgs[0].Spend.SpentUSDMicro != 0 || *msgs[0].Spend.PerWeekUSDMicro != 1000 {
		t.Fatalf("the first firing's message %+v", msgs)
	}
	f.runnerAppends(first.SessionID, f.status(session.StatusRunning, ""),
		f.event(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(int64(1200))}),
		f.status(session.StatusIdle, session.StopEndTurn))
	f.clock.Advance(time.Minute)
	f.fired("alice", "watch", f.envelope("2", "push", "o/r", "b"), store.OutcomeSkippedBudget)
	tr := f.get("alice", "watch")
	if sp := tr.Status.Spend; sp == nil || sp.SpentUSDMicro != 1200 || *sp.PerWeekUSDMicro != 1000 || *sp.PerLookUSDMicro != 1200 || sp.WindowDays != 7 {
		t.Fatalf("the trigger's spend %+v", tr.Status.Spend)
	}
	if c := tr.Status.Counts; c == nil || c.SkippedBudget != 1 || c.Started != 1 {
		t.Fatalf("the trigger's counts %+v", tr.Status.Counts)
	}

	// A wake of the session the routine started counts toward it; the
	// routine is at its allowance, so the wake is skipped and kept until
	// the window allows it.
	reach := f.api.SessionTriggers(first.SessionID)
	if _, err := reach.SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w", At: f.clock.Now().Add(time.Hour), Title: "Again", Note: "Look again."}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	f.tick()
	wake, err := f.objects.TriggerByName(t.Context(), alice, wakeName(first.SessionID))
	if err != nil || wake.Counts.SkippedBudget != 1 {
		t.Fatalf("the wake at the routine's allowance: %+v, %v", wake.Counts, err)
	}
	// A wake of a session a routine under its allowance started fires,
	// its message carrying that routine's spend.
	roomy := f.applyTrigger("alice", "roomy", "on: {product: github, verbs: ['*']}, budget: {perWeek: 1000000}, session: {message: 'Look at {{event.resource}}.', policy: new, endOnIdle: false}, skipIfActive: false")
	second := f.fired("alice", "roomy", f.envelope("3", "push", "o/r", "c"), store.OutcomeStarted)
	f.runnerAppends(second.SessionID, f.status(session.StatusRunning, ""),
		f.event(session.TypeModelRequest, session.ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(int64(300))}),
		f.status(session.StatusIdle, session.StopEndTurn))
	if _, err := f.api.SessionTriggers(second.SessionID).SetWake(t.Context(), runner.Wake{ToolUseID: "toolu_w2", At: f.clock.Now().Add(time.Hour), Title: "Again", Note: "Look again."}); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	f.tick()
	msgs = f.messages(second.SessionID)
	last := msgs[len(msgs)-1]
	if last.Wake == nil || last.Spend == nil || last.Spend.TriggerID != roomy.Status.ID || last.Spend.SpentUSDMicro != 300 {
		t.Fatalf("the wake's message %+v", last)
	}
}

// TestTheDocumentDescribesSpec087: the OpenAPI document describes every
// field spec 087 adds, and the texts of the routes that answer them say
// what they mean.
func TestTheDocumentDescribesSpec087(t *testing.T) {
	raw, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths      map[string]map[string]struct{ Description string } `yaml:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for schema, fields := range map[string][]string{
		"TriggerSpec":       {"budget"},
		"TriggerBudget":     {"perWeek"},
		"ManifestStatus":    {"spend"},
		"TriggerSpend":      {"windowDays", "spentUsdMicro", "perWeekUsdMicro", "perLookUsdMicro", "expectedLooks", "projectedUsdMicro"},
		"TriggerCounts":     {"skippedBudget"},
		"UserMessage":       {"spend"},
		"MessageSpend":      {"trigger_id", "window_days", "spent_usd_micro", "per_week_usd_micro", "per_look_usd_micro", "expected_looks", "projected_usd_micro"},
		"AgentToolUse":      {"attended"},
		"ApprovalRequested": {"attended"},
		"ModelRequest":      {"context"},
		"ContextReport":     {"window", "used", "source", "parts"},
		"ReportPart":        {"part", "tokens"},
		"Session":           {"context_report", "context"},
	} {
		s, ok := doc.Components.Schemas[schema]
		if !ok {
			t.Errorf("the document has no schema %s", schema)
			continue
		}
		for _, f := range fields {
			if _, ok := s.Properties[f]; !ok {
				t.Errorf("%s has no %s", schema, f)
			}
		}
	}
	texts := ""
	for _, ops := range doc.Paths {
		for _, op := range ops {
			texts += op.Description + "\n"
		}
	}
	for _, want := range []string{"skipped_budget", "spec.budget.perWeek", "budget {per_week_usd_micro}", "perWeekUsd", "status.spend", "context_report", "attended: false"} {
		if !strings.Contains(texts, want) {
			t.Errorf("no route's text says %q", want)
		}
	}
}
