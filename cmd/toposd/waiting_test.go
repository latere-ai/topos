// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// bashes is a reply that runs command with bash as the call id.
func bashes(id, command string) luxstub.Reply {
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "bash", Args: args}}}, StopReason: ir.StopToolUse}}
}

// readsLast is a reply that ends the turn once the request's last message
// holds every text of want, in order, and none of never.
func readsLast(said string, want, never []string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: said}}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		b, err := json.Marshal(r.Messages[len(r.Messages)-1])
		if err != nil {
			return err
		}
		last, at := string(b), 0
		for _, w := range want {
			i := strings.Index(last[at:], w)
			if i < 0 {
				return fmt.Errorf("the request's last message does not hold %q after what came before: %s", w, last)
			}
			at += i + len(w)
		}
		for _, n := range never {
			if strings.Contains(last, n) {
				return fmt.Errorf("the request's last message holds %q: %s", n, last)
			}
		}
		return nil
	}}
}

// TestMessagesWhileATurnRuns: a server over the stub Lux and the stub
// Cella, whose sandbox runs the real helper, runs a session of a bash
// agent. While its first command sleeps, the person sends a message to
// be read now: the command moves to the background in the sandbox, its
// call is answered with what it wrote and where it goes on writing, and
// the same turn's next request reads the message after it, led by the
// line that says it came while the agent worked. In the next turn, while
// a command runs, the person sends one message and withdraws another:
// the next request reads the first and not the second, and the log
// holds the read and the withdrawal.
func TestMessagesWhileATurnRuns(t *testing.T) {
	vars, _, _ := hostedStubs(t,
		bashes("toolu_1", "echo begun; sleep 30"),
		readsLast("On 9000.", []string{"begun", "moved to the background", "sent this while you were working:", "Use port 9000."}, nil),
		bashes("toolu_2", "sleep 3; echo passed"),
		readsLast("Done.", []string{"passed", "sent this while you were working:", "Also lint it."}, []string{"Skip the slow ones."}),
	)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	}()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: builder\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [bash]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/builder", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"builder","message":"Start the server."}`)
	var s session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	events := func() []session.Event {
		t.Helper()
		_, raw := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events?limit=500", "")
		var page struct {
			Items []session.Event `json:"items"`
		}
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	// waitUntil polls the log until done holds of it.
	waitUntil := func(what string, done func([]session.Event) bool) []session.Event {
		t.Helper()
		for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			evs := events()
			if done(evs) {
				return evs
			}
			if time.Now().After(deadline) {
				b, _ := json.Marshal(evs)
				t.Fatalf("the log never showed %s: %s", what, b)
			}
		}
	}
	running := func(id string) func([]session.Event) bool {
		return func(evs []session.Event) bool {
			for _, e := range evs {
				var p session.AgentToolUse
				if e.Type == session.TypeAgentToolUse && e.Decode(&p) == nil && p.ToolUseID == id {
					return true
				}
			}
			return false
		}
	}
	answered := func(n int) func([]session.Event) bool {
		return func(evs []session.Event) bool {
			ends := 0
			for _, e := range evs {
				var p session.SessionStatus
				if e.Type == session.TypeSessionStatus && e.Decode(&p) == nil && p.Status == session.StatusIdle {
					if p.StopReason != session.StopEndTurn {
						b, _ := json.Marshal(evs)
						t.Fatalf("a turn ended %s %s: %s", p.StopReason, p.Detail, b)
					}
					ends++
				}
			}
			return ends >= n
		}
	}
	message := func(text string, now bool) session.Event {
		t.Helper()
		code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", fmt.Sprintf(`{"type":"user.message","payload":{"content":[{"type":"text","text":%q}],"send_now":%v}}`, text, now))
		var e session.Event
		if code != http.StatusOK || json.Unmarshal([]byte(body), &e) != nil {
			t.Fatalf("send %q: %d %s", text, code, body)
		}
		return e
	}

	waitUntil("the first command running", running("toolu_1"))
	time.Sleep(time.Second)
	start := time.Now()
	message("Use port 9000.", true)
	waitUntil("the first turn's answer", answered(1))
	if took := time.Since(start); took > 15*time.Second {
		t.Fatalf("the turn answered %s after the message: the command was not moved", took)
	}

	message("Now run the tests.", false)
	waitUntil("the second command running", running("toolu_2"))
	message("Also lint it.", false)
	gone := message("Skip the slow ones.", false)
	if code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.withdraw","payload":{"event_id":"`+gone.ID+`"}}`); code != http.StatusOK {
		t.Fatalf("withdraw: %d %s", code, body)
	}
	evs := waitUntil("the second turn's answer", answered(2))
	states := map[string]session.MessageState{}
	for _, w := range session.WaitingMessages(evs) {
		var text string
		if len(w.Message.Content) > 0 {
			text = w.Message.Content[0].Text
		}
		states[text] = w.State
	}
	want := map[string]session.MessageState{"Use port 9000.": session.MessageRead, "Also lint it.": session.MessageRead, "Skip the slow ones.": session.MessageWithdrawn}
	if !maps.Equal(states, want) {
		t.Fatalf("the waiting messages %v, want %v", states, want)
	}
	var moved session.ToolResult
	for _, e := range evs {
		if id := e.Answers(); id == "toolu_1" {
			if err := e.Decode(&moved); err != nil {
				t.Fatal(err)
			}
		}
	}
	if moved.Outcome != "ok" {
		t.Fatalf("the moved command's result %+v", moved)
	}
	if code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.withdraw","payload":{"event_id":"`+gone.ID+`"}}`); code != http.StatusConflict {
		t.Fatalf("a second withdrawal: %d %s", code, body)
	}
}
