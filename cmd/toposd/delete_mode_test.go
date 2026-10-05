// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// said is a stub reply that ends the turn with words.
func said(words string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: words}}, StopReason: ir.StopEndTurn}}
}

// serveOverTheStubs starts serve over the hosted stubs with one runner,
// applies an agent of the stub model on Cella, and answers the API
// client and a wait for a session to go idle with a stop reason after a
// turn.
func serveOverTheStubs(t *testing.T, replies ...luxstub.Reply) (send func(method, path, body string) (int, string), waitFor func(id string, turn int, reason session.StopReason) session.Session) {
	t.Helper()
	vars, _, _ := hostedStubs(t, replies...)
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	t.Cleanup(func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	})
	send = apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: builder\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  approvals: {mode: confirm}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/builder", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	waitFor = func(id string, turn int, reason session.StopReason) session.Session {
		t.Helper()
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
			var s session.Session
			if err := json.Unmarshal([]byte(body), &s); err != nil {
				t.Fatal(err)
			}
			if s.Status == session.StatusIdle && s.StopReason == reason && s.Turn == turn {
				return s
			}
			if time.Now().After(deadline) {
				_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
				t.Fatalf("the session never went idle %s: %s\nevents %s", reason, body, events)
			}
		}
	}
	return send, waitFor
}

// TestADeletedSessionIsGone: through toposd, a session that ran a turn
// over the stub model is deleted, and every read of it is not_found while
// the list and the summary leave it out.
func TestADeletedSessionIsGone(t *testing.T) {
	send, waitFor := serveOverTheStubs(t, said("Hello."))
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"builder","message":"Say hello."}`)
	var s session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	waitFor(s.ID, 1, session.StopEndTurn)
	// The runner releases the directory store's lock on the session just
	// after the idle status it appended, so a delete in that moment is
	// refused as a lock's conflict; it is asked again until the lock is
	// gone, and any other answer fails.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		code, body := send(http.MethodDelete, "/v1/sessions/"+s.ID, "")
		if code == http.StatusNoContent {
			break
		}
		if code != http.StatusConflict || !strings.Contains(body, "locked by runner") || time.Now().After(deadline) {
			t.Fatalf("delete: %d %s", code, body)
		}
	}
	for _, path := range []string{"/v1/sessions/" + s.ID, "/v1/sessions/" + s.ID + "/events", "/v1/sessions/" + s.ID + "/stream", "/v1/sessions/" + s.ID + "/blobs/" + string(s.Agent.Digest)} {
		if code, body := send(http.MethodGet, path, ""); code != http.StatusNotFound {
			t.Errorf("GET %s after the delete: %d %s", path, code, body)
		}
	}
	if _, body := send(http.MethodGet, "/v1/sessions", ""); strings.Contains(body, s.ID) {
		t.Fatalf("the list after the delete: %s", body)
	}
	var sum session.Summary
	if _, body := send(http.MethodGet, "/v1/sessions/summary", ""); json.Unmarshal([]byte(body), &sum) != nil || sum != (session.Summary{}) {
		t.Fatalf("the summary after the delete: %s", body)
	}
}

// TestAModeChangeReachesTheRunner: through toposd, a session switched to
// plan after its first turn blocks the write its next turn's model asks
// for, records the change in its log and its header, and never reaches
// the machine for it.
func TestAModeChangeReachesTheRunner(t *testing.T) {
	write := luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_w", Name: "write", Args: json.RawMessage(`{"path":"NOTES.md","content":"x"}`)}}}, StopReason: ir.StopToolUse}}
	send, waitFor := serveOverTheStubs(t, said("Ready."), write, said("I will only look."))
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"builder","message":"Get ready."}`)
	var s session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	if s.Policy == nil || s.Policy.Mode != "confirm" {
		t.Fatalf("the session starts with %+v", s.Policy)
	}
	waitFor(s.ID, 1, session.StopEndTurn)
	code, body = send(http.MethodPatch, "/v1/sessions/"+s.ID, `{"policy":{"mode":"plan"}}`)
	if code != http.StatusOK || !strings.Contains(body, `"mode":"plan"`) {
		t.Fatalf("the change: %d %s", code, body)
	}
	if code, body := send(http.MethodPost, "/v1/sessions/"+s.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Write the notes."}]}}`); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	after := waitFor(s.ID, 2, session.StopEndTurn)
	if after.Policy == nil || after.Policy.Mode != "plan" {
		t.Fatalf("after the second turn: policy %+v", after.Policy)
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
	for _, want := range []string{`"type":"session.policy_changed"`, `"new":{"mode":"plan"}`, `"verdict":"block"`, `"mode":"plan"`, `"outcome":"blocked"`} {
		if !strings.Contains(events, want) {
			t.Errorf("the log lacks %s:\n%s", want, events)
		}
	}
	if strings.Contains(events, `"type":"session.machine"`) {
		t.Fatalf("a blocked write reached the machine:\n%s", events)
	}
}
