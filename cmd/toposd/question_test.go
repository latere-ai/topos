// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// storage is the question the stub model asks.
const storage = `{"questions":[{"header":"Storage","question":"Which database should the service keep its records in?","options":[` +
	`{"label":"Postgres","recommended":true,"description":"The shared cluster."},{"label":"SQLite","description":"A file beside the binary."}]}]}`

// asks is a reply that calls question, and decided one that ends the turn
// once the result it reads holds want.
func asks(id string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "question", Args: json.RawMessage(storage)}}}, StopReason: ir.StopToolUse}}
}

func decided(want, said string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: said}}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		last := r.Messages[len(r.Messages)-1]
		if len(last.Blocks) == 0 || last.Blocks[0].ToolResult == nil || last.Blocks[0].ToolResult.IsError || !strings.Contains(last.Blocks[0].ToolResult.Blocks[0].Text, want) {
			return fmt.Errorf("the second request does not hold a result naming %q: %+v", want, last)
		}
		return nil
	}}
}

// TestAQuestionIsAskedAnsweredAndContinued: a server over the stub Lux
// runs a session of an agent that names the question tool. Attended, the
// session streams the question's agent.tool_use and goes idle question,
// the list answers that stop reason and the summary counts it idle; the
// person's user.answer is appended, the next request holds a result
// naming the option chosen, and the session goes idle end_turn. Created
// without attended, the same script ends its turn with one unanswered
// result and never waits on the question.
func TestAQuestionIsAskedAnsweredAndContinued(t *testing.T) {
	vars, lux, _ := hostedStubs(t,
		asks("toolu_q1"), decided("Chosen: SQLite", "SQLite it is."),
		asks("toolu_q2"), decided("Nobody attends this session", "I assumed Postgres."),
	)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	}()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: asker\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [question]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/asker", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	create := func(attended bool) string {
		t.Helper()
		code, body := send(http.MethodPost, "/v1/sessions", fmt.Sprintf(`{"agent":"asker","message":"Set up the storage.","attended":%v}`, attended))
		var s session.Session
		if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil || s.Attended != attended {
			t.Fatalf("create: %d %s", code, body)
		}
		return s.ID
	}
	waitFor := func(id string, reason session.StopReason) session.Session {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
			var s session.Session
			if err := json.Unmarshal([]byte(body), &s); err != nil {
				t.Fatal(err)
			}
			if s.Status == session.StatusIdle && s.StopReason == reason {
				return s
			}
			if time.Now().After(deadline) {
				_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
				t.Fatalf("the session never went idle %s: %s\nevents %s", reason, body, events)
			}
		}
	}

	id := create(true)
	frames := streamUntil(t, publicURL, vars, id, `"stop_reason":"question"`)
	if !strings.Contains(frames, "event: agent.tool_use") || !strings.Contains(frames, `"name":"question"`) || !strings.Contains(frames, `"header":"Storage"`) {
		t.Fatalf("the stream did not carry the question's agent.tool_use:\n%s", frames)
	}
	waitFor(id, session.StopQuestion)
	if _, body := send(http.MethodGet, "/v1/sessions?status=idle", ""); !strings.Contains(body, `"stop_reason":"question"`) {
		t.Fatalf("the list does not answer the stop reason: %s", body)
	}
	var sum session.Summary
	if _, body := send(http.MethodGet, "/v1/sessions/summary", ""); json.Unmarshal([]byte(body), &sum) != nil || sum.Sessions.Idle != 1 || sum.Sessions.WaitingForApproval != 0 {
		t.Fatalf("the summary %s", body)
	}
	code, body := send(http.MethodPost, "/v1/sessions/"+id+"/events", `{"type":"user.answer","payload":{"tool_use_id":"toolu_q1","answers":[{"selected":["SQLite"]}]}}`)
	if code != http.StatusOK || !strings.Contains(body, `"type":"user.answer"`) {
		t.Fatalf("the answer: %d %s", code, body)
	}
	waitFor(id, session.StopEndTurn)
	_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
	for _, want := range []string{`"type":"user.answer"`, `"outcome":"ok"`, `"closed_by":"answer"`, `"text":"SQLite it is."`} {
		if !strings.Contains(events, want) {
			t.Errorf("the attended session's log lacks %s", want)
		}
	}

	quiet := create(false)
	waitFor(quiet, session.StopEndTurn)
	_, events = send(http.MethodGet, "/v1/sessions/"+quiet+"/events", "")
	if strings.Contains(events, `"stop_reason":"question"`) || strings.Count(events, `"outcome":"unanswered"`) != 1 || !strings.Contains(events, `"closed_by":"unattended"`) {
		t.Fatalf("the session nobody attends: %s", events)
	}
	if n := len(lux.Requests()); n != 4 {
		t.Fatalf("the model was asked %d times", n)
	}
}

// streamUntil reads a session's stream from its first event until a
// frame holds until, and returns the frames read.
func streamUntil(t *testing.T, publicURL string, vars map[string]string, id, until string) string {
	t.Helper()
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, publicURL+"/v1/sessions/"+id+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream: %d", resp.StatusCode)
	}
	var got strings.Builder
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for lines.Scan() {
		got.WriteString(lines.Text() + "\n")
		if strings.Contains(lines.Text(), until) {
			return got.String()
		}
	}
	if err := lines.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	t.Fatalf("the stream ended before %s:\n%s", until, got.String())
	return ""
}
