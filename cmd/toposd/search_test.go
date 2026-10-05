// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// searches is a reply that calls web_search, and found one that ends the
// turn once the result it reads holds want.
func searches(id string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "web_search", Args: json.RawMessage(`{"query":"go release"}`)}}}, StopReason: ir.StopToolUse}}
}

func found(want, said string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: said}}, StopReason: ir.StopEndTurn}, Expect: func(r *ir.Request) error {
		last := r.Messages[len(r.Messages)-1]
		if len(last.Blocks) == 0 || last.Blocks[0].ToolResult == nil || !strings.Contains(last.Blocks[0].ToolResult.Blocks[0].Text, want) {
			return fmt.Errorf("the request after the search does not hold %q: %+v", want, last)
		}
		return nil
	}}
}

// TestASessionSearchesWithItsOwnKey (spec 047): on an installation whose
// sessions act with their own credentials, an agent that names
// web_search searches the installation's search service with the
// session's own runner key, the one the session key routes registered by
// hash; the next request holds the results, the tool.result carries the
// cost the service reported, and the session's spend includes it. A
// search the service refuses is answered with the service's sentence,
// which the model reads, and its code is the result's meta.
func TestASessionSearchesWithItsOwnKey(t *testing.T) {
	var mu sync.Mutex
	var bearers []string
	refuse := false
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearers = append(bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		no := refuse
		mu.Unlock()
		var in struct {
			Query      string `json:"query"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Query != "go release" || in.MaxResults != 5 {
			t.Errorf("the search service read %+v, %v", in, err)
		}
		w.Header().Set("Content-Type", "application/json")
		if no {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"search_needs_credit","message":"Searching the web needs a level that uses credit."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"results":[{"title":"Go 1.25 is released","url":"https://go.dev/blog/go1.25","snippet":"Go 1.25 is now available."}],"cost_usd_micro":10000}`)
	}))
	t.Cleanup(svc.Close)

	vars, s := credentialStubs(t,
		searches("toolu_s1"), found("https://go.dev/blog/go1.25", "Go 1.25 is out."),
		searches("toolu_s2"), found("The search service refused the search: Searching the web needs a level that uses credit.", "I cannot search on this level."),
	)
	vars["TOPOS_RUNNER_CAPACITY"], vars["TOPOS_SEARCH_URL"] = "1", svc.URL
	publicURL, _, stop := startServe(t, vars)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	}()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: searcher\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [web_search]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/searcher", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	create := func() string {
		t.Helper()
		code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"searcher","message":"What is the latest Go release?"}`)
		var created session.Session
		if code != http.StatusCreated || json.Unmarshal([]byte(body), &created) != nil {
			t.Fatalf("create: %d %s", code, body)
		}
		return created.ID
	}
	result := func(id string) (session.Session, session.ToolResult, string) {
		t.Helper()
		waitAnswered(t, publicURL, vars, id)
		_, body := send(http.MethodGet, "/v1/sessions/"+id, "")
		var got session.Session
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
		var page struct {
			Items []session.Event `json:"items"`
		}
		if err := json.Unmarshal([]byte(events), &page); err != nil {
			t.Fatalf("events %s: %v", events, err)
		}
		for _, e := range page.Items {
			if e.Type == session.TypeToolResult {
				var p session.ToolResult
				if err := e.Decode(&p); err != nil {
					t.Fatal(err)
				}
				return got, p, events
			}
		}
		t.Fatalf("no tool.result: %s", events)
		return got, session.ToolResult{}, ""
	}

	id := create()
	got, p, events := result(id)
	key, ok := s.keys.Key(id, "session")
	mu.Lock()
	first := bearers[0]
	mu.Unlock()
	if !ok || hash(first) != key.Hash {
		t.Fatal("the search did not carry the session's own runner key")
	}
	if sandbox, ok := s.keys.Key(id, "sandbox"); ok && hash(first) == sandbox.Hash {
		t.Fatal("the search carried the sandbox's key")
	}
	if p.Outcome != "ok" || p.CostUSDMicro == nil || *p.CostUSDMicro != 10000 || !strings.Contains(events, `"text":"Go 1.25 is out."`) {
		t.Fatalf("the result %+v", p)
	}
	if got.Budget.SpentCostUSDMicro < 10000 {
		t.Fatalf("the session's spend %d does not count the search", got.Budget.SpentCostUSDMicro)
	}
	if strings.Contains(events, `"type":"session.machine"`) {
		t.Fatal("a session that only searched opened a sandbox")
	}

	mu.Lock()
	refuse = true
	mu.Unlock()
	refused := create()
	_, p, events = result(refused)
	if p.Outcome != "error" || p.CostUSDMicro != nil || !strings.Contains(string(p.Meta), `"refusal":"search_needs_credit"`) || !strings.Contains(events, "I cannot search on this level.") {
		t.Fatalf("the refused search %+v %s", p, p.Meta)
	}
	if n := len(s.lux.Requests()); n != 4 {
		t.Fatalf("the model was asked %d times", n)
	}
}
