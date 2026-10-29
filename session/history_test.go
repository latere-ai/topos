// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

// heads are the entries' heads and bodies, one string each, for a test
// to compare whole.
func heads(entries []session.HistoryEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Head
		if e.Body != "" {
			out[i] += "\n" + e.Body
		}
	}
	return out
}

// TestHistoryReadsTheLogAsWritten (spec 084): the entries of a thread's
// view hold a person's message with its files, the agent's text without
// its reasoning, each call with its input, each result with its outcome
// and its text, a picture by its path, an answer, an interrupt and a
// title, each with its sequence and time; a summary and a clearing
// change nothing, a summary and every other event are left out, and a
// redacted event is one line holding nothing of what it was.
func TestHistoryReadsTheLogAsWritten(t *testing.T) {
	b := &logb{t: t}
	b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: ada, Content: []lux.Block{txt("The staging port is 8443.")}, Attachments: []session.Attachment{
		{Name: "a.env", Path: "/work/attachments/evt_1/a.env"}, {Name: "b.csv", Path: "/work/attachments/evt_1/b.csv"},
	}})
	b.add(session.TypeModelRequest, "", 1, session.ModelRequest{Model: "m", Outcome: "ok"})
	b.assistant("", 1, false, lux.Block{Type: ir.BlockThinking, Text: "secret plan", Signature: "sig"}, txt("Reading."), use("toolu_r", "read"), use("toolu_b", "bash"))
	b.add(session.TypeAgentToolUse, "", 1, session.AgentToolUse{ToolUseID: "toolu_r", Name: "read", Input: json.RawMessage(`{"path":"plot.png"}`), Verdict: "allow"})
	b.toolUse("", 1, "toolu_b")
	b.add(session.TypeToolResult, "", 1, session.ToolResult{ToolUseID: "toolu_r", Content: []lux.Block{{Type: ir.BlockImage, Image: &lux.Image{MediaType: "image/png", Data: "aGk="}}}, Outcome: "ok", Meta: json.RawMessage(`{"path":"/work/plot.png"}`)})
	b.add(session.TypeToolResult, "", 1, session.ToolResult{ToolUseID: "toolu_b", Content: []lux.Block{txt("dial tcp: ECONNREFUSED")}, IsError: true})
	b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactClearToolResults, ToolUseIDs: []string{"toolu_b"}, FromSeq: 7, ToSeq: 7})
	b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 7, Summary: "Ada gave a port."})
	b.add(session.TypeUserInterrupt, "", 2, session.UserInterrupt{Sender: grace})
	b.add(session.TypeTitleChanged, "", 2, session.TitleChanged{By: ada, Old: "Staging", New: "Staging port"})
	b.add(session.TypeAgentToolUse, "", 2, session.AgentToolUse{ToolUseID: "toolu_q", Name: "question", Input: json.RawMessage(`{"questions":[]}`), Verdict: "allow"})
	b.add(session.TypeUserAnswer, "", 2, session.UserAnswer{Sender: ada, ToolUseID: "toolu_q", Answers: []session.AnswerEntry{{Selected: []string{"SQLite"}}, {}}})
	b.add(session.TypeSessionMachine, "", 2, session.SessionMachine{Context: "machine"})
	secret := b.user(grace, "the password is hunter2", 2)
	b.redact(secret)

	got, err := session.History(b.events, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Ada wrote:\nThe staging port is 8443.\nFiles: /work/attachments/evt_1/a.env, /work/attachments/evt_1/b.csv",
		"You wrote:\nReading.",
		"You called read, id toolu_r:\n{\"path\":\"plot.png\"}",
		"You called bash, id toolu_b:\n{\"command\":\"ls\"}",
		"read returned ok, id toolu_r:\n[a picture: /work/plot.png]",
		"bash returned error, id toolu_b:\ndial tcp: ECONNREFUSED",
		"Grace interrupted you.",
		"Ada changed the conversation's title to:\nStaging port",
		"You called question, id toolu_q:\n{\"questions\":[]}",
		"Ada answered your question, id toolu_q:\n1. Chosen: SQLite\n2. Left to you.",
		"This event was removed from the log; nothing of what it held is shown.",
	}
	if h := heads(got); !slices.Equal(h, want) {
		t.Fatalf("entries:\n%s\nwant:\n%s", strings.Join(h, "\n--\n"), strings.Join(want, "\n--\n"))
	}
	seqs := []uint64{1, 3, 4, 5, 6, 7, 10, 11, 12, 13, 15}
	for i, e := range got {
		if e.Seq != seqs[i] || !e.Time.Equal(t0.Add(time.Duration(seqs[i])*time.Second)) {
			t.Errorf("entry %d is #%d at %s", i, e.Seq, e.Time)
		}
	}
	calls := map[uint64][]string{4: {"toolu_r"}, 5: {"toolu_b"}, 6: {"toolu_r"}, 7: {"toolu_b"}, 12: {"toolu_q"}, 13: {"toolu_q"}}
	for _, e := range got {
		if !slices.Equal(e.Calls, calls[e.Seq]) {
			t.Errorf("#%d belongs to %v, want %v", e.Seq, e.Calls, calls[e.Seq])
		}
	}
	for _, e := range got {
		if strings.Contains(e.Head+e.Body, "hunter2") || strings.Contains(e.Body, "secret plan") || strings.Contains(e.Body, "Ada gave a port.") {
			t.Fatalf("#%d shows what it must not: %q", e.Seq, e.Body)
		}
	}

	// The read stops before the sequence it is given.
	early, err := session.History(b.events, "", 4)
	if err != nil || len(early) != 2 || early[1].Seq != 3 {
		t.Fatalf("through #3: %+v, %v", early, err)
	}
}

// TestHistoryReadsOneThread (spec 084): a spawned thread reads its task,
// its parent's messages and its own calls, and nothing of the session's
// own thread; the session's thread reads none of the spawned thread's.
func TestHistoryReadsOneThread(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "The code word is quartz.", 1)
	th := b.thread("", "Review the diff.", 1)
	b.assistant(th, 1, false, txt("Reviewing."))
	b.add(session.TypeSubagentMessage, th, 1, session.SubagentMessage{From: "", To: th, FromName: "builder", Content: []lux.Block{txt("Check go.mod too.")}})
	b.assistant("", 1, false, txt("Spawned."))

	sub, err := session.History(b.events, th, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"The agent that started you as its subagent gave you this task:\nReview the diff.", "You wrote:\nReviewing.", "builder wrote:\nCheck go.mod too."}
	if h := heads(sub); !slices.Equal(h, want) {
		t.Fatalf("the thread's entries %q", h)
	}
	root, err := session.History(b.events, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if h := heads(root); !slices.Equal(h, []string{"Ada wrote:\nThe code word is quartz.", "You wrote:\nSpawned."}) {
		t.Fatalf("the session's entries %q", h)
	}
}

// TestHistoryShowsACallNoToolUseRecords (spec 084): a call refused before
// it was decided has no agent.tool_use, so the message that made it shows
// it and belongs to it; a message cut off shows none of its calls, which
// never ran, and a message of calls alone, each recorded, shows nothing.
// A client's result reads with the outcome its error flag implies.
func TestHistoryShowsACallNoToolUseRecords(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "Go.", 1)
	b.assistant("", 1, false, use("toolu_bad", "bash"))
	b.add(session.TypeToolResult, "", 1, session.ToolResult{ToolUseID: "toolu_bad", Content: []lux.Block{txt("invalid input")}, Outcome: "invalid_input", IsError: true})
	b.assistant("", 1, false, use("toolu_ok", "deploy"))
	b.add(session.TypeAgentToolUse, "", 1, session.AgentToolUse{ToolUseID: "toolu_ok", Name: "deploy", Input: json.RawMessage(`{}`), Verdict: "allow", Client: true})
	b.add(session.TypeUserToolResult, "", 1, session.UserToolResult{Sender: ada, ToolUseID: "toolu_ok", Content: []lux.Block{txt("deployed")}, IsError: true})
	b.assistant("", 1, true, txt("Writ"), use("toolu_cut", "write"))

	got, err := session.History(b.events, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Ada wrote:\nGo.",
		"You wrote:\nYou called bash, id toolu_bad:\n{\"command\":\"ls\"}",
		"bash returned invalid_input, id toolu_bad:\ninvalid input",
		"You called deploy, id toolu_ok:\n{}",
		"deploy returned error, id toolu_ok:\ndeployed",
		"You wrote:\nWrit",
	}
	if h := heads(got); !slices.Equal(h, want) {
		t.Fatalf("entries:\n%s", strings.Join(h, "\n--\n"))
	}
	if !slices.Equal(got[1].Calls, []string{"toolu_bad"}) || len(got[5].Calls) != 0 {
		t.Fatalf("calls %v and %v", got[1].Calls, got[5].Calls)
	}
}

// TestHistoryRefusesAPayloadItCannotRead: an event of a type it reads
// whose payload does not decode is an error, as the fold's is.
func TestHistoryRefusesAPayloadItCannotRead(t *testing.T) {
	for _, typ := range []session.Type{
		session.TypeUserMessage, session.TypeSubagentMessage, session.TypeSubagentStarted, session.TypeAgentMessage,
		session.TypeAgentToolUse, session.TypeToolResult, session.TypeUserToolResult, session.TypeUserAnswer,
		session.TypeUserInterrupt, session.TypeTitleChanged,
	} {
		e := session.Event{ID: "evt_1", Seq: 1, Type: typ, Payload: json.RawMessage(`[]`)}
		if typ == session.TypeSubagentStarted || typ == session.TypeSubagentMessage {
			e.Subagent = "evt_1"
		}
		thread := e.Subagent
		if _, err := session.History([]session.Event{e}, thread, 10); err == nil {
			t.Errorf("%s: read a payload that does not decode", typ)
		}
	}
	// A meta that does not decode leaves a picture without its path.
	b := &logb{t: t}
	b.add(session.TypeToolResult, "", 1, session.ToolResult{ToolUseID: "toolu_r", Content: []lux.Block{{Type: ir.BlockImage, Image: &lux.Image{MediaType: "image/png", Data: "aGk="}}}, Meta: json.RawMessage(`"path"`)})
	got, err := session.History(b.events, "", 10)
	if err != nil || len(got) != 1 || got[0].Body != "[a picture]" {
		t.Fatalf("%+v, %v", got, err)
	}
}

// TestAFoldForHistoryNamesWhatItReadsBack (spec 084): for a thread that
// holds the history tool, a cleared result names its call's id and a
// summary the events it covers, the range of the summaries it merged;
// without the option each renders as before.
func TestAFoldForHistoryNamesWhatItReadsBack(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "first", 1)
	b.assistant("", 1, false, txt("reply one"))
	b.user(ada, "second", 2)
	b.assistant("", 2, false, use("toolu_big", "bash"))
	b.toolUse("", 2, "toolu_big")
	b.result("", 2, "toolu_big", "a long listing")
	b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactClearToolResults, ToolUseIDs: []string{"toolu_big"}})
	b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 2, Summary: "Ada asked once."})
	b.add(session.TypeContextCompacted, "", 2, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 2, ToSeq: 3, Summary: "Ada asked twice."})

	rendered := func(o session.FoldOptions) string {
		tr, err := session.FoldWith(b.events, "", o)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range tr.Messages {
			for _, blk := range m.Blocks {
				switch {
				case blk.Type == ir.BlockText:
					out = append(out, blk.Text)
				case blk.ToolResult != nil:
					out = append(out, blk.ToolResult.Blocks[0].Text)
				}
			}
		}
		return strings.Join(out, "\n")
	}
	with := rendered(session.FoldOptions{History: true})
	for _, want := range []string{
		"Summary of the conversation so far, events #1 to #3. The session's log keeps those events as they were written",
		"Ada asked twice.",
		`[this result was cleared to save context; history reads it back with {"tool_use_id": "toolu_big"}]`,
	} {
		if !strings.Contains(with, want) {
			t.Errorf("with history the fold does not hold %q:\n%s", want, with)
		}
	}
	without := rendered(session.FoldOptions{})
	for _, want := range []string{"Summary of the conversation so far:\n\nAda asked twice.", "[cleared to save context; run the tool again if the result is needed]"} {
		if !strings.Contains(without, want) {
			t.Errorf("without history the fold does not hold %q:\n%s", want, without)
		}
	}
	if strings.Contains(without, "history") {
		t.Fatalf("a fold without history names it:\n%s", without)
	}

	// A summary whose range holds no event of the view renders before the
	// next event, with its range all the same.
	c := &logb{t: t}
	c.user(ada, "start", 1)
	th := c.thread("", "Review the diff.", 1)
	c.assistant(th, 1, false, txt("reviewing"))
	c.add(session.TypeContextCompacted, "", 1, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 2, ToSeq: 3, Summary: "A reviewer ran."})
	c.user(ada, "next", 2)
	tr, err := session.FoldWith(c.events, "", session.FoldOptions{History: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := tr.Messages[0].Blocks[1].Text; !strings.HasPrefix(got, "Summary of the conversation so far, events #2 to #3.") {
		t.Fatalf("the summary renders %q", got)
	}
}
