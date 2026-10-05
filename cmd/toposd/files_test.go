// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// TestAFileTheAgentWroteIsReadThroughTheAPI: through toposd, a file the
// agent wrote in its sandbox is read by the person over the API at the
// path its write result names, with its size in that result, labeled a
// download; once Cella stops the idle sandbox, the read is
// file_unavailable and the sandbox stays stopped.
func TestAFileTheAgentWroteIsReadThroughTheAPI(t *testing.T) {
	page := "<!doctype html><title>A poem</title><p>Roses.</p>"
	input, err := json.Marshal(map[string]string{"path": "poetry.html", "content": page})
	if err != nil {
		t.Fatal(err)
	}
	write := luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_w", Name: "write", Args: input}}}, StopReason: ir.StopToolUse}}
	vars, _, stub := hostedStubs(t, write, said("The poem is ready."))
	maps.Copy(vars, map[string]string{"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1"})
	publicURL, _, stop := startServe(t, vars)
	t.Cleanup(func() {
		if code := stop(); code != 0 {
			t.Errorf("serve exited %d", code)
		}
	})
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: poet\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  approvals: {mode: progressive}\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/poet", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"poet","message":"Write me a poem as a page."}`)
	var s session.Session
	if code != http.StatusCreated || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("create: %d %s", code, body)
	}
	var meta tools.Meta
	for deadline := time.Now().Add(30 * time.Second); meta.Path == ""; time.Sleep(50 * time.Millisecond) {
		_, events := send(http.MethodGet, "/v1/sessions/"+s.ID+"/events", "")
		var page struct {
			Items []session.Event `json:"items"`
		}
		if err := json.Unmarshal([]byte(events), &page); err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Items {
			var r session.ToolResult
			if e.Type == session.TypeToolResult && e.Decode(&r) == nil && len(r.Meta) > 0 {
				if err := json.Unmarshal(r.Meta, &meta); err != nil {
					t.Fatal(err)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no write result: %s", events)
		}
	}
	if !strings.HasSuffix(meta.Path, "/poetry.html") || meta.Size == nil || *meta.Size != int64(len(page)) {
		t.Fatalf("the write's meta: %+v", meta)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, publicURL+"/v1/sessions/"+s.ID+"/files?path="+url.QueryEscape(meta.Path), nil)
	if err != nil {
		t.Fatal(err)
	}
	code, body, header := sendRaw(t, req, vars)
	if code != http.StatusOK || body != page || header.Get("Content-Type") != "text/html; charset=utf-8" ||
		header.Get("Content-Disposition") != "attachment; filename=poetry.html" || header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("the file: %d %v %q", code, header, body)
	}
	// Cella stops an idle sandbox; its files are not available until the
	// session runs it again, and a read does not start it.
	name := cella.SandboxName(s.ID)
	stub.Stop(name)
	if code, body := send(http.MethodGet, "/v1/sessions/"+s.ID+"/files?path=poetry.html", ""); code != http.StatusConflict || !strings.Contains(body, `"file_unavailable"`) {
		t.Fatalf("a stopped sandbox: %d %s", code, body)
	}
	if sb, _ := stub.Sandbox(name); sb.Status.Phase != "Stopped" {
		t.Fatalf("the read left the sandbox %s", sb.Status.Phase)
	}
}

// sendRaw sends req with the local issuer's token and answers its
// status, its body and its header.
func sendRaw(t *testing.T, req *http.Request, vars map[string]string) (int, string, http.Header) {
	t.Helper()
	var tok bytes.Buffer
	if code := run(t.Context(), []string{"token"}, env(vars), &tok, io.Discard); code != 0 {
		t.Fatalf("token: exit %d", code)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(tok.String()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b), resp.Header
}
