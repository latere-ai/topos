// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"
)

// titled waits until the session id holds a title other than its
// create's and answers it.
func titled(t *testing.T, send func(method, path, body string) (int, string), id, create string) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
		var s struct {
			Title string `json:"title"`
		}
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatal(err)
		}
		if s.Title != create {
			return s.Title
		}
		if time.Now().After(deadline) {
			_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
			t.Fatalf("the session was never named: %s\nevents %s", body, events)
		}
	}
}

// TestServeNamesItsSessions: serve's runner names a session after a turn
// its person started when its agent sets spec.title, asking the owner
// policy session.title as the session's initiator; and at its next start
// the pass names the session an agent ran before it turned titles on,
// once, and logs its counts (spec 083).
func TestServeNamesItsSessions(t *testing.T) {
	delay, interval := titlePassDelay, titlePassInterval
	// The first start's pass waits past the test; the second's waits for
	// the claim queue's first look at the idle sessions, which takes each
	// one's lease for a moment, as the pass's own delay does at a real
	// start.
	titlePassDelay, titlePassInterval = time.Hour, 0
	t.Cleanup(func() { titlePassDelay, titlePassInterval = delay, interval })
	vars, lux, _ := hostedStubs(t, said("Reviewed."), said("Main.go review"), said("Kept."), said("Kept notes"))
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	agent := func(name, title string) string {
		return "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: " + name + "\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  machine: {kind: cella}\n" + title
	}
	create := func(send func(method, path, body string) (int, string), name string) (string, string) {
		t.Helper()
		code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"`+name+`","message":"Review main.go.","title":"Review main.go."}`)
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, body)
		}
		var s struct{ ID, Title string }
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			t.Fatal(err)
		}
		return s.ID, s.Title
	}

	ctx, cancel := context.WithCancel(t.Context())
	var first syncBuffer
	publicURL, _, wait := serveLogging(t, ctx, vars, 10*time.Millisecond, &first)
	send := apiClient(t, publicURL, vars)
	for name, title := range map[string]string{"reviewer": "  title: {}\n", "keeper": ""} {
		if code, body := send(http.MethodPut, "/v1/agents/"+name, agent(name, title)); code != http.StatusCreated {
			t.Fatalf("apply %s: %d %s", name, code, body)
		}
	}
	reviewed, given := create(send, "reviewer")
	if got := titled(t, send, reviewed, given); got != "Main.go review" {
		t.Fatalf("the session was named %q", got)
	}
	_, events := send(http.MethodGet, "/v1/sessions/"+reviewed+"/events", "")
	if !strings.Contains(events, `"purpose":"title"`) || !strings.Contains(events, `"subject":"service:harness"`) {
		t.Fatalf("the title's events: %s", events)
	}
	kept, given := create(send, "keeper")
	for deadline := time.Now().Add(30 * time.Second); len(lux.Requests()) < 3; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the keeper's turn never ran")
		}
	}
	// The keeper's agent turns titles on once its session is idle, so no
	// turn of it asks; the next start's pass does.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, body := send(http.MethodGet, "/v1/sessions/"+kept, ""); strings.Contains(body, `"status":"idle"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the keeper's session never went idle")
		}
	}
	if code, body := send(http.MethodPut, "/v1/agents/keeper", agent("keeper", "  title: {}\n")); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("apply keeper again: %d %s", code, body)
	}
	cancel()
	if code := wait(); code != 0 {
		t.Fatalf("the first serve exited %d", code)
	}
	if strings.Contains(first.String(), "title pass") {
		t.Fatalf("the first start's pass ran: %s", first.String())
	}

	titlePassDelay = 500 * time.Millisecond
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	var second syncBuffer
	publicURL, _, wait = serveLogging(t, ctx, vars, 10*time.Millisecond, &second)
	send = apiClient(t, publicURL, vars)
	if got := titled(t, send, kept, given); got != "Kept notes" {
		t.Fatalf("the pass named the session %q", got)
	}
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(second.String(), "title pass"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the pass logged no counts: %s", second.String())
		}
	}
	if log := second.String(); !strings.Contains(log, "agents=2 named=1 kept=0 skipped=1 failed=0") {
		t.Fatalf("the pass logged %s", log)
	}
	cancel()
	if code := wait(); code != 0 {
		t.Fatalf("the second serve exited %d", code)
	}
	if n := len(lux.Requests()); n != 4 {
		t.Fatalf("the model was asked %d times", n)
	}
}
