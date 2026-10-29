// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

var (
	started = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	changed = time.Date(2026, 10, 28, 10, 12, 0, 0, time.UTC)
)

func event(t *testing.T, typ session.Type, payload any, at time.Time, thread string) session.Event {
	t.Helper()
	e, err := session.NewEvent(typ, payload, at)
	if err != nil {
		t.Fatal(err)
	}
	e.Subagent = thread
	return e
}

func said(s string) []lux.Block { return []lux.Block{{Type: ir.BlockText, Text: s}} }

func agentSaid(blocks ...lux.Block) session.AgentMessage {
	return session.AgentMessage{Message: lux.Message{Role: "assistant", Blocks: blocks}}
}

func conversation() session.Session {
	return session.Session{ID: "ses_01JA", Title: "Tide tables", CreatedAt: started, UpdatedAt: changed, LastSeq: 9}
}

// TestAConversationsFile: a conversation's file is named by the day it
// started and its id, which names it back; it holds the title, the dates
// and what the person and the agent said in order, an answer's choices
// and words, and no tool call, tool output, subagent's thread or redacted
// event (spec 094).
func TestAConversationsFile(t *testing.T) {
	s := conversation()
	if got := ConversationName(s); got != "2026-10-28-ses_01JA.md" {
		t.Fatalf("the name %q", got)
	}
	for name, want := range map[string]string{"2026-10-28-ses_01JA.md": "ses_01JA", "2026-10-28-a-b.md": "a-b"} {
		if id, ok := ConversationID(name); !ok || id != want {
			t.Errorf("ConversationID(%q) = %q, %v", name, id, ok)
		}
	}
	for _, name := range []string{"ses_01JA.md", "2026-10-28-ses_01JA.txt", "2026-13-28-ses_01JA.md", "2026-10-28-.md", "2026-10-28-a/b.md", "2026-10-28_ses.md", "MEMORY.md"} {
		if id, ok := ConversationID(name); ok {
			t.Errorf("ConversationID(%q) = %q", name, id)
		}
	}
	redacted := event(t, session.TypeUserMessage, session.UserMessage{Content: said("a secret")}, started, "")
	redacted.Payload = json.RawMessage(`{"tombstone":true}`)
	tool := lux.Block{Type: ir.BlockToolUse}
	evs := []session.Event{
		event(t, session.TypeUserMessage, session.UserMessage{Content: said("What time is **high tide** at Kiel?")}, started, ""),
		event(t, session.TypeAgentMessage, agentSaid(tool), started.Add(time.Minute), ""),
		event(t, session.TypeToolResult, map[string]any{"tool_use_id": "toolu_1", "content": "the tide tables say 14:12"}, started.Add(time.Minute), ""),
		event(t, session.TypeAgentMessage, agentSaid(lux.Block{Type: ir.BlockThinking, Text: "thinking it over"}, lux.Block{Type: ir.BlockText, Text: "High tide is at 14:12."}), started.Add(2*time.Minute), ""),
		event(t, session.TypeAgentMessage, agentSaid(lux.Block{Type: ir.BlockText, Text: "a subagent's answer"}), started.Add(2*time.Minute), "thr_1"),
		redacted,
		event(t, session.TypeUserAnswer, session.UserAnswer{ToolUseID: "toolu_2", Answers: []session.AnswerEntry{{Selected: []string{"Kiel"}, Text: "and Laboe"}}}, started.Add(3*time.Minute), ""),
	}
	got := string(RenderConversation(s, evs))
	want := "# Tide tables\n\nSession ses_01JA, started 2026-10-28 09:00 UTC, last changed 2026-10-28 10:12 UTC.\n" +
		"\n## User, 2026-10-28 09:00 UTC\n\nWhat time is **high tide** at Kiel?\n" +
		"\n## Agent, 2026-10-28 09:02 UTC\n\nHigh tide is at 14:12.\n" +
		"\n## User, 2026-10-28 09:03 UTC\n\nKiel\n\nand Laboe\n"
	if got != want {
		t.Fatalf("the file\n%s\nwant\n%s", got, want)
	}
	untitled := s
	untitled.Title = "  "
	if got := string(RenderConversation(untitled, nil)); !strings.HasPrefix(got, "# Session ses_01JA\n") {
		t.Fatalf("an untitled conversation's file %q", got)
	}
	multi := s
	multi.Title = "Tide\ntables"
	if got := string(RenderConversation(multi, nil)); !strings.HasPrefix(got, "# Tide tables\n") {
		t.Fatalf("a title of two lines %q", got)
	}
}

// TestAConversationsVersion: a file's version changes with the log, the
// title and the time the session last changed, and not otherwise.
func TestAConversationsVersion(t *testing.T) {
	s := conversation()
	v := ConversationFileOf(s).Version
	if v == "" || ConversationFileOf(s).Version != v {
		t.Fatal("a version is not stable")
	}
	for name, mut := range map[string]func(*session.Session){
		"the log":   func(s *session.Session) { s.LastSeq++ },
		"the title": func(s *session.Session) { s.Title = "Tides" },
		"a change":  func(s *session.Session) { s.UpdatedAt = s.UpdatedAt.Add(time.Second) },
	} {
		c := s
		mut(&c)
		if ConversationFileOf(c).Version == v {
			t.Errorf("%s did not change the version", name)
		}
	}
	if f := ConversationFileOf(s); f.Name != ConversationName(s) || !f.Modified.Equal(changed) || f.Size != 0 {
		t.Fatalf("the file %+v", f)
	}
}

// TestALongConversationIsCut: a conversation past MaxConversationBytes
// keeps its messages in order up to the bound and says how many were left
// out, and a first message past the bound alone is cut on a character's
// start and says so.
func TestALongConversationIsCut(t *testing.T) {
	s := conversation()
	line := strings.Repeat("Gezeiten ", 1000)
	var evs []session.Event
	for i := range 40 {
		evs = append(evs, event(t, session.TypeUserMessage, session.UserMessage{Content: said(line)}, started.Add(time.Duration(i)*time.Minute), ""))
	}
	got := RenderConversation(s, evs)
	if len(got) > MaxConversationBytes {
		t.Fatalf("%d bytes, past %d", len(got), MaxConversationBytes)
	}
	kept := strings.Count(string(got), "## User")
	if kept == 0 || kept == len(evs) {
		t.Fatalf("kept %d of %d messages", kept, len(evs))
	}
	tail := string(got[strings.LastIndex(strings.TrimRight(string(got), "\n"), "\n")+1:])
	if want := "more messages of this conversation are not in this file, which holds at most 256 KiB.]\n"; !strings.HasSuffix(tail, want) || !strings.Contains(tail, "["+itoa(len(evs)-kept)+" ") {
		t.Fatalf("the last line %q, with %d kept", tail, kept)
	}
	huge := strings.Repeat("潮", MaxConversationBytes)
	one := RenderConversation(s, []session.Event{event(t, session.TypeUserMessage, session.UserMessage{Content: said(huge)}, started, "")})
	if len(one) > MaxConversationBytes || !strings.HasSuffix(string(one), "[The rest of this message is not in this file, which holds at most 256 KiB.]\n") || !strings.Contains(string(one), "潮潮") {
		t.Fatalf("a first message past the bound: %d bytes, ending %q", len(one), one[len(one)-120:])
	}
	two := RenderConversation(s, []session.Event{
		event(t, session.TypeUserMessage, session.UserMessage{Content: said(huge)}, started, ""),
		event(t, session.TypeAgentMessage, agentSaid(lux.Block{Type: ir.BlockText, Text: "short"}), started, ""),
	})
	if !strings.HasSuffix(string(two), "[The rest of this message and 1 more message of this conversation are not in this file, which holds at most 256 KiB.]\n") {
		t.Fatalf("a first message past the bound before another ends %q", two[len(two)-160:])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
