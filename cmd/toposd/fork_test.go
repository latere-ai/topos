// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"encoding/pem"
	"maps"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	cellamachine "latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/runner/checkpoint"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// bashReply is a model reply that runs command with bash.
func bashReply(id, command string) luxstub.Reply {
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "bash", Args: args}}}, StopReason: ir.StopToolUse}}
}

// saidReply is a model reply that answers text and ends the turn.
func saidReply(text string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: "anthropic/claude-haiku-4.5", Blocks: []ir.Block{{Type: ir.BlockText, Text: text}}, StopReason: ir.StopEndTurn}}
}

// gitHostOverTLS serves the bare repositories under root over git's own
// http backend on TLS, to a request that carries the placeholder of a
// sandbox's git host Secret, as Cella's egress gateway receives it, and
// answers the server and a file holding its certificate.
func gitHostOverTLS(t *testing.T, root string) (*httptest.Server, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer cella-placeholder-ses-") || !strings.HasSuffix(auth, "-origo") {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	cert := filepath.Join(t.TempDir(), "git-host.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, cert
}

// gitRun runs git in dir and answers its output.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=p", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=p", "GIT_COMMITTER_EMAIL=p@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestAContinuedHostedSessionHasItsFiles: through toposd, a hosted session
// that works in a repository on the git host writes a file it never
// commits, its checkpoint goes to the git host, and the session ends and
// loses its sandbox. The session POST /v1/sessions/{id}/fork starts is
// sent a message, and its first call, in a sandbox of its own, reads the
// file, recorded as a machine restored at the fork point's checkpoint.
func TestAContinuedHostedSessionHasItsFiles(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "app.git")
	gitRun(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitRun(t, bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	gitRun(t, root, "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "--quiet", "-m", "Start")
	gitRun(t, seed, "push", "--quiet", "origin", "HEAD:main")
	host, cert := gitHostOverTLS(t, root)

	vars, _, cella := hostedStubs(t, bashReply("toolu_write", "echo draft > notes.txt"), saidReply("Drafted."), bashReply("toolu_read", "cat notes.txt"), saidReply("Read."))
	// The sandboxes' git trusts the git host's certificate, as a sandbox
	// trusts a public host's.
	home := filepath.Join(vars["TOPOS_MACHINE_DIR"], "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[http \""+host.URL+"/\"]\n\tsslCAInfo = "+cert+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "origo-token")
	if err := os.WriteFile(token, []byte("installation-git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	maps.Copy(vars, map[string]string{
		"TOPOS_PUBLIC_URL": "http://127.0.0.1:8080", "TOPOS_LOCAL_ISSUER_KEY": localKey(t), "TOPOS_DATA_DIR": t.TempDir(), "TOPOS_RUNNER_CAPACITY": "1",
		"TOPOS_ORIGO_URL": host.URL, "TOPOS_ORIGO_TOKEN_FILE": token,
	})
	publicURL, _, stop := startServe(t, vars)
	send := apiClient(t, publicURL, vars)
	manifest := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: writer\nspec:\n  model: {name: anthropic/claude-haiku-4.5}\n  tools: [bash]\n  machine: {kind: cella}\n"
	if code, body := send(http.MethodPut, "/v1/agents/writer", manifest); code != http.StatusCreated {
		t.Fatalf("apply: %d %s", code, body)
	}
	repo := host.URL + "/app.git"
	code, body := send(http.MethodPost, "/v1/sessions", `{"agent":"writer","message":"Write a draft.","resources":[{"type":"repository","url":"`+repo+`"}]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var parent session.Session
	if err := json.Unmarshal([]byte(body), &parent); err != nil {
		t.Fatal(err)
	}
	waitAnswered(t, publicURL, vars, parent.ID)
	if code, body := send(http.MethodPost, "/v1/sessions/"+parent.ID+"/end", `{"reason":"completed"}`); code != http.StatusOK {
		t.Fatalf("end: %d %s", code, body)
	}
	kept := gitRun(t, bare, "rev-parse", checkpoint.KeptRef(parent.ID))
	if files := gitRun(t, bare, "ls-tree", "-r", "--name-only", kept); !strings.Contains(files, "notes.txt") {
		t.Fatalf("the git host's checkpoint holds %q", files)
	}
	if branches := gitRun(t, bare, "for-each-ref", "--format=%(refname)", "refs/heads"); branches != "refs/heads/main" {
		t.Fatalf("the git host's branches are %q; the draft was never committed", branches)
	}
	// The sandbox goes, as Cella deletes it once the session's age runs out.
	if !cella.Remove(cellamachine.SandboxName(parent.ID)) {
		t.Fatal("the session had no sandbox")
	}

	code, body = send(http.MethodPost, "/v1/sessions/"+parent.ID+"/fork", "")
	if code != http.StatusCreated {
		t.Fatalf("fork: %d %s", code, body)
	}
	var child session.Session
	if err := json.Unmarshal([]byte(body), &child); err != nil {
		t.Fatal(err)
	}
	if code, body := send(http.MethodPost, "/v1/sessions/"+child.ID+"/events", `{"type":"user.message","payload":{"content":[{"type":"text","text":"Read the draft."}]}}`); code != http.StatusOK {
		t.Fatalf("send: %d %s", code, body)
	}
	// A fork starts idle at its fork point's stop reason, so its own
	// answer is the idle status after the message.
	var raw string
	var page struct {
		Items []session.Event `json:"items"`
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		_, raw = send(http.MethodGet, "/v1/sessions/"+child.ID+"/events", "")
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		if n := len(page.Items); n > 0 && page.Items[n-1].Type == session.TypeSessionStatus && !child.Copied(page.Items[n-1]) && strings.Contains(string(page.Items[n-1].Payload), `"status":"idle"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the continued session never answered: %s", raw)
		}
	}
	var read string
	var machine *session.SessionMachine
	for _, e := range page.Items {
		switch {
		case child.Copied(e):
		case e.Type == session.TypeToolResult:
			var p session.ToolResult
			if err := e.Decode(&p); err != nil {
				t.Fatal(err)
			}
			if p.ToolUseID == "toolu_read" && len(p.Content) > 0 {
				read = p.Content[0].Text
			}
		case e.Type == session.TypeSessionMachine && machine == nil:
			machine = &session.SessionMachine{}
			if err := e.Decode(machine); err != nil {
				t.Fatal(err)
			}
		case e.Type == session.TypeSessionError:
			t.Errorf("the continued session recorded %s", e.Payload)
		}
	}
	if !strings.Contains(read, "draft") {
		t.Fatalf("the continued session read %q, want the draft\nevents %s", read, raw)
	}
	if machine == nil || machine.Reason != "restored" || machine.Checkpoint == nil || machine.Checkpoint.Commit != kept {
		t.Fatalf("the continued session's machine %+v, want restored at %s", machine, kept)
	}
	if b, err := os.ReadFile(filepath.Join(cella.Workspace(cellamachine.SandboxName(child.ID)), "notes.txt")); err != nil || string(b) != "draft\n" {
		t.Fatalf("the continued session's sandbox holds notes.txt %q, %v", b, err)
	}
	// The continued session keeps its own checkpoint, chained to the one
	// it restored.
	if got := gitRun(t, bare, "rev-parse", checkpoint.KeptRef(child.ID)+"^"); got != kept {
		t.Fatalf("the continued session's checkpoint at the git host chains to %s, want %s", got, kept)
	}
	if code := stop(); code != 0 {
		t.Fatalf("serve exited %d", code)
	}
}
