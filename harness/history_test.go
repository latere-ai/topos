// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// withHistory offers the history tool, as a runner does for an agent
// that names it.
func withHistory(c *Config) { c.History = true }

// numbered is a log written in order: each event's sequence from 1 and
// its time a second after the one before, from t0.
type numbered struct {
	t   *testing.T
	evs []session.Event
}

func (l *numbered) add(typ session.Type, thread string, p any) uint64 {
	l.t.Helper()
	n := len(l.evs) + 1
	e, err := session.NewEvent(typ, p, t0.Add(time.Duration(n)*time.Second))
	if err != nil {
		l.t.Fatal(err)
	}
	e.Seq, e.Thread = uint64(n), thread
	l.evs = append(l.evs, e)
	return e.Seq
}

// at is the time of event n of a numbered log as an entry shows it.
func at(n int) string { return t0.Add(time.Duration(n) * time.Second).Format(time.RFC3339) }

// resultOf is the tool.result of call id.
func resultOf(t *testing.T, evs []session.Event, id string) session.ToolResult {
	t.Helper()
	for _, e := range evs {
		var p session.ToolResult
		if e.Type == session.TypeToolResult && e.Decode(&p) == nil && p.ToolUseID == id {
			return p
		}
	}
	t.Fatalf("no result of %s", id)
	return session.ToolResult{}
}

// TestHistoryNeverOpensAMachine (spec 084): a history call scores 0 and
// is allowed in every mode, plan included, and answers without opening a
// machine opened on demand.
func TestHistoryNeverOpensAMachine(t *testing.T) {
	props := tools.History(nil).Properties()
	input := json.RawMessage(`{"query":"port"}`)
	risk := Score(tools.NameHistory, props, input, machine.KindCella, nil)
	if risk.Score != 0 || opensMachine(props) {
		t.Fatalf("risk %+v, opens a machine %v", risk, opensMachine(props))
	}
	for _, mode := range []Mode{ModePlan, ModeConfirm, ModeProgressive} {
		if d := (Policy{Mode: mode}).Decide(tools.NameHistory, props, input, risk, machine.KindHost, nil); d.Verdict != VerdictAllow {
			t.Errorf("%s: %+v", mode, d)
		}
	}
	e := setup(t, func(c *Config) {
		withHistory(c)
		c.Policy.Mode = ModePlan
		c.Machine = machine.Defer(t.Context(), machine.KindCella, func(context.Context) (machine.Machine, error) {
			t.Error("a history call opened the machine")
			return nil, errors.New("not opened")
		})
	})
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_h", tools.NameHistory, `{"query":"port"}`)), reply(ir.StopEndTurn, text("8443")))
	e.send(ctx, "The port is 8443.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if n := len(e.events(ctx, session.TypeSessionMachine)); n != 0 {
		t.Fatalf("%d session.machine events", n)
	}
	if res := resultOf(t, e.all(), "toolu_h"); res.Outcome != tools.OutcomeOK || !strings.Contains(res.Content[0].Text, "The port is 8443.") {
		t.Fatalf("the result %+v", res)
	}
}

// TestHistoryReadsTheThreadsLogAsWritten (spec 084): each read answers
// the thread's own view through the event before the call: a range, a
// call with its result and a query, each as the events were written, a
// cleared result and a summarized stretch whole, the summary left out, a
// redacted event as one line, and nothing at or after the call.
func TestHistoryReadsTheThreadsLogAsWritten(t *testing.T) {
	e := setup(t, withHistory)
	ada := e.s.Initiator
	l := &numbered{t: t}
	l.add(session.TypeUserMessage, "", session.UserMessage{Sender: ada, Content: []lux.Block{{Type: ir.BlockText, Text: "The staging port is 8443."}}})
	l.add(session.TypeAgentMessage, "", session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockThinking, Text: "try curl", Signature: "sig"}, {Type: ir.BlockText, Text: "Checking."},
		{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_1", Name: "bash", Args: json.RawMessage(`{"command":"curl"}`)}},
	}}})
	l.add(session.TypeAgentToolUse, "", session.AgentToolUse{ToolUseID: "toolu_1", Name: "bash", Input: json.RawMessage(`{"command":"curl"}`), Verdict: "allow"})
	l.add(session.TypeToolResult, "", session.ToolResult{ToolUseID: "toolu_1", Content: []lux.Block{{Type: ir.BlockText, Text: "curl: ECONNREFUSED 127.0.0.1:8443"}}, Outcome: tools.OutcomeOK})
	l.add(session.TypeContextCompacted, "", session.ContextCompacted{Kind: session.CompactClearToolResults, ToolUseIDs: []string{"toolu_1"}, FromSeq: 4, ToSeq: 4})
	l.add(session.TypeContextCompacted, "", session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 4, Summary: "Ada asked about staging."})
	secret := l.add(session.TypeUserMessage, "", session.UserMessage{Sender: ada, Content: []lux.Block{{Type: ir.BlockText, Text: "the token is s3cr3t"}}})
	l.evs[secret-1].Payload = json.RawMessage(`{"tombstone":true}`)
	l.add(session.TypeEventRedacted, "", session.EventRedacted{EventID: l.evs[secret-1].ID, By: ada})
	l.add(session.TypeAgentMessage, "", session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
		{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_h", Name: tools.NameHistory, Args: json.RawMessage(`{}`)}},
	}}})
	l.add(session.TypeAgentToolUse, "", session.AgentToolUse{ToolUseID: "toolu_h", Name: tools.NameHistory, Input: json.RawMessage(`{}`), Verdict: "allow"})
	l.add(session.TypeUserMessage, "", session.UserMessage{Sender: ada, Content: []lux.Block{{Type: ir.BlockText, Text: "a message after the call"}}})

	tu := &turn{h: e.h, s: e.s, sh: &shared{events: l.evs}}
	read := func(r tools.HistoryRead) string {
		t.Helper()
		out, err := tu.history(t.Context(), tools.Call{ID: "toolu_h"}, r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	entries := []string{
		"[#1 " + at(1) + "] Ada wrote:\nThe staging port is 8443.",
		"[#2 " + at(2) + "] You wrote:\nChecking.",
		"[#3 " + at(3) + "] You called bash, id toolu_1:\n{\"command\":\"curl\"}",
		"[#4 " + at(4) + "] bash returned ok, id toolu_1:\ncurl: ECONNREFUSED 127.0.0.1:8443",
		"[#7 " + at(7) + "] This event was removed from the log; nothing of what it held is shown.",
	}
	for _, c := range []struct {
		read tools.HistoryRead
		want string
	}{
		{tools.HistoryRead{}, strings.Join(entries, "\n\n")},
		{tools.HistoryRead{From: 2, To: 3}, strings.Join(entries[1:3], "\n\n")},
		{tools.HistoryRead{From: 4, To: 400}, strings.Join(entries[3:], "\n\n")},
		{tools.HistoryRead{ToolUseID: "toolu_1"}, strings.Join(entries[2:4], "\n\n")},
		{tools.HistoryRead{Query: "econnrefused"}, entries[3]},
		{tools.HistoryRead{Query: "8443", From: 4}, entries[3]},
		{tools.HistoryRead{Query: "s3cr3t"}, `No event from #1 to #9 holds "s3cr3t".`},
		{tools.HistoryRead{Query: "a message after the call"}, `No event from #1 to #9 holds "a message after the call".`},
		{tools.HistoryRead{ToolUseID: "toolu_h"}, "History finds no call with the id toolu_h in this conversation before this call."},
		{tools.HistoryRead{From: 50}, "History shows no event from #50; the conversation before this call runs to #9."},
		{tools.HistoryRead{From: 5, To: 6}, "History shows no event from #5 to #6; the conversation before this call runs to #9."},
	} {
		if got := read(c.read); got != c.want {
			t.Errorf("%+v:\n%s\nwant:\n%s", c.read, got, c.want)
		}
	}
	// A call the log does not record reads through the log's end.
	out, err := tu.history(t.Context(), tools.Call{ID: "toolu_unrecorded"}, tools.HistoryRead{Query: "after the call"})
	if err != nil || !strings.Contains(out, "a message after the call") {
		t.Fatalf("a call the log does not record: %q, %v", out, err)
	}
	// A log the fold cannot read is the harness's failure.
	broken := slices.Clone(l.evs)
	broken[0].Payload = json.RawMessage(`[]`)
	if _, err := (&turn{h: e.h, s: e.s, sh: &shared{events: broken}}).history(t.Context(), tools.Call{ID: "toolu_h"}, tools.HistoryRead{}); err == nil {
		t.Fatal("a payload that does not decode was read")
	}
}

// TestHistoryReadsBeforeItsOwnCall (spec 084): a history call beside
// another call of its step reads that call, and not its result, however
// the two run; the read reaches the model as the call's result.
func TestHistoryReadsBeforeItsOwnCall(t *testing.T) {
	e := setup(t, withHistory)
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_e", "echo", `{"text":"probe"}`), call("toolu_h", tools.NameHistory, `{}`)),
		luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("The port is 8443.")}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
			last := r.Messages[len(r.Messages)-1]
			for _, b := range last.Blocks {
				if b.ToolResult != nil && b.ToolResult.ToolUseID == "toolu_h" && strings.Contains(b.ToolResult.Blocks[0].Text, "The port is 8443.") {
					return nil
				}
			}
			return errors.New("the model did not read the history")
		}},
	)
	e.send(ctx, "The port is 8443.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	got := resultOf(t, e.all(), "toolu_h").Content[0].Text
	if !strings.Contains(got, "You called echo, id toolu_e:\n{\"text\":\"probe\"}") || strings.Contains(got, "echo probe") || strings.Contains(got, "toolu_h") {
		t.Fatalf("the history read past its call:\n%s", got)
	}
}

// TestEveryThreadOfTheAgentHoldsHistory (spec 084): an agent that names
// history holds it in its own thread and in a thread it spawns, which
// reads its own log and never the session's thread; a subagent whose
// tools leave it out, the advisor, and an agent that does not name it
// hold none, and a client's tool of the name stays the client's.
func TestEveryThreadOfTheAgentHoldsHistory(t *testing.T) {
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = nil })(c)
		withHistory(c)
		withAdvisor(c)
	})
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)),
		reply(ir.StopEndTurn, text("Reviewed.")),
	)
	e.stub.Script(reviewerModel,
		luxstub.Reply{Response: ir.Response{Model: reviewerModel, Blocks: []ir.Block{call("toolu_rh", tools.NameHistory, `{}`)}, StopReason: ir.StopToolUse}, Expect: func(r *ir.Request) error {
			for _, tool := range r.Tools {
				if tool.Name == tools.NameHistory {
					return nil
				}
			}
			return errors.New("the thread was not offered history")
		}},
		reply(ir.StopEndTurn, text("Fine.")),
	)
	e.send(ctx, "The code word is quartz.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	evs := e.all()
	_, started := e.thread(evs)
	if !slices.Contains(started.Tools, tools.NameHistory) {
		t.Fatalf("the thread was started with %v", started.Tools)
	}
	got := resultOf(t, evs, "toolu_rh").Content[0].Text
	if !strings.Contains(got, "The thread that started you gave you this task:\nReview main.go.") || strings.Contains(got, "quartz") {
		t.Fatalf("the thread read:\n%s", got)
	}

	holds := func(tu *turn, names []string) bool {
		t.Helper()
		r, err := tu.registry(names)
		if err != nil {
			t.Fatal(err)
		}
		tu.reg = r
		return tu.readsHistory()
	}
	root := &turn{h: e.h, s: e.s, root: e.cfg.Tools, sh: &shared{}}
	if !holds(root, e.cfg.Tools.Names()) {
		t.Fatal("the session's thread does not hold history")
	}
	if holds(&turn{h: e.h, s: e.s, root: e.cfg.Tools, thread: "evt_t"}, []string{"echo"}) {
		t.Fatal("a thread whose tools leave history out holds it")
	}
	adv, err := root.child("evt_a", 1, root.advisorConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, held := adv.reg.Get(tools.NameHistory); held {
		t.Fatal("the advisor holds history")
	}
	plain := setup(t, nil)
	if holds(&turn{h: plain.h, s: plain.s, root: plain.cfg.Tools}, plain.cfg.Tools.Names()) {
		t.Fatal("an agent that does not name history holds it")
	}
	client := &fakeTool{name: tools.NameHistory, props: tools.Properties{Client: true}}
	if err := e.cfg.Tools.Add(client); err != nil {
		t.Fatal(err)
	}
	if holds(&turn{h: e.h, s: e.s, root: e.cfg.Tools}, e.cfg.Tools.Names()) {
		t.Fatal("a client's tool of the name was taken for history")
	}
	if (&turn{}).readsHistory() {
		t.Fatal("a turn with no registry holds history")
	}
}

// TestAHistoryCallRunsAgainOnResume (spec 084): a history call a runner
// stopped under is run again, not answered as of unknown effect: it
// changes nothing and answers the same from the same log.
func TestAHistoryCallRunsAgainOnResume(t *testing.T) {
	e := setup(t, withHistory)
	ctx := t.Context()
	e.send(ctx, "The port is 8443.")
	msg, err := session.NewEvent(session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_h", Name: tools.NameHistory, Args: json.RawMessage(`{}`)}}}}, StopReason: ir.StopToolUse}, t0)
	if err != nil {
		t.Fatal(err)
	}
	use, err := session.NewEvent(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_h", Name: tools.NameHistory, Input: json.RawMessage(`{"query":"port"}`), Verdict: "allow", Repeatable: true}, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.appendEvents(ctx, msg, use)
	e.running(ctx)
	e.stub.Script(model, reply(ir.StopEndTurn, text("8443")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := resultOf(t, e.all(), "toolu_h"); res.Outcome != tools.OutcomeOK || !strings.Contains(res.Content[0].Text, "The port is 8443.") {
		t.Fatalf("the result %+v", res)
	}
}

// entriesOf are n entries of seqs from 1, each body size bytes holding
// its number.
func entriesOf(n, size int) []session.HistoryEntry {
	out := make([]session.HistoryEntry, n)
	for i := range out {
		body := fmt.Sprintf("entry %d ", i+1)
		out[i] = session.HistoryEntry{Seq: uint64(i + 1), Time: t0, Head: "You wrote:", Body: body + strings.Repeat("x", max(size-len(body), 0))}
	}
	return out
}

// TestHistoryAnswersStopAtTheLimit (spec 084): an answer holds whole
// entries up to the output limit and closes with the input that reads
// on, which, followed, reads every entry once; an entry longer than the
// limit is cut with a line that says where; no answer passes the limit.
func TestHistoryAnswersStopAtTheLimit(t *testing.T) {
	entries := entriesOf(10, 1000)
	got, err := historyAnswer(entries, tools.HistoryRead{From: 2}, 10, 3500)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "\n\n"+`[this answer stops here; read on with {"from":5,"to":10}]`) || strings.Count(got, "] You wrote:") != 3 || len(got) > 3500 {
		t.Fatalf("an answer past the limit (%d bytes):\n%s", len(got), got)
	}

	// Followed, the input that reads on reads every entry once, whatever
	// the limit, and no answer passes it.
	sizes := []int{40, 900, 120, 3000, 15, 600, 2500, 80, 1200, 300, 7000, 50}
	var mixed []session.HistoryEntry
	for i, n := range sizes {
		e := entriesOf(1, n)[0]
		e.Seq, e.Body = uint64(i+1), fmt.Sprintf("entry %d ", i+1)+strings.Repeat("x", n)
		mixed = append(mixed, e)
	}
	for _, limit := range []int{1500, 2000, 3333, 5000, 9000, 40000} {
		seen := map[uint64]int{}
		r := tools.HistoryRead{}
		for reads := 0; ; reads++ {
			if reads > len(mixed)+1 {
				t.Fatalf("limit %d: the reads do not end", limit)
			}
			out, err := historyAnswer(mixed, r, uint64(len(mixed)), limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) > limit {
				t.Fatalf("limit %d: an answer of %d bytes", limit, len(out))
			}
			for _, e := range mixed {
				if strings.Contains(out, fmt.Sprintf("[#%d ", e.Seq)) {
					seen[e.Seq]++
				}
			}
			i := strings.LastIndex(out, "read on with ")
			if i < 0 {
				break
			}
			r = tools.HistoryRead{}
			if err := json.Unmarshal([]byte(strings.TrimSuffix(out[i+len("read on with "):], "]")), &r); err != nil {
				t.Fatal(err)
			}
		}
		for _, e := range mixed {
			if seen[e.Seq] != 1 {
				t.Fatalf("limit %d: #%d was read %d times", limit, e.Seq, seen[e.Seq])
			}
		}
	}

	// An entry longer than the limit alone is cut where the line says.
	long := entriesOf(2, 9000)
	long[0].Body = "é" + long[0].Body
	cutAnswer, err := historyAnswer(long, tools.HistoryRead{}, 2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if len(cutAnswer) > 4096 || !utf8.ValidString(cutAnswer) || !strings.HasSuffix(cutAnswer, `[this answer stops here; read on with {"from":2,"to":2}]`) {
		t.Fatalf("a long entry (%d bytes):\n%s", len(cutAnswer), cutAnswer)
	}
	whole := prompts.Render(prompts.HistoryEntry, prompts.Data{"Seq": long[0].Seq, "Time": stamp(long[0].Time), "Head": long[0].Head, "Body": long[0].Body})
	var kept int
	if _, err := fmt.Sscanf(cutAnswer[strings.Index(cutAnswer, "[cut here"):], "[cut here: this entry holds %d bytes, and the answer shows its first %d]", new(int), &kept); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cutAnswer, whole[:kept]+"\n[cut here: this entry holds "+fmt.Sprint(len(whole))+" bytes") {
		t.Fatalf("the cut line does not say where:\n%s", cutAnswer[:200])
	}
	// A cut never splits a character, and what it leaves fits its room.
	for room := 90; room < 110; room++ {
		if out := cut(strings.Repeat("é", 100), room); !utf8.ValidString(out) || len(out) > room {
			t.Fatalf("room %d: %q", room, out)
		}
	}
	// The last entry alone, too long, is cut and closes with nothing more.
	if last, err := historyAnswer(long, tools.HistoryRead{From: 2}, 2, 4096); err != nil || len(last) > 4096 || strings.Contains(last, "read on") || !strings.Contains(last, "[cut here") {
		t.Fatalf("the last entry cut: %v\n%s", err, last)
	}
	// A call whose answer stops after the call reads on to its result.
	call := entriesOf(3, 3000)
	call[0].Calls, call[2].Calls = []string{"toolu_1"}, []string{"toolu_1"}
	byID, err := historyAnswer(call, tools.HistoryRead{ToolUseID: "toolu_1"}, 3, 4000)
	if err != nil || !strings.HasSuffix(byID, `read on with {"from":3,"to":3}]`) || strings.Contains(byID, "[#2 ") {
		t.Fatalf("a call's read: %v\n%s", err, byID)
	}
}

// TestHistoryQueriesAreBounded (spec 084): a query answers the entries
// whose text holds the words, case folded, in order, each with at most
// HistoryMatchChars characters around its first match, at most
// HistoryMatches of them, and closes with the input that reads on.
func TestHistoryQueriesAreBounded(t *testing.T) {
	var entries []session.HistoryEntry
	for i := range 25 {
		pad := strings.Repeat("a", i*50)
		entries = append(entries, session.HistoryEntry{Seq: uint64(2 * (i + 1)), Time: t0, Head: "bash returned error:", Body: pad + "dial: ECONNREFUSED here" + strings.Repeat("z", 600)})
	}
	entries = append(entries, session.HistoryEntry{Seq: 60, Time: t0, Head: "You wrote:", Body: "nothing to see"})
	got, err := historyAnswer(entries, tools.HistoryRead{Query: "econnrefused"}, 60, tools.DefaultOutputLimit)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(got, "] bash returned error:"); n != HistoryMatches {
		t.Fatalf("%d matches", n)
	}
	if !strings.HasSuffix(got, `[this answer stops here; read on with {"to":60,"query":"econnrefused","from":42}]`) && !strings.HasSuffix(got, `[this answer stops here; read on with {"from":42,"to":60,"query":"econnrefused"}]`) {
		t.Fatalf("the answer closes with:\n%s", got[len(got)-200:])
	}
	for _, m := range strings.Split(got, "\n\n")[:HistoryMatches] {
		_, excerpt, _ := strings.Cut(m, "\n")
		excerpt = strings.TrimSuffix(strings.TrimPrefix(excerpt, "..."), "...")
		if n := utf8.RuneCountInString(excerpt); n > HistoryMatchChars || !strings.Contains(excerpt, "ECONNREFUSED") {
			t.Fatalf("an excerpt of %d characters: %q", n, excerpt)
		}
	}
	if first, _, _ := strings.Cut(got, "\n\n"); !strings.HasPrefix(first, "[#2 ") || strings.Contains(first, "\n...") || !strings.HasSuffix(first, "...") {
		t.Fatalf("the first match, at its entry's start: %q", first)
	}
	// A match the output limit stops before is the one it reads on from.
	small, err := historyAnswer(entries, tools.HistoryRead{Query: "ECONNREFUSED", From: 10, To: 20}, 60, 1200)
	if err != nil || strings.Count(small, "] bash returned") != 2 || !strings.HasSuffix(small, `read on with {"from":14,"to":20,"query":"ECONNREFUSED"}]`) {
		t.Fatalf("a query past the limit: %v\n%s", err, small)
	}
	// Case folding holds beyond ASCII, and a match's place is its own.
	ex, before, after, ok := around("Die ÉCOLE de Paris", "école", 7)
	if !ok || ex != " ÉCOLE " || !before || !after {
		t.Fatalf("%q %v %v %v", ex, before, after, ok)
	}
	if ex, _, _, ok := around("short", "SHORT TEXT", 4); ok || ex != "" {
		t.Fatalf("a query longer than the text matched: %q", ex)
	}
	if ex, before, after, ok := around("abcdefghij", "cdef", 2); !ok || ex != "cd" || !before || !after {
		t.Fatalf("a match wider than the excerpt: %q %v %v", ex, before, after)
	}
	if _, _, _, ok := around("anything", "", 10); ok {
		t.Fatal("an empty query matched")
	}
	if none, err := historyAnswer(entries, tools.HistoryRead{Query: "ENOENT", To: 30}, 60, tools.DefaultOutputLimit); err != nil || none != `No event from #1 to #30 holds "ENOENT".` {
		t.Fatalf("no match: %q, %v", none, err)
	}
}

// flat is a request's messages as one list of blocks, each its role and
// what it holds, for a test to compare prefixes.
func flat(r *ir.Request) []string {
	var out []string
	for _, m := range r.Messages {
		for _, b := range m.Blocks {
			s := string(m.Role) + ":" + string(b.Type) + ":" + b.Text
			if b.ToolUse != nil {
				s += b.ToolUse.ID + string(b.ToolUse.Args)
			}
			if b.ToolResult != nil {
				s += b.ToolResult.ToolUseID
				for _, in := range b.ToolResult.Blocks {
					s += in.Text
				}
			}
			out = append(out, s)
		}
	}
	return out
}

// TestHistoryTextsNameWhatItReadsBack (spec 084): in a thread that holds
// history, a cleared result names its call's id and the request offers
// the tool; a compaction asks with compact-v3, which its
// context.compacted names, over the transcript its steps sent, with no
// sequence number added, and the summary then names its range. A thread
// without the tool reads each as before.
func TestHistoryTextsNameWhatItReadsBack(t *testing.T) {
	t.Run("cleared", func(t *testing.T) {
		for _, with := range []bool{true, false} {
			e := setup(t, func(c *Config) { window(8000)(c); c.History = with })
			ctx := t.Context()
			e.history(ctx, 12000, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
			e.stub.Script(model, reply(ir.StopEndTurn, text("Done.")))
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			req := e.stub.Requests()[0].Request
			body := strings.Join(flat(req), "\n")
			cleared := `[this result was cleared to save context; history reads it back with {"tool_use_id": "toolu_h00"}]`
			offered := slices.ContainsFunc(req.Tools, func(d ir.Tool) bool { return d.Name == tools.NameHistory })
			if with != (strings.Contains(body, cleared) && offered) || with == strings.Contains(body, prompts.Text(prompts.TranscriptCleared)) {
				t.Fatalf("history %v: offered %v, the request:\n%s", with, offered, body)
			}
		}
	})
	t.Run("summary", func(t *testing.T) {
		for _, with := range []bool{true, false} {
			e := setup(t, func(c *Config) { window(8000)(c); c.History = with })
			ctx := t.Context()
			e.stub.Script(model,
				reply(ir.StopToolUse, call("toolu_e", "echo", `{"text":"probe"}`)),
				reply(ir.StopEndTurn, text("Ready.")),
				luxstub.Reply{Response: ir.Response{Model: model, Blocks: []ir.Block{text("Requests: the list. Next step: finish it.")}, StopReason: ir.StopEndTurn, Usage: ir.Usage{InputTokens: 9000, OutputTokens: 30}}},
				reply(ir.StopEndTurn, text("Finished.")),
			)
			e.send(ctx, "Start.")
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			e.history(ctx, 8000, 8000, 8000, 8000, 40)
			if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
				t.Fatalf("outcome %+v", out)
			}
			reqs := e.stub.Requests()
			if len(reqs) != 4 {
				t.Fatalf("%d requests", len(reqs))
			}
			step, compaction, after := flat(reqs[1].Request), flat(reqs[2].Request), strings.Join(flat(reqs[3].Request), "\n")
			ask := prompts.Text(prompts.Compaction)
			if with {
				ask = prompts.Text(prompts.CompactionHistory)
			}
			if last := compaction[len(compaction)-1]; last != "user:text:"+ask {
				t.Fatalf("history %v: the compaction asks with %q", with, last)
			}
			// The compaction's request opens with the transcript the steps
			// sent, so the provider's cache serves it.
			if len(step) < 3 || !slices.Equal(compaction[:len(step)], step) {
				t.Fatalf("history %v: the compaction's transcript is not the one its steps sent:\n%q\n%q", with, compaction, step)
			}
			var sum session.ContextCompacted
			for _, c := range e.compactions(ctx) {
				if c.Kind == session.CompactSummary {
					sum = c
				}
			}
			want := string(prompts.Compaction)
			if with {
				want = string(prompts.CompactionHistory)
			}
			if sum.Prompt != want {
				t.Fatalf("history %v: the summary names %q", with, sum.Prompt)
			}
			named := fmt.Sprintf("Summary of the conversation so far, events #%d to #%d. The session's log keeps those events", sum.FromSeq, sum.ToSeq)
			if with != strings.Contains(after, named) || with == strings.Contains(after, "Summary of the conversation so far:\n\n") {
				t.Fatalf("history %v: the request after the summary:\n%.600s", with, after)
			}
			// Each request, the summary's among them, is built again from
			// the log with the prompt the summary names.
			if got := outcomes(e.replay(ctx, e.h, e.all())); len(got) != 4 || slices.ContainsFunc(got, func(o string) bool { return o != models.ReplayMatch }) {
				t.Fatalf("history %v: replay %v", with, got)
			}
		}
	})
}

// TestRebuildRendersTheHistoryTexts (spec 084): every request of a
// session whose threads held history, a step after a clearing, a
// compaction's summary request and the steps after it, and a spawned
// thread's, is built again to the bytes it was sent with; a
// configuration without the tool builds them otherwise.
func TestRebuildRendersTheHistoryTexts(t *testing.T) {
	e := setup(t, func(c *Config) {
		window(8000)(c)
		withHistory(c)
		withReviewer(func(s *Subagent) { s.Tools = []string{"echo", tools.NameHistory} })(c)
	})
	ctx := t.Context()
	e.history(ctx, 12000, 12000, 40, 40, 40, 40, 40, 40, 40, 40, 40, 40)
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Review main.go."}`)),
		reply(ir.StopToolUse, call("toolu_h", tools.NameHistory, `{"tool_use_id":"toolu_h00"}`)),
		reply(ir.StopEndTurn, text("Done.")),
	)
	e.stub.Script(reviewerModel, reply(ir.StopToolUse, call("toolu_rh", tools.NameHistory, `{}`)), reply(ir.StopEndTurn, text("Fine.")))
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := resultOf(t, e.all(), "toolu_h"); !strings.Contains(res.Content[0].Text, strings.Repeat("r", 100)) {
		t.Fatalf("the cleared result was not read back: %.200s", res.Content[0].Text)
	}
	log := e.all()
	steps := e.replay(ctx, e.h, log)
	if len(steps) != 5 || slices.ContainsFunc(steps, func(s models.ReplayStep) bool { return s.Outcome != models.ReplayMatch }) {
		t.Fatalf("replay %+v", steps)
	}
	cfg := e.cfg
	cfg.History = false
	without, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(e.replay(ctx, without, log)); !slices.Contains(got, models.ReplayMismatch) {
		t.Fatalf("a configuration without history rebuilt the same requests: %v", got)
	}
}
