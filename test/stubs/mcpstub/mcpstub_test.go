// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package mcpstub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post sends body to the stub with the headers of h, and answers the
// status and the body.
func post(t *testing.T, s *Server, body string, h map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`

func modern(method string) map[string]string {
	return map[string]string{"MCP-Protocol-Version": Revision, "Mcp-Method": method, "Authorization": "Bearer t"}
}

// TestTheStubHoldsTheTransport: the stub refuses a wrong bearer, a request
// that does not accept both answers, a body that is not one message, and
// mirrored headers that differ from the body, with HeaderMismatch; it
// decodes a base64 name, answers an unknown method and tool and a bad
// cursor with JSON-RPC errors, and records what it received.
func TestTheStubHoldsTheTransport(t *testing.T) {
	s := New(t, Options{Admit: func(b string) bool { return b == "t" }, Tools: []Tool{{Name: "名前", InputSchema: json.RawMessage(`{}`), Answer: func(json.RawMessage) any { return map[string]any{} }}}})
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`
	if code, _ := post(t, s, list, map[string]string{"Authorization": "Bearer other"}); code != http.StatusUnauthorized {
		t.Fatalf("a wrong bearer: %d", code)
	}
	if code, _ := post(t, s, "not json", modern("tools/list")); code != http.StatusBadRequest {
		t.Fatalf("not a message: %d", code)
	}
	if code, body := post(t, s, list, map[string]string{"MCP-Protocol-Version": Revision, "Mcp-Method": "tools/call", "Authorization": "Bearer t"}); code != http.StatusBadRequest || !strings.Contains(body, "-32020") {
		t.Fatalf("a method that differs: %d %s", code, body)
	}
	if code, body := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`, modern("tools/list")); code != http.StatusBadRequest || !strings.Contains(body, "MCP-Protocol-Version") {
		t.Fatalf("no version in the body: %d %s", code, body)
	}
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"名前",` + meta + `}}`
	h := modern("tools/call")
	h["Mcp-Name"] = "x"
	if code, body := post(t, s, call, h); code != http.StatusBadRequest || !strings.Contains(body, "Mcp-Name") {
		t.Fatalf("a name that differs: %d %s", code, body)
	}
	h["Mcp-Name"] = "=?base64?5ZCN5YmN?="
	if code, body := post(t, s, call, h); code != http.StatusOK || !strings.Contains(body, `"result"`) {
		t.Fatalf("a base64 name: %d %s", code, body)
	}
	unknown := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gone",` + meta + `}}`
	h["Mcp-Name"] = "gone"
	if _, body := post(t, s, unknown, h); !strings.Contains(body, "-32602") {
		t.Fatalf("an unknown tool: %s", body)
	}
	if code, body := post(t, s, `{"jsonrpc":"2.0","id":4,"method":"prompts/list","params":{`+meta+`}}`, modern("prompts/list")); code != http.StatusNotFound || !strings.Contains(body, "-32601") {
		t.Fatalf("an unknown method: %d %s", code, body)
	}
	if _, body := post(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"cursor":"x",`+meta+`}}`, modern("tools/list")); !strings.Contains(body, "bad cursor") {
		t.Fatalf("a bad cursor: %s", body)
	}
	if n := len(s.Requests()); n != 8 {
		t.Fatalf("recorded %d requests", n)
	}
	if decode("=?base64?!!?=") != "=?base64?!!?=" {
		t.Fatal("a broken base64 value was decoded")
	}
}

// TestTheStubsLegacyMode: in its legacy mode the stub refuses the current
// revision with no JSON-RPC error, a request outside a session and one of
// another version.
func TestTheStubsLegacyMode(t *testing.T) {
	s := New(t, Options{Legacy: true})
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	if code, body := post(t, s, list, map[string]string{"MCP-Protocol-Version": Revision}); code != http.StatusBadRequest || strings.Contains(body, "jsonrpc") {
		t.Fatalf("the current revision: %d %s", code, body)
	}
	if code, _ := post(t, s, list, map[string]string{"MCP-Protocol-Version": Legacy, "Mcp-Session-Id": "none"}); code != http.StatusNotFound {
		t.Fatalf("no session: %d", code)
	}
	code, _ := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("initialize: %d", code)
	}
	if code, _ := post(t, s, list, map[string]string{"MCP-Protocol-Version": "2025-03-26", "Mcp-Session-Id": "sess-1"}); code != http.StatusBadRequest {
		t.Fatalf("another version: %d", code)
	}
	if code, _ := post(t, s, list, map[string]string{"MCP-Protocol-Version": Legacy, "Mcp-Session-Id": "sess-1"}); code != http.StatusOK {
		t.Fatalf("in the session: %d", code)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL(), strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("a request that accepts neither answer: %d", resp.StatusCode)
	}
}
