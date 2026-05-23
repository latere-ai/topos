package cella

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/agents/internal/sandbox"
)

// ── helpers ───────────────────────────────────────────────────────────────────

const (
	testToken     = "test-bearer-token-abc123"
	testSandboxID = "sbx_01hx000000000000000000000a"
	testCommandID = "cmd_01hx000000000000000000000b"
)

// headerTracker records the Authorization header seen on every request.
// Used to assert AC-2 (token sent on every call).
type headerTracker struct {
	mu      sync.Mutex
	headers []string
}

func (t *headerTracker) add(h string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.headers = append(t.headers, h)
}

func (t *headerTracker) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.headers...)
}

// writeJSON writes v as an application/json response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// cellaErrorBody is the flat Cella error envelope used in tests.
type cellaErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// sandboxBody returns a canonical fake Cella Sandbox JSON object.
func sandboxBody(state string) map[string]any {
	return map[string]any{
		"id":         testSandboxID,
		"name":       "test-sandbox",
		"backend":    "k8s",
		"state":      state,
		"created_at": "2026-01-01T00:00:00Z",
		"tier":       "ephemeral",
	}
}

// commandBody returns a canonical fake Cella Command JSON object.
func commandBody(phase string, exitCode int) map[string]any {
	return map[string]any{
		"command_id": testCommandID,
		"phase":      phase,
		"exit_code":  exitCode,
		"started_at": "2026-01-01T00:00:01Z",
		"exited_at":  "2026-01-01T00:00:02Z",
	}
}

// logsBody returns a canonical fake Cella logs cursor response.
func logsBody(output string, phase string, exitCode int, nextCursor int64) map[string]any {
	return map[string]any{
		"bytes":       output,
		"next_cursor": nextCursor,
		"phase":       phase,
		"exit_code":   exitCode,
	}
}

// ── fakeServer ────────────────────────────────────────────────────────────────
//
// fakeServer builds an httptest.Server that mimics the Cella API endpoints
// used by CellaSandboxProvider. It captures authorization headers on every
// request so tests can assert AC-2.

type fakeServer struct {
	tracker *headerTracker
	// commandLogs is called back for each log poll; returns the canned response.
	// If nil, returns a single exited response with output "hello\n".
	commandLogs func(cursor int64) map[string]any
	// overrideHandler, if set, overrides ALL routing for a specific test.
	overrideHandler http.HandlerFunc
}

func newFakeServer(t *testing.T, fs *fakeServer) *httptest.Server {
	t.Helper()
	if fs.tracker == nil {
		fs.tracker = &headerTracker{}
	}
	mux := http.NewServeMux()

	// Helper: capture the Authorization header on every incoming request.
	capture := func(r *http.Request) {
		fs.tracker.add(r.Header.Get("Authorization"))
	}

	// POST /v1/sandboxes — create
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		if fs.overrideHandler != nil {
			fs.overrideHandler(w, r)
			return
		}
		writeJSON(w, http.StatusOK, sandboxBody("creating"))
	})

	// GET /v1/sandboxes/{id} — get / health check
	mux.HandleFunc("GET /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		if fs.overrideHandler != nil {
			fs.overrideHandler(w, r)
			return
		}
		writeJSON(w, http.StatusOK, sandboxBody("running"))
	})

	// DELETE /v1/sandboxes/{id} — destroy
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		if fs.overrideHandler != nil {
			fs.overrideHandler(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// POST /v1/sandboxes/{id}/commands — start command
	mux.HandleFunc("POST /v1/sandboxes/{id}/commands", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		if fs.overrideHandler != nil {
			fs.overrideHandler(w, r)
			return
		}
		writeJSON(w, http.StatusOK, commandBody("running", 0))
	})

	// GET /v1/sandboxes/{id}/commands/{cid}/logs — log cursor
	mux.HandleFunc("GET /v1/sandboxes/{id}/commands/{cid}/logs", func(w http.ResponseWriter, r *http.Request) {
		capture(r)
		if fs.overrideHandler != nil {
			fs.overrideHandler(w, r)
			return
		}
		var cursor int64
		fmt.Sscanf(r.URL.Query().Get("cursor"), "%d", &cursor)
		if fs.commandLogs != nil {
			writeJSON(w, http.StatusOK, fs.commandLogs(cursor))
			return
		}
		// Default: immediately exited with "hello\n" at cursor 0.
		if cursor == 0 {
			writeJSON(w, http.StatusOK, logsBody("hello\n", "exited", 0, 6))
		} else {
			writeJSON(w, http.StatusOK, logsBody("", "exited", 0, cursor))
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// provider returns a CellaSandboxProvider wired to srv.
func newTestProvider(srv *httptest.Server) *CellaSandboxProvider {
	return NewCellaSandboxProvider(srv.URL, NewStaticTokenSource(testToken))
}

// assertBearer verifies that every captured header equals "Bearer <testToken>".
func assertBearer(t *testing.T, tracker *headerTracker) {
	t.Helper()
	want := "Bearer " + testToken
	for i, h := range tracker.all() {
		if h != want {
			t.Errorf("request %d: Authorization = %q, want %q", i, h, want)
		}
	}
	if len(tracker.all()) == 0 {
		t.Error("no requests captured; tracker may not be wired")
	}
}

// ── AC-1: Full lifecycle ──────────────────────────────────────────────────────

func TestFullLifecycle(t *testing.T) {
	// readFileContent is what we'll write and then read back.
	readFileContent := []byte("hello, world!\x00binary\xff")

	// Build a stateful log-response function: the first poll for the
	// ReadFile command returns base64 of the expected content and exits;
	// for other commands it returns "ok\n" immediately.
	//
	// We need to distinguish which command is being polled. We use a
	// simple counter: the logs handler is only called after POST /commands,
	// so we track the last command issued.
	var (
		mu          sync.Mutex
		lastCommand string // last command_id issued (echoed back from POST)
		callCount   int    // incremented on each command POST
	)

	listOutput := "f\t13\t644\tfile.txt\nd\t0\t755\tsubdir\n"

	fs := &fakeServer{tracker: &headerTracker{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.tracker.add(r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
			writeJSON(w, http.StatusOK, sandboxBody("creating"))

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+testSandboxID):
			writeJSON(w, http.StatusOK, sandboxBody("running"))

		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/"+testSandboxID):
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			// Parse the argv to determine which "kind" of command this is.
			var body struct {
				Argv []string `json:"argv"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			callCount++
			n := callCount
			lastCommand = fmt.Sprintf("cmd_%02d", n)
			mu.Unlock()
			writeJSON(w, http.StatusOK, commandBody("running", 0))

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/commands/") && strings.HasSuffix(r.URL.Path, "/logs"):
			mu.Lock()
			cmd := lastCommand
			n := callCount
			mu.Unlock()
			_ = cmd
			var cursor int64
			fmt.Sscanf(r.URL.Query().Get("cursor"), "%d", &cursor)

			// Commands:
			// 1 = Exec ("echo hi")
			// 2 = WriteFile
			// 3 = ReadFile
			// 4 = ListFiles
			switch n {
			case 1: // echo
				writeJSON(w, http.StatusOK, logsBody("hi\n", "exited", 0, 3))
			case 2: // WriteFile — no output, exit 0
				writeJSON(w, http.StatusOK, logsBody("", "exited", 0, 0))
			case 3: // ReadFile — return base64 of content
				b64 := base64.StdEncoding.EncodeToString(readFileContent)
				writeJSON(w, http.StatusOK, logsBody(b64+"\n", "exited", 0, int64(len(b64)+1)))
			case 4: // ListFiles — return find output
				writeJSON(w, http.StatusOK, logsBody(listOutput, "exited", 0, int64(len(listOutput))))
			default:
				writeJSON(w, http.StatusOK, logsBody("", "exited", 0, 0))
			}

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := NewCellaSandboxProvider(srv.URL, NewStaticTokenSource(testToken))
	ctx := context.Background()

	// 1. Create
	sb, err := p.Create(ctx, sandbox.CreateOptions{Name: "test-sandbox"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID != testSandboxID {
		t.Errorf("Create: ID = %q, want %q", sb.ID, testSandboxID)
	}

	// 2. Exec — combined stdout captured, exit code 0
	res, err := p.Exec(ctx, testSandboxID, sandbox.ExecOptions{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "hi\n" {
		t.Errorf("Exec: Stdout = %q, want %q", res.Stdout, "hi\n")
	}
	if res.ExitCode != 0 {
		t.Errorf("Exec: ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Phase != "exited" {
		t.Errorf("Exec: Phase = %q, want exited", res.Phase)
	}

	// 3. WriteFile — must not error
	if err := p.WriteFile(ctx, testSandboxID, "/tmp/test.bin", readFileContent); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 4. ReadFile — content must round-trip byte-for-byte
	got, err := p.ReadFile(ctx, testSandboxID, "/tmp/test.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(readFileContent) {
		t.Errorf("ReadFile: content mismatch\n got: %q\nwant: %q", got, readFileContent)
	}

	// 5. ListFiles — parsed entries
	files, err := p.ListFiles(ctx, testSandboxID, "/tmp")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles: got %d entries, want 2", len(files))
	}
	if files[0].Name != "file.txt" || files[0].IsDir || files[0].Size != 13 {
		t.Errorf("ListFiles[0] = %+v, want {Name:file.txt IsDir:false Size:13}", files[0])
	}
	if files[1].Name != "subdir" || !files[1].IsDir {
		t.Errorf("ListFiles[1] = %+v, want {Name:subdir IsDir:true}", files[1])
	}

	// 6. HealthCheck — returns nil for state=running
	if err := p.HealthCheck(ctx, testSandboxID); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}

	// 7. Destroy — 204 response, returns nil
	if err := p.Destroy(ctx, testSandboxID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	// AC-2: every request carried the correct bearer token.
	assertBearer(t, fs.tracker)
}

// ── AC-2: Authorization header on every request ───────────────────────────────

func TestAuthorizationHeaderOnEveryRequest(t *testing.T) {
	tracker := &headerTracker{}
	fs := &fakeServer{tracker: tracker}
	srv := newFakeServer(t, fs)
	p := newTestProvider(srv)
	ctx := context.Background()

	// Issue one of each verb type.
	_, _ = p.Create(ctx, sandbox.CreateOptions{})
	_, _ = p.Exec(ctx, testSandboxID, sandbox.ExecOptions{Argv: []string{"true"}})
	_ = p.HealthCheck(ctx, testSandboxID)
	_ = p.Destroy(ctx, testSandboxID)

	want := "Bearer " + testToken
	all := tracker.all()
	if len(all) == 0 {
		t.Fatal("no requests captured")
	}
	for i, h := range all {
		if h != want {
			t.Errorf("request %d: Authorization = %q, want %q", i, h, want)
		}
	}
}

// ── AC-3: Error mapping ───────────────────────────────────────────────────────

func TestErrorMapping404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, cellaErrorBody{
			Code:      "not_found",
			Message:   "sandbox not found",
			RequestID: "req_test",
		})
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("404 → error = %v, want ErrNotFound", err)
	}
}

func TestErrorMapping409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, cellaErrorBody{
			Code:      "conflict",
			Message:   "name already in use",
			RequestID: "req_conflict",
		})
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Create(context.Background(), sandbox.CreateOptions{Name: "dup"})
	if !errors.Is(err, sandbox.ErrConflict) {
		t.Errorf("409 → error = %v, want ErrConflict", err)
	}
}

func TestErrorMapping500(t *testing.T) {
	const (
		wantCode      = "internal"
		wantMessage   = "unexpected runtime failure"
		wantRequestID = "req_500"
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, cellaErrorBody{
			Code:      wantCode,
			Message:   wantMessage,
			RequestID: wantRequestID,
		})
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	err := p.Destroy(context.Background(), testSandboxID)

	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("500 → error type %T, want *sandbox.APIError", err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("APIError.Status = %d, want 500", apiErr.Status)
	}
	if apiErr.Code != wantCode {
		t.Errorf("APIError.Code = %q, want %q", apiErr.Code, wantCode)
	}
	if apiErr.Message != wantMessage {
		t.Errorf("APIError.Message = %q, want %q", apiErr.Message, wantMessage)
	}
	if apiErr.RequestID != wantRequestID {
		t.Errorf("APIError.RequestID = %q, want %q", apiErr.RequestID, wantRequestID)
	}
}

// ── AC-4: Correct method + path + body per call ───────────────────────────────

func TestCreateSendsPOSTWithKindLabel(t *testing.T) {
	var gotBody cellaCreateSandboxReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sandboxes" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, sandboxBody("creating"))
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Create(context.Background(), sandbox.CreateOptions{
		Name:   "my-sandbox",
		Labels: map[string]string{"team": "eng"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if gotBody.Labels["kind"] != "agent" {
		t.Errorf("label kind = %q, want agent", gotBody.Labels["kind"])
	}
	if gotBody.Labels["team"] != "eng" {
		t.Errorf("label team = %q, want eng", gotBody.Labels["team"])
	}
	if gotBody.Name != "my-sandbox" {
		t.Errorf("name = %q, want my-sandbox", gotBody.Name)
	}
}

func TestDestroyIssuesDELETE(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	if err := p.Destroy(context.Background(), testSandboxID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	wantPath := "/v1/sandboxes/" + testSandboxID
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
}

func TestDestroyIdempotent404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, cellaErrorBody{Code: "not_found", Message: "gone"})
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	// Destroy should swallow ErrNotFound and return nil.
	if err := p.Destroy(context.Background(), testSandboxID); err != nil {
		t.Errorf("Destroy on 404 = %v, want nil (idempotent)", err)
	}
}

func TestExecIssuesPOSTCommands(t *testing.T) {
	var gotPath string
	var gotBody cellaCreateCommandReq

	fs := &fakeServer{
		tracker: &headerTracker{},
		commandLogs: func(cursor int64) map[string]any {
			return logsBody("output\n", "exited", 42, 7)
		},
	}
	srv := newFakeServer(t, fs)
	// Override the POST /commands handler to capture body.
	origMux := srv.Config.Handler
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands") {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			writeJSON(w, http.StatusOK, commandBody("running", 0))
			return
		}
		origMux.ServeHTTP(w, r)
	})

	p := newTestProvider(srv)
	res, err := p.Exec(context.Background(), testSandboxID, sandbox.ExecOptions{
		Argv: []string{"sh", "-c", "echo hello"},
		Cwd:  "/workspace",
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	wantPath := "/v1/sandboxes/" + testSandboxID + "/commands"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if !gotBody.Detach {
		t.Error("detach = false, want true")
	}
	if gotBody.Cwd != "/workspace" {
		t.Errorf("cwd = %q, want /workspace", gotBody.Cwd)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

// ── Exec polling: multi-page logs ────────────────────────────────────────────

func TestExecPollsUntilNonRunning(t *testing.T) {
	// Simulate two "running" pages before terminal page.
	callCount := 0
	fs := &fakeServer{
		tracker: &headerTracker{},
		commandLogs: func(cursor int64) map[string]any {
			callCount++
			switch callCount {
			case 1:
				return logsBody("part1\n", "running", 0, 6)
			case 2:
				return logsBody("part2\n", "running", 0, 12)
			default:
				return logsBody("part3\n", "exited", 0, 18)
			}
		},
	}
	srv := newFakeServer(t, fs)
	p := newTestProvider(srv)

	res, err := p.Exec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"sleep", "0"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := "part1\npart2\npart3\n"
	if string(res.Stdout) != want {
		t.Errorf("Stdout = %q, want %q", res.Stdout, want)
	}
}

// ── StreamExec ────────────────────────────────────────────────────────────────

func TestStreamExecDeliversChunksAndEOF(t *testing.T) {
	callCount := 0
	fs := &fakeServer{
		tracker: &headerTracker{},
		commandLogs: func(cursor int64) map[string]any {
			callCount++
			if callCount == 1 {
				return logsBody("chunk1\n", "running", 0, 7)
			}
			return logsBody("chunk2\n", "exited", 0, 14)
		},
	}
	srv := newFakeServer(t, fs)
	p := newTestProvider(srv)

	stream, err := p.StreamExec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"echo"}})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}

	var chunks []string
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		chunks = append(chunks, string(chunk))
	}

	if len(chunks) < 1 {
		t.Fatal("no chunks received")
	}
	got := strings.Join(chunks, "")
	want := "chunk1\nchunk2\n"
	if got != want {
		t.Errorf("stream output = %q, want %q", got, want)
	}

	result := stream.Result()
	if result.Phase != "exited" {
		t.Errorf("Result().Phase = %q, want exited", result.Phase)
	}
	_ = stream.Close()
}

// ── ReadFile / WriteFile round-trip ──────────────────────────────────────────

func TestWriteFileEmbedsBase64InArgv(t *testing.T) {
	data := []byte("binary\x00\xff\xfe data")
	var gotArgv []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			var body struct {
				Argv []string `json:"argv"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotArgv = body.Argv
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody("", "exited", 0, 0))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	if err := p.WriteFile(context.Background(), testSandboxID, "/tmp/file.bin", data); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// argv[4] is the base64 payload; argv[3] is the path.
	if len(gotArgv) < 5 {
		t.Fatalf("argv too short: %v", gotArgv)
	}
	if gotArgv[3] != "/tmp/file.bin" {
		t.Errorf("argv[3] = %q, want /tmp/file.bin", gotArgv[3])
	}
	b64 := gotArgv[4]
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode argv[4]: %v", err)
	}
	if string(decoded) != string(data) {
		t.Errorf("round-trip: got %q, want %q", decoded, data)
	}
}

func TestReadFileParsesBase64Output(t *testing.T) {
	content := []byte("hello, world!\x00\x01\xff")
	b64 := base64.StdEncoding.EncodeToString(content)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody(b64+"\n", "exited", 0, int64(len(b64)+1)))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	got, err := p.ReadFile(context.Background(), testSandboxID, "/tmp/file.bin")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("ReadFile: got %q, want %q", got, content)
	}
}

// ── ListFiles parsing ─────────────────────────────────────────────────────────

func TestListFilesParsesOutput(t *testing.T) {
	// "d\t0\t755\tsubdir" + "f\t1024\t644\tfile.txt"
	findOutput := "d\t0\t755\tsubdir\nf\t1024\t644\tfile.txt\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody(findOutput, "exited", 0, int64(len(findOutput))))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	files, err := p.ListFiles(context.Background(), testSandboxID, "/workspace")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d entries, want 2", len(files))
	}
	if !files[0].IsDir || files[0].Name != "subdir" {
		t.Errorf("files[0] = %+v, want {Name:subdir IsDir:true}", files[0])
	}
	if files[1].IsDir || files[1].Name != "file.txt" || files[1].Size != 1024 {
		t.Errorf("files[1] = %+v, want {Name:file.txt IsDir:false Size:1024}", files[1])
	}
	// Mode for "644" octal = 0o644 = 420 decimal
	if files[1].Mode != 0644 {
		t.Errorf("files[1].Mode = 0%o, want 0644", files[1].Mode)
	}
}

// ── HealthCheck ───────────────────────────────────────────────────────────────

func TestHealthCheckRunningReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sandboxBody("running"))
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(srv)
	if err := p.HealthCheck(context.Background(), testSandboxID); err != nil {
		t.Errorf("HealthCheck(running) = %v, want nil", err)
	}
}

func TestHealthCheckNonRunningReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sandboxBody("stopped"))
	}))
	t.Cleanup(srv.Close)
	p := newTestProvider(srv)
	err := p.HealthCheck(context.Background(), testSandboxID)
	var ae *sandbox.APIError
	if !errors.As(err, &ae) {
		t.Errorf("HealthCheck(stopped) = %v, want *APIError", err)
	}
}

// ── Context cancellation ─────────────────────────────────────────────────────

func TestExecRespectsContextCancellation(t *testing.T) {
	// The log endpoint blocks briefly then checks if the context was respected.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			// Always return "still running" to force the poll loop to
			// rely on context cancellation.
			writeJSON(w, http.StatusOK, logsBody("", "running", 0, 0))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	_, err := p.Exec(ctx, testSandboxID, sandbox.ExecOptions{Argv: []string{"sleep", "999"}})
	if err == nil {
		t.Error("Exec: expected error on ctx cancellation, got nil")
	}
}

// ── parseListFiles unit test ──────────────────────────────────────────────────

func TestParseListFiles(t *testing.T) {
	cases := []struct {
		name  string
		input string
		count int
		isDir []bool
		names []string
		sizes []int64
		modes []uint32
	}{
		{
			name:  "basic",
			input: "f\t100\t644\treadme.md\nd\t0\t755\tsrc\n",
			count: 2,
			isDir: []bool{false, true},
			names: []string{"readme.md", "src"},
			sizes: []int64{100, 0},
			modes: []uint32{0644, 0755},
		},
		{
			name:  "trailing newline only",
			input: "f\t1\t600\tsecret\n",
			count: 1,
		},
		{
			name:  "empty",
			input: "",
			count: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries, err := parseListFiles(c.input)
			if err != nil {
				t.Fatalf("parseListFiles(%q): %v", c.input, err)
			}
			if len(entries) != c.count {
				t.Fatalf("len = %d, want %d", len(entries), c.count)
			}
			for i := range c.names {
				if entries[i].Name != c.names[i] {
					t.Errorf("[%d].Name = %q, want %q", i, entries[i].Name, c.names[i])
				}
				if entries[i].IsDir != c.isDir[i] {
					t.Errorf("[%d].IsDir = %v, want %v", i, entries[i].IsDir, c.isDir[i])
				}
				if entries[i].Size != c.sizes[i] {
					t.Errorf("[%d].Size = %d, want %d", i, entries[i].Size, c.sizes[i])
				}
				if entries[i].Mode != c.modes[i] {
					t.Errorf("[%d].Mode = 0%o, want 0%o", i, entries[i].Mode, c.modes[i])
				}
			}
		})
	}
}
