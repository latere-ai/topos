// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"latere.ai/x/pkg/authz/stub"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// fileReply is a reply that calls a file tool with input.
func fileReply(id, name string, input map[string]string) luxstub.Reply {
	args, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: name, Args: args}}}, StopReason: ir.StopToolUse}}
}

// TestMemoryAcrossSessions: on an installation whose authorizer names a
// person's memory folders for each session, a session saves a memory in
// its project folder, written with its own token for the files audience,
// and a second session with the same folder reads it with its own; the
// model reads the folders in the attachments block; the session answers
// each folder's name and access alone; and neither session creates a
// sandbox (spec 093).
func TestMemoryAcrossSessions(t *testing.T) {
	vars, s := credentialStubs(t,
		fileReply("toolu_1", "write", map[string]string{"path": "/memory/project/color.md", "content": "The club's color is navy.\n"}),
		saidReply("I will remember that."),
		fileReply("toolu_2", "read", map[string]string{"path": "/memory/project/color.md"}),
		saidReply("The club's color is navy."),
	)
	arca := arcastub.New(t)
	const owner = "https://issuer.example|admin"
	memory := map[string]any{"memory": []map[string]string{
		{"name": "user", "url": arca.URL(), "owner": owner, "path": "files/memory/you", "access": "read_only"},
		{"name": "project", "url": arca.URL(), "owner": owner, "path": "files/memory/projects/p1", "access": "read_write"},
	}}
	s.az.SetRules(stub.Rule{Action: authorizer.ActionSessionCreate, Allow: true, Limits: memory})
	vars["TOPOS_RUNNER_CAPACITY"] = "1"
	vars["TOPOS_FILES_AUDIENCE"] = "arca"
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: keeper\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [read, write, edit, glob, grep]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/keeper", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	converse := func(text string) session.Session {
		t.Helper()
		code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"keeper"}`)
		var sess session.Session
		if code != http.StatusCreated || json.Unmarshal([]byte(body), &sess) != nil {
			t.Fatalf("create: %d %s", code, body)
		}
		if want := `"memory":[{"name":"user","access":"read_only"},{"name":"project","access":"read_write"}]`; !strings.Contains(body, want) || strings.Contains(body, arca.URL()) || strings.Contains(body, owner) {
			t.Fatalf("the session answers %s, want %s and no address or owner", body, want)
		}
		if code, body := send(http.MethodPost, "/v1/sessions/"+sess.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"`+text+`"}]}}`); code != http.StatusOK {
			t.Fatalf("send: %d %s", code, body)
		}
		waitAnswered(t, publicURL, vars, sess.ID)
		return sess
	}
	first := converse("Remember the club's color is navy.")
	o, ok := arca.Object(owner, "files/memory/projects/p1/color.md")
	if !ok || string(o.Data) != "The club's color is navy.\n" {
		t.Fatalf("the folder holds %v", arca.Paths(owner, "files/"))
	}
	if m, minted := s.idp.Token(o.Token); !minted || m.Audience != "arca" || m.Workload != "session" || m.SessionID != first.ID {
		t.Fatalf("the memory was written with %+v", m)
	}

	second := converse("What color is the club?")
	_, events := send(http.MethodGet, "/v1/sessions/"+second.ID+"/events", "")
	var page struct {
		Items []session.Event `json:"items"`
	}
	if err := json.Unmarshal([]byte(events), &page); err != nil {
		t.Fatal(err)
	}
	read := false
	for _, e := range page.Items {
		var p session.ToolResult
		if e.Type == session.TypeToolResult && e.Decode(&p) == nil && !p.IsError && len(p.Content) > 0 && strings.Contains(p.Content[0].Text, "navy") {
			read = true
		}
	}
	if !read {
		t.Fatalf("the second session did not read the memory: %s", events)
	}
	var readers []string
	for _, r := range arca.Requests() {
		if r.Op != arcastub.OpRead && r.Op != arcastub.OpHead {
			continue
		}
		if m, minted := s.idp.Token(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); minted && m.Audience == "arca" && m.Workload == "session" {
			readers = append(readers, m.SessionID)
		}
	}
	if !slices.Contains(readers, second.ID) {
		t.Fatalf("the second session did not read with its own token; readers %v", readers)
	}
	for i, r := range s.lux.Requests() {
		var system []string
		for _, b := range r.Request.System {
			system = append(system, b.Text)
		}
		if !strings.Contains(strings.Join(system, "\n"), "- /memory/user/ (read only)\n- /memory/project/ (read and write)\n</attachments>") {
			t.Fatalf("request %d does not name the memory folders: %q", i, system)
		}
	}
	if n := s.cella.Count(cellastub.OpCreate); n != 0 {
		t.Fatalf("sessions that only remember created %d sandboxes", n)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}
