// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// TestSpentCountsToolCosts (spec 042): the spend is every model
// request's cost and every tool result's a service charged, in Spent and
// in the header ApplyBatch keeps; a redacted result keeps its cost in its
// tombstone and is still counted; a result with no cost and an old log
// count as before.
func TestSpentCountsToolCosts(t *testing.T) {
	l := &qlog{t: t}
	l.add(TypeModelRequest, ModelRequest{Model: "m", CostUSDMicro: new(int64(1200))})
	searched := l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_1", Content: []lux.Block{{Type: ir.BlockText, Text: "1. Go"}}, Outcome: "ok", CostUSDMicro: new(int64(10000))})
	l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_2", Content: []lux.Block{{Type: ir.BlockText, Text: "read"}}, Outcome: "ok"})
	l.add(TypeModelRequest, ModelRequest{Model: "m", CostUSDMicro: new(int64(800))})
	if got := Spent(l.evs); got != 12000 {
		t.Fatalf("spent %d, want 12000", got)
	}
	var s Session
	ApplyBatch(&s, l.evs)
	if s.Budget.SpentCostUSDMicro != 12000 {
		t.Fatalf("the header's spend %d, want 12000", s.Budget.SpentCostUSDMicro)
	}

	l.redact(searched.ID)
	i := len(l.evs)
	for j, e := range l.evs {
		if e.ID == searched.ID {
			i = j
		}
	}
	tomb := l.evs[i]
	if !tomb.Redacted() || tomb.Answers() != "toolu_1" || strings.Contains(string(tomb.Payload), "1. Go") {
		t.Fatalf("the tombstone %s", tomb.Payload)
	}
	if got := Spent(l.evs); got != 12000 {
		t.Fatalf("spent after the redaction %d, want 12000", got)
	}
	var again Session
	ApplyBatch(&again, l.evs)
	if again.Budget.SpentCostUSDMicro != 12000 {
		t.Fatalf("the header's spend after the redaction %d", again.Budget.SpentCostUSDMicro)
	}

	// A tool result redacted with no cost keeps the tombstone it had.
	free := l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_3", Outcome: "ok"})
	l.redact(free.ID)
	if p := string(l.evs[len(l.evs)-1].Payload); p != `{"tombstone":true,"tool_use_id":"toolu_3"}` {
		t.Fatalf("a free result's tombstone %s", p)
	}
	if Cost(Event{Type: TypeUserMessage, Payload: []byte(`{"cost_usd_micro":5}`)}) != 0 {
		t.Fatal("a message was counted")
	}
}
