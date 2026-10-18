// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/session"
)

// conversation is a log of one turn a person started and the agent
// answered, and a title the harness gave it.
const conversation = `{"type":"user.message","payload":{"sender":{"subject":"local:tasks","name":"tasks","kind":"person"},"content":[{"type":"text","text":"Draft the release note in ${WORKDIR}."}]}}

{"type":"session.status","turn":1,"payload":{"status":"running","runner":{"id":"topos-tasks","kind":"local"}}}
{"type":"agent.message","turn":1,"step":1,"time":"2026-10-01T09:00:00Z","payload":{"message":{"role":"assistant","blocks":[{"type":"text","text":"The note lists the new flags."}]},"stop_reason":"end_turn"}}
{"type":"session.status","turn":1,"step":1,"payload":{"status":"idle","stop_reason":"end_turn"}}
{"type":"session.title_changed","payload":{"by":{"subject":"service:harness","kind":"service"},"old":"","new":"Release note draft"}}
`

func TestReadLogReadsEachLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	writeFiles(t, filepath.Dir(path), map[string]string{"log.jsonl": conversation})
	now := time.Date(2026, 10, 19, 8, 0, 0, 0, time.FixedZone("x", 3600))
	evs, err := readLog(path, "/w", "http://h", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("%d events", len(evs))
	}
	var m session.UserMessage
	if err := evs[0].Decode(&m); err != nil || !strings.Contains(m.Content[0].Text, "in /w.") {
		t.Fatalf("the placeholder is not expanded: %+v, %v", m, err)
	}
	if evs[0].Time != now.UTC() || evs[2].Time != time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) {
		t.Fatalf("times %v and %v", evs[0].Time, evs[2].Time)
	}
	if evs[2].Turn != 1 || evs[2].Step != 1 || evs[2].Type != session.TypeAgentMessage || evs[0].ID == evs[1].ID || session.CheckID(session.PrefixEvent, evs[4].ID) != nil {
		t.Fatalf("events %+v", evs)
	}
	for _, c := range []struct{ body, want string }{
		{"", "holds no event"},
		{"\n\n", "holds no event"},
		{`{"type":"user.message"`, "line 1"},
		{`{"type":"user.message","id":"evt_1","payload":{}}`, `unknown field "id"`},
		{"\n" + `{"type":"user.shout","payload":{}}`, `line 2: "user.shout" is not an event type`},
		{`{"type":"user.message","payload":[1]}`, "not a JSON object"},
		{`{"type":"user.message"}`, "not a JSON object"},
	} {
		if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readLog(path, "/w", "", now); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err %v, want one naming %q", c.body, err, c.want)
		}
	}
	if _, err := readLog(filepath.Join(t.TempDir(), "absent.jsonl"), "/w", "", now); err == nil {
		t.Fatal("a log that does not exist was read")
	}
}

func TestLoadTaskChecksItsLog(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"coding/seeded/task.yaml":          "name: seeded\ncategory: coding\nprompt: Go on.\nlog: conversation.jsonl\n",
		"coding/seeded/check.yaml":         minimalCheck,
		"coding/seeded/conversation.jsonl": conversation,
		"coding/broken/task.yaml":          "name: broken\ncategory: coding\nprompt: Go on.\nlog: conversation.jsonl\n",
		"coding/broken/check.yaml":         minimalCheck,
		"coding/broken/conversation.jsonl": `{"type":"nothing","payload":{}}`,
		"coding/missing/task.yaml":         "name: missing\ncategory: coding\nprompt: Go on.\nlog: conversation.jsonl\n",
		"coding/missing/check.yaml":        minimalCheck,
	})
	task, err := LoadTask(filepath.Join(root, "coding", "seeded"))
	if err != nil || task.Log != "conversation.jsonl" {
		t.Fatalf("task %+v, %v", task, err)
	}
	for name, want := range map[string]string{"broken": `log "conversation.jsonl": line 1`, "missing": `log "conversation.jsonl"`} {
		if _, err := LoadTask(filepath.Join(root, "coding", name)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want one naming %q", name, err, want)
		}
	}
}

// TestARunStartsFromItsLog: a task's log is the session's first events,
// in order, the prompt follows them, and the turn the prompt starts is
// numbered after the log's: the model reads the conversation the log
// holds before the prompt.
func TestARunStartsFromItsLog(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"coding/seeded/task.yaml":          "name: seeded\ncategory: coding\nprompt: Now add the date.\nlog: conversation.jsonl\nruns: 1\n",
		"coding/seeded/check.yaml":         "- title: {matches: '^Release note draft$'}\n- final_message: {matches: 'dated'}\n",
		"coding/seeded/conversation.jsonl": conversation,
		"coding/seeded/solution.yaml":      "steps:\n  - expect: {messages: 3}\n    text: The note is dated.\n",
	})
	task, err := LoadTask(filepath.Join(root, "coding", "seeded"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunTask(t.Context(), task, Options{Work: t.TempDir(), Script: FileSolution}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("the run failed: %s", res.Reason)
	}
	evs := readEvents(t, res)
	want := []session.Type{session.TypeUserMessage, session.TypeSessionStatus, session.TypeAgentMessage, session.TypeSessionStatus, session.TypeTitleChanged, session.TypeUserMessage}
	for i, typ := range want {
		if evs[i].Type != typ || evs[i].Seq != uint64(i+1) {
			t.Fatalf("event %d is %s at %d, want %s", i, evs[i].Type, evs[i].Seq, typ)
		}
	}
	var prompt session.UserMessage
	if err := evs[5].Decode(&prompt); err != nil || prompt.Content[0].Text != "Now add the date." {
		t.Fatalf("the prompt %+v, %v", prompt, err)
	}
	for _, e := range evs[6:] {
		if e.Type == session.TypeAgentMessage && e.Turn != 2 {
			t.Fatalf("the prompt's turn is numbered %d", e.Turn)
		}
	}
}

// readEvents is a run's session log.
func readEvents(t *testing.T, res RunResult) []session.Event {
	t.Helper()
	b, err := os.ReadFile(res.Log)
	if err != nil {
		t.Fatal(err)
	}
	var evs []session.Event
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var e session.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	return evs
}

// TestTitleAssertions: title holds the new title of the log's last
// session.title_changed to matches and not_matching, "" when the log
// has none, and a title with no pattern or a bad one does not parse.
func TestTitleAssertions(t *testing.T) {
	f := newFixture(t)
	renamed := func(title string) session.Event {
		return event(t, session.TypeTitleChanged, "", session.TitleChanged{By: session.Sender{Subject: session.AuthorizerSubject, Kind: session.SenderService}, New: title})
	}
	f.in.Events = []session.Event{renamed("Release notes"), renamed("Database migration")}
	for src, pass := range map[string]bool{
		"- title: {matches: '(?i)migration'}\n":                       true,
		"- title: {matches: '(?i)release'}\n":                         false,
		"- title: {not_matching: '(?i)release'}\n":                    true,
		"- title: {matches: 'Database', not_matching: 'migration'}\n": false,
	} {
		v := f.judge(src)
		if v.Pass != pass || (!pass && (v.Kind != "title" || !strings.Contains(v.Reason, `"Database migration"`))) {
			t.Errorf("%s: %+v, want pass %v", src, v, pass)
		}
	}
	f.in.Events = nil
	if v := f.judge("- title: {matches: '^$'}\n"); !v.Pass {
		t.Fatalf("a log with no title: %+v", v)
	}
	for src, want := range map[string]string{"- title: {}\n": "no pattern", "- title: {matches: '('}\n": "missing closing", "- title: {not_matching: '['}\n": "missing closing"} {
		if _, err := ParseCheck([]byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", src, err)
		}
	}
}
