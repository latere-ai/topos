// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// sendAt appends a person's message and the running status that opens
// its turn, each at its own time, as a client and the runner do.
func (e *env) sendAt(ctx context.Context, text string, sent, opened time.Time) {
	e.t.Helper()
	msg, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: e.s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, sent)
	if err != nil {
		e.t.Fatal(err)
	}
	run, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusRunning, Runner: &session.RunnerRef{ID: "run_1", Kind: session.RunnerHosted}}, opened)
	if err != nil {
		e.t.Fatal(err)
	}
	e.appendEvents(ctx, msg, run)
}

// lastSystem is the last system block of each request the stub received
// for the model name, and whether the block before it carries the cache
// hint and it carries none.
func (e *env) lastSystem(name string) ([]string, bool) {
	e.t.Helper()
	var out []string
	placed := true
	for _, r := range e.stub.Requests() {
		if r.Request.Model != name {
			continue
		}
		n := len(r.Request.System)
		if n < 2 {
			e.t.Fatalf("a request with %d system blocks", n)
		}
		out = append(out, r.Request.System[n-1].Text)
		placed = placed && r.Request.System[n-2].CacheHint && !r.Request.System[n-1].CacheHint
	}
	return out, placed
}

// TestEveryRequestStatesTheDateItsTurnOpenedOn: every request of a turn,
// its later steps and a subagent's thread included, ends its system
// prompt with the UTC day of the session.status running that opened the
// turn, whatever the clock says when the request is built and though no
// machine has attached; the next turn states the day it opened on. The
// part follows the system prompt's cache breakpoint and carries none
// (specs 010 and 011).
func TestEveryRequestStatesTheDateItsTurnOpenedOn(t *testing.T) {
	// The turn opens half an hour before midnight UTC, written in a zone
	// two hours east, and its requests are built after midnight.
	east := time.FixedZone("east", 2*60*60)
	opened := time.Date(2026, 10, 13, 1, 30, 0, 0, east)
	clock := opened.Add(time.Hour)
	e := setup(t, func(c *Config) {
		withReviewer(nil)(c)
		c.Clock = func() time.Time {
			clock = clock.Add(time.Minute)
			return clock
		}
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)),
		reply(ir.StopEndTurn, text("The latest is from October.")),
		reply(ir.StopEndTurn, text("Still October.")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopEndTurn, text("Fine.")))
	e.sendAt(ctx, "What is the latest research?", opened.Add(-time.Minute), opened)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.events(ctx, session.TypeSessionMachine)); n != 0 {
		t.Fatalf("%d machines attached; the turn is to run with none", n)
	}
	next := time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC)
	e.sendAt(ctx, "And today?", next.Add(-time.Hour), next)
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	first := "<context>\nToday's date: 2026-10-12 (UTC)\n</context>"
	second := "<context>\nToday's date: 2026-10-14 (UTC)\n</context>"
	own, placed := e.lastSystem(model)
	if !slices.Equal(own, []string{first, first, second}) || !placed {
		t.Fatalf("the session's own requests end %q, breakpoint before the date %v", own, placed)
	}
	if sub, placed := e.lastSystem(reviewerModel); !slices.Equal(sub, []string{first}) || !placed {
		t.Fatalf("the subagent's request ends %q, breakpoint before the date %v", sub, placed)
	}
	// The blocks before the date are the same bytes on both days, so the
	// prefix a provider cached on the first is read on the second.
	reqs := e.stub.Requests()
	before := func(r *ir.Request) []string {
		var out []string
		for _, b := range r.System[:len(r.System)-1] {
			out = append(out, b.Text)
		}
		return out
	}
	if a, b := before(reqs[0].Request), before(reqs[len(reqs)-1].Request); !slices.Equal(a, b) {
		t.Fatalf("the system prompt before the date changed with the day:\n%q\n%q", a, b)
	}
}

// TestTheDateOutranksTheDayARecordedBlockNames: a context block recorded
// with the first version of the context prompt names the day its machine
// attached in a Date line. A request of a later day states today's date
// after it and says what that line is, so the model reads one date as the
// current one; a block of the same day, and a block of the version that
// names no date, get no such line (spec 011).
func TestTheDateOutranksTheDayARecordedBlockNames(t *testing.T) {
	block := func(name prompts.Name, date string) string {
		data := prompts.Data{"Workdir": "/work", "OS": "linux", "Arch": "arm64", "Cella": true, "Environment": "",
			"Git": true, "Branch": "main", "Head": "1a2b3c4", "Modified": 0, "Untracked": 0, "Commits": []string{"1a2b3c4 Date: not a date line"}}
		if date != "" {
			data["Date"] = date
		}
		return prompts.Render(name, data)
	}
	today := t0.Format(time.DateOnly)
	for name, c := range map[string]struct {
		context string
		want    string
	}{
		"an earlier day":   {block("context/context-v1", "2026-09-20"), "<context>\nToday's date: " + today + " (UTC)\nThe line \"Date: 2026-09-20\" above is the day this session's machine attached, not today.\n</context>"},
		"the same day":     {block("context/context-v1", today), dated(t0)},
		"a block undated":  {block(prompts.ContextBlock, ""), dated(t0)},
		"no context block": {"", dated(t0)},
	} {
		e := setup(t, nil)
		ctx := t.Context()
		if c.context != "" {
			attached, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: "cella"}, Reason: "attached", Context: c.context}, t0.AddDate(0, 0, -7))
			if err != nil {
				t.Fatal(err)
			}
			e.appendEvents(ctx, attached)
		}
		e.stub.Script(model, reply(ir.StopEndTurn, text("ok")))
		e.send(ctx, "What day is it?")
		if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
			t.Fatalf("%s: outcome %+v", name, out)
		}
		got, placed := e.lastSystem(model)
		if !slices.Equal(got, []string{c.want}) || !placed {
			t.Errorf("%s: the request ends %q, want %q", name, got, c.want)
		}
		if n := strings.Count(systemText(e.stub.Requests()[0].Request), "Today's date: "); n != 1 {
			t.Errorf("%s: the system prompt states today's date %d times", name, n)
		}
	}
}

// TestDateOf: the date of a request is the UTC day of the last
// session.status running through its fold point; an idle status, a
// status past the fold point and a status whose payload does not decode
// open no turn; a log with no running status is dated by its last event
// through the fold point, and an empty one by nothing.
func TestDateOf(t *testing.T) {
	at := func(seq uint64, typ session.Type, payload any, when time.Time) session.Event {
		e, err := session.NewEvent(typ, payload, when)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = seq
		return e
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 12, 0, 0, 0, time.UTC) }
	running, idle := session.SessionStatus{Status: session.StatusRunning}, session.SessionStatus{Status: session.StatusIdle, StopReason: session.StopEndTurn}
	message := session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "hi"}}}
	broken := at(6, session.TypeSessionStatus, running, day(6))
	broken.Payload = []byte(`{"tombstone":true}`)
	log := []session.Event{
		at(1, session.TypeUserMessage, message, day(1)),
		at(2, session.TypeSessionStatus, running, day(2)),
		at(3, session.TypeSessionStatus, idle, day(3)),
		at(4, session.TypeUserMessage, message, day(4)),
		at(5, session.TypeSessionStatus, running, time.Date(2026, 10, 6, 1, 0, 0, 0, time.FixedZone("east", 2*60*60))),
		broken,
		at(7, session.TypeUserMessage, message, day(7)),
		at(8, session.TypeSessionStatus, running, day(8)),
	}
	shuffled := slices.Clone(log)
	slices.Reverse(shuffled)
	for _, c := range []struct {
		name    string
		evs     []session.Event
		through uint64
		want    string
	}{
		{"the last running status through the fold point", log, 7, "2026-10-05"},
		{"in any order of the events", shuffled, 7, "2026-10-05"},
		{"an idle status opens no turn", log, 4, "2026-10-02"},
		{"a status past the fold point is not read", log, 2, "2026-10-02"},
		{"the whole log", log, 8, "2026-10-08"},
		{"no running status: the last event", log[:1], 1, "2026-10-01"},
		{"no running status through the fold point", log, 1, "2026-10-01"},
		{"no event through the fold point", log, 0, ""},
		{"an empty log", nil, 9, ""},
	} {
		if got := dateOf(c.evs, c.through); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := dateBlock("", nil); got != "" {
		t.Errorf("a request with no date carries the part %q", got)
	}
}

// TestAReplayReadsTheDateFromTheLog: a request is built again to its
// recorded hash by a harness whose clock is a month on, since its date
// is the log's and not the clock's (specs 007 and 011).
func TestAReplayReadsTheDateFromTheLog(t *testing.T) {
	e := setup(t, nil)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_1", "echo", `{"text":"a"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.send(ctx, "Work.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	later := e.cfg
	later.Clock = func() time.Time { return t0.AddDate(0, 1, 0) }
	h, err := New(later)
	if err != nil {
		t.Fatal(err)
	}
	steps := e.replay(ctx, h, e.all())
	if len(steps) != 2 || slices.ContainsFunc(steps, func(s models.ReplayStep) bool { return s.Outcome != models.ReplayMatch }) {
		t.Fatalf("replay %+v", steps)
	}
}
