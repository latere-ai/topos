// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

var firing = session.Sender{Subject: session.TriggerSubjectPrefix + "trg_1", Kind: session.SenderTrigger}

func (b *logb) status(st session.Status, reason session.StopReason, detail string) string {
	return b.add(session.TypeSessionStatus, "", 0, session.SessionStatus{Status: st, StopReason: reason, Detail: detail})
}

func (b *logb) request(namesReads bool) string {
	return b.add(session.TypeModelRequest, "", 0, session.ModelRequest{Model: "m", Outcome: "ok", FoldSeq: uint64(len(b.events)), NamesReads: namesReads})
}

func (b *logb) read(ids ...string) string {
	return b.add(session.TypeMessagesRead, "", 0, session.MessagesRead{EventIDs: ids})
}

func (b *logb) withdraw(s session.Sender, id string) string {
	return b.add(session.TypeUserWithdraw, "", 0, session.UserWithdraw{Sender: s, EventID: id})
}

// lines renders each message of a transcript as "role: text|text", a
// tool result as "result".
func lines(t *testing.T, events []session.Event) []string {
	t.Helper()
	tr, err := session.Fold(events, "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range tr.Messages {
		var parts []string
		for _, b := range m.Blocks {
			switch b.Type {
			case ir.BlockText:
				parts = append(parts, b.Text)
			case ir.BlockToolUse:
				parts = append(parts, "use")
			case ir.BlockToolResult:
				parts = append(parts, "result")
			}
		}
		out = append(out, string(m.Role)+": "+strings.Join(parts, "|"))
	}
	return out
}

func sameTexts(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("transcript\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// aStep is the start of a turn whose first answer calls a tool.
func aStep(b *logb) {
	b.user(ada, "Start the server.", 1)
	b.status(session.StatusRunning, "", "")
}

func (b *logb) calls(id string) {
	b.assistant("", 1, false, use(id, "bash"))
	b.toolUse("", 1, id)
}

func TestAFoldWithoutReadsIsUnchanged(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(false)
	b.calls("toolu_1")
	mid := b.user(ada, "Use port 9000.", 1)
	b.result("", 1, "toolu_1", "started")
	b.request(false)
	b.assistant("", 1, false, txt("On 9000."))
	b.status(session.StatusIdle, session.StopEndTurn, "")
	sameTexts(t, lines(t, b.events), []string{
		"user: Start the server.", "assistant: use", "user: result|Use port 9000.", "assistant: On 9000.",
	})
	w := session.WaitingMessages(b.events)
	if len(w) != 1 || w[0].Event.ID != mid || w[0].State != session.MessageRead || w[0].ReadAt != 0 {
		t.Fatalf("waiting %+v", w)
	}
	if session.HasPendingInput(b.events) {
		t.Fatal("a message a request read before spec 098 is pending")
	}
}

func TestAReadMessageRendersWhereItWasRead(t *testing.T) {
	t.Run("read by the next step, after the answer it arrived during", func(t *testing.T) {
		b := &logb{t: t}
		aStep(b)
		mid := b.user(ada, "Use port 9000.", 1)
		b.request(true)
		b.calls("toolu_1")
		b.result("", 1, "toolu_1", "started")
		b.read(mid)
		b.request(true)
		b.assistant("", 1, false, txt("On 9000."))
		b.status(session.StatusIdle, session.StopEndTurn, "")
		sameTexts(t, lines(t, b.events), []string{
			"user: Start the server.", "assistant: use",
			"user: result|Ada sent this while you were working:|Use port 9000.", "assistant: On 9000.",
		})
	})
	t.Run("read by the next turn, after the turn's last answer", func(t *testing.T) {
		b := &logb{t: t}
		aStep(b)
		mid := b.user(ada, "Also add a test.", 1)
		b.request(true)
		b.assistant("", 1, false, txt("Started."))
		b.status(session.StatusIdle, session.StopEndTurn, "")
		if !session.HasPendingInput(b.events) {
			t.Fatal("a message the turn never read is no pending input")
		}
		b.status(session.StatusRunning, "", "")
		b.read(mid)
		sameTexts(t, lines(t, b.events), []string{
			"user: Start the server.", "assistant: Started.", "user: Also add a test.",
		})
	})
	t.Run("a summary covers it at its read", func(t *testing.T) {
		b := &logb{t: t}
		aStep(b)
		mid := b.user(ada, "Use port 9000.", 1)
		b.request(true)
		b.calls("toolu_1")
		b.result("", 1, "toolu_1", "started")
		b.add(session.TypeContextCompacted, "", 1, session.ContextCompacted{Kind: session.CompactSummary, FromSeq: 1, ToSeq: 3, Summary: "Ada asked for a server."})
		b.read(mid)
		got := lines(t, b.events)
		if !strings.Contains(strings.Join(got, "\n"), "Use port 9000.") {
			t.Fatalf("a summary of the message's own place covered its read: %q", got)
		}
	})
	t.Run("read again where it was read", func(t *testing.T) {
		b := &logb{t: t}
		aStep(b)
		b.request(true)
		b.calls("toolu_1")
		mid := b.user(ada, "Use port 9000.", 1)
		b.result("", 1, "toolu_1", "started")
		b.read(mid)
		before := lines(t, b.events)
		b.request(true)
		b.assistant("", 1, false, txt("On 9000."))
		after := lines(t, b.events)
		sameTexts(t, after[:len(before)], before)
	})
}

func TestTheSentNowLineLeadsAMessageThatCutItsTurn(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	now := b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: ada, Content: []lux.Block{txt("Stop, use 9000.")}, SendNow: true})
	b.status(session.StatusIdle, session.StopInterrupted, session.DetailSendNow)
	b.status(session.StatusRunning, "", "")
	b.read(now)
	sameTexts(t, lines(t, b.events), []string{
		"user: Start the server.|Ada stopped your work to send this:|Stop, use 9000.",
	})
}

func TestAFiringWaitsWhileItsTurnRuns(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	b.calls("toolu_1")
	fired := b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: firing, Content: []lux.Block{txt("Check the deploy.")}, FiringID: "fir_1"})
	b.result("", 1, "toolu_1", "started")
	if got := lines(t, b.events); strings.Contains(strings.Join(got, "\n"), "Check the deploy.") {
		t.Fatalf("an unread firing renders: %q", got)
	}
	b.request(true)
	b.assistant("", 1, false, txt("Started."))
	b.status(session.StatusIdle, session.StopEndTurn, "")
	if !session.HasPendingInput(b.events) {
		t.Fatal("a firing the turn held is no pending input")
	}
	b.status(session.StatusRunning, "", "")
	b.read(fired)
	got := lines(t, b.events)
	if last := got[len(got)-1]; !strings.HasSuffix(last, "|Check the deploy.") || strings.Contains(last, "while you were working") {
		t.Fatalf("the firing reads %q, not after the turn's answer as a message of its own", last)
	}
}

func TestAnUnreadWaitingMessageIsPendingInput(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	mid := b.user(ada, "One more thing.", 1)
	b.request(true)
	b.status(session.StatusIdle, session.StopEndTurn, "")
	if !session.HasPendingInput(b.events) {
		t.Fatal("unread")
	}
	read := &logb{t: t, events: append([]session.Event{}, b.events...)}
	read.status(session.StatusRunning, "", "")
	read.read(mid)
	read.request(true)
	read.status(session.StatusIdle, session.StopEndTurn, "")
	if session.HasPendingInput(read.events) {
		t.Fatal("a read message is pending input")
	}
	gone := &logb{t: t, events: append([]session.Event{}, b.events...)}
	gone.withdraw(ada, mid)
	if session.HasPendingInput(gone.events) {
		t.Fatal("a withdrawn message is pending input")
	}
}

func TestAHeldMessageWaitsWithTheSession(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	b.assistant("", 1, false, use("toolu_q", session.ToolQuestion))
	b.add(session.TypeAgentToolUse, "", 1, session.AgentToolUse{ToolUseID: "toolu_q", Name: session.ToolQuestion, Input: json.RawMessage(`{"questions":[{"header":"Port","question":"Which port?","options":[{"label":"8000","description":"The default."},{"label":"9000","description":"Free."}]}]}`), Verdict: "allow"})
	b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: firing, Content: []lux.Block{txt("Check the deploy.")}, FiringID: "fir_1"})
	b.status(session.StatusIdle, session.StopQuestion, "")
	if _, open := session.OpenQuestion(b.events); !open {
		t.Fatal("a firing closed the person's question")
	}
	if session.HasPendingInput(b.events) {
		t.Fatal("a held firing is pending input while the session waits for the person")
	}
}

func TestAWithdrawnMessageIsNotRead(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	b.calls("toolu_1")
	mid := b.user(ada, "Use port 9000.", 1)
	b.withdraw(ada, mid)
	b.result("", 1, "toolu_1", "started")
	b.read(mid)
	w := session.WaitingMessages(b.events)
	if len(w) != 1 || w[0].State != session.MessageWithdrawn {
		t.Fatalf("waiting %+v", w)
	}
	if got := lines(t, b.events); strings.Contains(strings.Join(got, "\n"), "port 9000") {
		t.Fatalf("a withdrawn message renders: %q", got)
	}
	read := &logb{t: t}
	aStep(read)
	m := read.user(ada, "Use port 9000.", 1)
	read.read(m)
	read.withdraw(ada, m)
	if w := session.WaitingMessages(read.events); w[0].State != session.MessageRead {
		t.Fatalf("a withdrawal after the read took a read message back: %+v", w)
	}
}

func TestAMessageSentWhileIdleIsNotWaiting(t *testing.T) {
	b := &logb{t: t}
	b.user(ada, "Hello.", 1)
	b.status(session.StatusRunning, "", "")
	b.request(true)
	b.assistant("", 1, false, txt("Hi."))
	b.status(session.StatusIdle, session.StopEndTurn, "")
	b.user(ada, "Again.", 2)
	if w := session.WaitingMessages(b.events); len(w) != 0 {
		t.Fatalf("waiting %+v", w)
	}
	sameTexts(t, lines(t, b.events), []string{"user: Hello.", "assistant: Hi.", "user: Again."})
}

func TestAWaitingMessagesFilesWaitWithIt(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	b.calls("toolu_1")
	with := func(name string) string {
		return b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: ada, Content: []lux.Block{txt("See " + name + ".")}, Attachments: []session.Attachment{{Name: name, Path: session.AttachmentDir + "/" + name}}})
	}
	read, gone := with("a.txt"), with("b.txt")
	paths := func() []string {
		var out []string
		for _, a := range session.PendingAttachments(b.events) {
			out = append(out, a.Name)
		}
		return out
	}
	if got := paths(); len(got) != 0 {
		t.Fatalf("an unread message's files are pending: %q", got)
	}
	b.withdraw(ada, gone)
	b.result("", 1, "toolu_1", "started")
	b.read(read, gone)
	if got := paths(); strings.Join(got, ",") != "a.txt" {
		t.Fatalf("pending %q, want the read message's file alone", got)
	}
}

func TestATurnNobodyClosedClosesAtTheNext(t *testing.T) {
	b := &logb{t: t}
	aStep(b)
	b.request(true)
	b.add(session.TypeUserMessage, "", 1, session.UserMessage{Sender: firing, Content: []lux.Block{txt("Check the deploy.")}, FiringID: "fir_1"})
	if w := session.WaitingMessages(b.events); w[0].Closed != 0 {
		t.Fatalf("a running turn closed: %+v", w[0])
	}
	again := b.status(session.StatusRunning, "", "")
	w := session.WaitingMessages(b.events)
	if len(w) != 1 || w[0].Closed == 0 || w[0].Closed != b.events[len(b.events)-1].Seq || again == "" {
		t.Fatalf("a firing of a turn a runner took over is not of a closed turn: %+v", w)
	}
}
