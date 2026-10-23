// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// fired is a firing's message as the trigger trg sends it.
func fired(trg, firing string, wake bool) UserMessage {
	m := UserMessage{Sender: Sender{Subject: TriggerSubjectPrefix + trg, Kind: SenderTrigger}, Content: []lux.Block{{Type: ir.BlockText, Text: "Look."}}, FiringID: firing}
	if wake {
		m.Wake = &MessageWake{TriggerID: trg, SetAt: t0, DueAt: t0, Count: 1}
	}
	return m
}

// TestLooksAreTheTurnsTriggersBegan (spec 087): a look runs from its
// trigger's message through the status that closes the turn, every
// thread's costs counted; a later message ends it earlier; a person's
// turn is no look; a running turn counts so far; a wake's look says so;
// and a fork's copied looks are its parent's.
func TestLooksAreTheTurnsTriggersBegan(t *testing.T) {
	l := &qlog{t: t}
	cost := func(n int64) ModelRequest { return ModelRequest{Model: "m", Outcome: "ok", CostUSDMicro: new(n)} }
	running := SessionStatus{Status: StatusRunning}
	idle := SessionStatus{Status: StatusIdle, StopReason: StopEndTurn}

	l.add(TypeUserMessage, said("Hello."))
	l.add(TypeSessionStatus, running)
	l.add(TypeModelRequest, cost(999))
	l.add(TypeSessionStatus, idle)

	l.add(TypeUserMessage, fired("trg_1", "frg_1", false))
	l.add(TypeSessionStatus, running)
	l.add(TypeModelRequest, cost(100))
	l.add(TypeModelRequest, cost(50))
	l.evs[len(l.evs)-1].Thread = "thr_1"
	l.add(TypeToolResult, ToolResult{ToolUseID: "toolu_1", Outcome: "ok", CostUSDMicro: new(int64(7))})
	l.add(TypeSessionStatus, idle)
	l.add(TypeModelRequest, ModelRequest{Model: "small", Outcome: "ok", Purpose: PurposeTitle, CostUSDMicro: new(int64(3))})

	// Two firings that open one turn are not counted twice.
	l.add(TypeUserMessage, fired("trg_1", "frg_2", false))
	l.add(TypeUserMessage, fired("trg_w", "frg_3", true))
	l.add(TypeSessionStatus, running)
	l.add(TypeModelRequest, cost(20))
	l.add(TypeSessionStatus, idle)

	// A turn still running counts what it spent so far.
	l.add(TypeUserMessage, fired("trg_1", "frg_4", false))
	l.add(TypeSessionStatus, running)
	l.add(TypeModelRequest, cost(30))

	got := Looks(Session{}, l.evs)
	want := []Look{
		{FiringID: "frg_1", TriggerID: "trg_1", CostUSDMicro: 157, Ended: true},
		{FiringID: "frg_2", TriggerID: "trg_1", CostUSDMicro: 0, Ended: true},
		{FiringID: "frg_3", TriggerID: "trg_w", Wake: true, CostUSDMicro: 20, Ended: true},
		{FiringID: "frg_4", TriggerID: "trg_1", CostUSDMicro: 30},
	}
	if len(got) != len(want) {
		t.Fatalf("looks %+v", got)
	}
	for i := range want {
		want[i].At = got[i].At
		if got[i] != want[i] {
			t.Errorf("look %d is %+v, want %+v", i, got[i], want[i])
		}
	}
	if !got[0].At.Equal(t0) {
		t.Errorf("a look's time is its message's, %v", got[0].At)
	}

	// A fork copied everything through the second look; its own looks
	// are the ones after.
	fork := Session{Parent: &Parent{SessionID: "ses_p", Seq: l.evs[len(l.evs)-4].Seq}}
	if own := Looks(fork, l.evs); len(own) != 1 || own[0].FiringID != "frg_4" {
		t.Fatalf("a fork's own looks %+v", own)
	}
}

// TestThePaceLineSaysWhenAWatchIsAheadOfPace (spec 087): the fold renders
// the pace line before a firing's message that carries its spend, after
// the woken context and before a wake's line; it says the watch is ahead
// of pace when the projection passes the allowance, with a wake's advice
// or a trigger's; and a message with no spend renders none.
func TestThePaceLineSaysWhenAWatchIsAheadOfPace(t *testing.T) {
	week, look := int64(1000000), int64(200000)
	ahead := MessageSpend{TriggerID: "trg_1", WindowDays: 7, SpentUSDMicro: 800000, PerWeekUSDMicro: &week, PerLookUSDMicro: &look, ExpectedLooks: 3, ProjectedUSDMicro: new(int64(1400000))}
	onPace := ahead
	onPace.ExpectedLooks, onPace.ProjectedUSDMicro = 1, new(int64(1000000))
	free := MessageSpend{TriggerID: "trg_1", WindowDays: 7, SpentUSDMicro: 4200, ExpectedLooks: 1}

	render := func(m UserMessage) []lux.Block {
		t.Helper()
		l := &qlog{t: t}
		l.add(TypeUserMessage, m)
		tr, err := FoldWith(l.evs, "", FoldOptions{Notify: true})
		if err != nil {
			t.Fatal(err)
		}
		return tr.Messages[0].Blocks
	}
	trig := fired("trg_1", "frg_1", false)
	trig.Notify, trig.Spend = NotifyJudged, &ahead
	blocks := render(trig)
	if len(blocks) != 3 || !strings.HasPrefix(blocks[0].Text, "<woken>") || !strings.HasPrefix(blocks[1].Text, "<pace>") || blocks[2].Text != "Look." {
		t.Fatalf("the blocks of a judged trigger's look %+v", blocks)
	}
	if p := blocks[1].Text; !strings.Contains(p, "may cost $1.00 a week") || !strings.Contains(p, "cost $0.80") || !strings.Contains(p, "about $0.20") ||
		!strings.Contains(p, "3 looks") || !strings.Contains(p, "about $1.40 in all") || !strings.Contains(p, "ahead of pace") || !strings.Contains(p, "propose a change") {
		t.Fatalf("the pace of a trigger ahead of it: %s", p)
	}

	wake := fired("trg_w", "frg_2", true)
	wake.Spend = &ahead
	blocks = render(wake)
	if len(blocks) != 4 || !strings.HasPrefix(blocks[1].Text, "<pace>") || !strings.HasPrefix(blocks[2].Text, "<wake>") || !strings.Contains(blocks[1].Text, "Set your next wake later") {
		t.Fatalf("the blocks of a wake's look %+v", blocks)
	}

	on := fired("trg_1", "frg_3", false)
	on.Spend = &onPace
	if p := render(on)[0].Text; strings.Contains(p, "ahead of pace") || !strings.Contains(p, "about $1.00 in all") {
		t.Fatalf("a watch on pace: %s", p)
	}
	none := fired("trg_1", "frg_4", false)
	none.Spend = &free
	if p := render(none)[0].Text; !strings.Contains(p, "no weekly allowance") || !strings.Contains(p, "$0.0042") || strings.Contains(p, "A look costs") {
		t.Fatalf("a watch with no allowance and no ended look: %s", p)
	}
	if blocks := render(fired("trg_1", "frg_5", false)); len(blocks) != 1 {
		t.Fatalf("a message with no spend renders %+v", blocks)
	}
	if USD(1234567) != "$1.23" || USD(0) != "$0.00" || USD(4200) != "$0.0042" || USD(10000) != "$0.01" {
		t.Errorf("USD: %s %s %s %s", USD(1234567), USD(0), USD(4200), USD(10000))
	}
}
