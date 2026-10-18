// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness/parsing"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// attachedPDF finds the path of the PDF a message attached in a request.
var attachedPDF = regexp.MustCompile(`attachments/[A-Za-z0-9_]+/report\.pdf`)

// readsTheReport is a reply that reads the PDF the person attached, its
// path taken from the request it answers.
func readsTheReport(id string) luxstub.Reply {
	return luxstub.Reply{
		Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "read", Args: json.RawMessage(`{}`)}}}, StopReason: ir.StopToolUse},
		Respond: func(r *ir.Request, resp *ir.Response) {
			for _, m := range r.Messages {
				for _, b := range m.Blocks {
					if p := attachedPDF.FindString(b.Text); p != "" {
						resp.Blocks[0].ToolUse.Args = json.RawMessage(fmt.Sprintf(`{"path":%q}`, p))
					}
				}
			}
		},
	}
}

// TestAPDFIsReadThroughTheParsingService (spec 080): on an installation
// with a parsing service, a hosted session whose message attached a PDF
// reads it with read. While the identity provider issues no token for
// lectio, the read is the binary answer and the sentence that says the
// service is unavailable, and the turn ends as any other; once it does,
// the service is reached with the session's own token for lectio and
// the workload session, fetches the file from the session's folder at the
// address the files API presigned, and the model reads its pages as
// Markdown, the tool.result carrying the cost of the page a model read.
func TestAPDFIsReadThroughTheParsingService(t *testing.T) {
	var mu sync.Mutex
	var bearers, fetched []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		bearers = append(bearers, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		var out any = map[string]any{"id": "prs_1", "state": "succeeded", "progress": map[string]any{"pages_total": 3}, "usage": map[string]any{"input_tokens": 2000, "output_tokens": 100}}
		switch r.URL.Path {
		case "/v1/parsing/parses":
			var in struct {
				Source struct {
					URL string `json:"url"`
				} `json:"source"`
			}
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("a submit: %v", err)
			}
			resp, err := http.Get(in.Source.URL)
			if err != nil {
				t.Errorf("fetch the source: %v", err)
				return
			}
			b, err := io.ReadAll(resp.Body)
			if cerr := resp.Body.Close(); err != nil || cerr != nil {
				t.Errorf("read the source: %v %v", err, cerr)
			}
			fetched = append(fetched, string(b))
		case "/v1/parsing/parses/prs_1/pages":
			out = map[string]any{"pages": []map[string]any{{"number": 1, "source": "reader"}}}
		case "/v1/parsing/parses/prs_1/pages/1":
			out = map[string]any{"number": 1, "source": "reader", "model": "gemini/gemini-3-flash", "usage": map[string]any{"input_tokens": 2000, "output_tokens": 100}}
		case "/v1/parsing/parses/prs_1/document":
			sel, err := parsing.Select(r.URL.Query().Get("pages"), 3)
			if err != nil {
				t.Errorf("the document's pages: %v", err)
			}
			for _, n := range sel {
				_, _ = fmt.Fprintf(w, "<!-- page %d -->\nHigh tide on page %d.\n", n, n)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(svc.Close)

	vars, s := credentialStubs(t,
		readsTheReport("toolu_r1"), found("It is a PDF, and it could not be read as text: the parsing service is unavailable.", "I cannot read it."),
		readsTheReport("toolu_r2"), found("is a PDF of 3 pages, read as text through the parsing service. Pages 1-3 follow", "It is about the tide."),
	)
	arca := arcastub.New(t)
	const owner = "https://issuer.example|admin"
	s.az.SetRules(stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true,
		Limits: map[string]any{"folder": map[string]any{"url": arca.URL(), "owner": owner, "path": "files/conversations/first"}}})
	vars["TOPOS_RUNNER_CAPACITY"] = "1"
	vars["TOPOS_FILES_AUDIENCE"] = "arca"
	vars["TOPOS_PARSING_URL"] = svc.URL + "/v1/parsing"
	publicURL, _, stop := startServe(t, vars)
	defer func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	}()
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: reader\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [read]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/reader", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"reader"}`)
	var sess session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &sess) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	pdf := "%PDF-1.7\n\x00\x01 the tide report\n%%EOF\n"
	message := func(text string, attach bool) {
		t.Helper()
		files := ""
		if attach {
			files = fmt.Sprintf(`,"attachments":[{"name":"report.pdf","media_type":"application/pdf","data":%q}]`, base64.StdEncoding.EncodeToString([]byte(pdf)))
		}
		code, body := send(http.MethodPost, "/v1/sessions/"+sess.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":`+fmt.Sprintf("%q", text)+`}]`+files+`}}`)
		if code != http.StatusOK {
			t.Fatalf("send: %d %s", code, body)
		}
		waitAnswered(t, publicURL, vars, sess.ID)
	}
	results := func() []session.ToolResult {
		t.Helper()
		_, events := send(http.MethodGet, "/v1/sessions/"+sess.ID+"/events", "")
		var page struct {
			Items []session.Event `json:"items"`
		}
		if err := json.Unmarshal([]byte(events), &page); err != nil {
			t.Fatalf("events %s: %v", events, err)
		}
		var out []session.ToolResult
		for _, e := range page.Items {
			if e.Type == session.TypeToolResult {
				var p session.ToolResult
				if err := e.Decode(&p); err != nil {
					t.Fatal(err)
				}
				out = append(out, p)
			}
		}
		return out
	}

	// The identity provider issues no token for lectio yet.
	message("What does the report say?", true)
	rs := results()
	if len(rs) != 1 || !rs[0].IsError || rs[0].CostUSDMicro != nil || !strings.Contains(rs[0].Content[0].Text, "is a binary file") {
		t.Fatalf("a read with no token for the service: %+v", rs)
	}
	mu.Lock()
	reached := len(bearers)
	mu.Unlock()
	if reached != 0 {
		t.Fatalf("the service was reached %d times with no token", reached)
	}

	s.idp.SetAudiences("cella", "origo", "arca", "lectio")
	message("Try again.", false)
	// The session was idle before the message, so the turn it starts is
	// waited for by its result.
	for deadline := time.Now().Add(30 * time.Second); len(rs) < 2 || !strings.Contains(lastAnswer(send, sess.ID), "It is about the tide."); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the second turn did not end: %+v", rs)
		}
		rs = results()
	}
	if len(rs) != 2 || rs[1].IsError || !strings.Contains(rs[1].Content[0].Text, "High tide on page 3.") || rs[1].CostUSDMicro == nil || *rs[1].CostUSDMicro <= 0 {
		t.Fatalf("the read through the service: %+v", rs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(fetched) != 1 || fetched[0] != pdf {
		t.Fatalf("the service fetched %q", fetched)
	}
	for _, b := range bearers {
		m, minted := s.idp.Token(b)
		if !minted || m.Audience != "lectio" || m.Workload != "session" || m.SessionID != sess.ID {
			t.Fatalf("the service was reached with %+v", m)
		}
	}
}

// lastAnswer is the text of the session's last agent.message.
func lastAnswer(send func(method, path, body string) (int, string), id string) string {
	_, events := send(http.MethodGet, "/v1/sessions/"+id+"/events", "")
	var page struct {
		Items []session.Event `json:"items"`
	}
	if json.Unmarshal([]byte(events), &page) != nil {
		return ""
	}
	last := ""
	for _, e := range page.Items {
		var m session.AgentMessage
		if e.Type == session.TypeAgentMessage && e.Decode(&m) == nil && len(m.Message.Blocks) > 0 {
			last = m.Message.Blocks[0].Text
		}
	}
	return last
}
