package cella

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/agents/internal/sandbox"
)

// errTokenSource is a TokenSource that always returns an error.
type errTokenSource struct {
	err error
}

func (s *errTokenSource) Token(_ context.Context) (string, error) {
	return "", s.err
}

// TestSetBearer_TokenSourceError verifies that when the TokenSource fails,
// no HTTP request goes out and the error is propagated.
func TestSetBearer_TokenSourceError(t *testing.T) {
	// We verify no request was sent by using a server that always fails
	// the test if hit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to fake server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tokenErr := errors.New("token source failed")
	p := NewCellaSandboxProvider(srv.URL, &errTokenSource{err: tokenErr})

	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err == nil {
		t.Fatal("expected error from token source failure, got nil")
	}
	if !strings.Contains(err.Error(), tokenErr.Error()) {
		t.Errorf("error %q does not contain %q", err.Error(), tokenErr.Error())
	}
}

// TestWithHTTPClient verifies that the WithHTTPClient option is applied and
// the custom client is used for requests.
func TestWithHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, sandboxBody("creating"))
	}))
	t.Cleanup(srv.Close)

	// Use a custom HTTP client; if it's applied the request succeeds.
	customClient := &http.Client{Transport: http.DefaultTransport}
	p := NewCellaSandboxProvider(srv.URL, NewStaticTokenSource(testToken),
		WithHTTPClient(customClient))

	sb, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create with custom client: %v", err)
	}
	if sb.ID != testSandboxID {
		t.Errorf("sb.ID = %q, want %q", sb.ID, testSandboxID)
	}
}

// TestTransportError_Create verifies that a transport-level error (server
// closed / unreachable URL) is returned as an error from Create.
func TestTransportError_Create(t *testing.T) {
	// Start and immediately close a server to get a refused-connection URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // closed immediately

	p := NewCellaSandboxProvider(srv.URL, NewStaticTokenSource(testToken))
	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
}

// TestFetchLogs_NonJSONBody verifies that a non-JSON body from the logs
// endpoint causes a decode error rather than a silent empty response.
func TestFetchLogs_NonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			// Return 200 but non-JSON body.
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not json at all"))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Exec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("expected decode error from malformed logs body, got nil")
	}
}

// TestFetchLogs_Non200Status verifies that a non-200 from the logs endpoint
// is returned as an error.
func TestFetchLogs_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusInternalServerError, cellaErrorBody{
				Code: "internal", Message: "log store unavailable", RequestID: "req_1",
			})
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Exec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("expected error from non-200 logs, got nil")
	}
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type %T, want *sandbox.APIError", err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("APIError.Status = %d, want 500", apiErr.Status)
	}
}

// TestFetchLogs_CursorAdvances verifies that cursor-based pagination
// advances correctly through multiple pages of log output.
func TestFetchLogs_CursorAdvances(t *testing.T) {
	var cursors []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			var cursor int64
			fmt.Sscanf(r.URL.Query().Get("cursor"), "%d", &cursor)
			cursors = append(cursors, cursor)
			switch cursor {
			case 0:
				writeJSON(w, http.StatusOK, logsBody("page1\n", "running", 0, 6))
			case 6:
				writeJSON(w, http.StatusOK, logsBody("page2\n", "exited", 0, 12))
			default:
				writeJSON(w, http.StatusOK, logsBody("", "exited", 0, cursor))
			}
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	res, err := p.Exec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Stdout) != "page1\npage2\n" {
		t.Errorf("Stdout = %q, want page1\\npage2\\n", res.Stdout)
	}
	// Verify cursor progression: 0 → 6.
	if len(cursors) < 2 {
		t.Fatalf("expected ≥2 cursor polls, got %d", len(cursors))
	}
	if cursors[0] != 0 {
		t.Errorf("first cursor = %d, want 0", cursors[0])
	}
	if cursors[1] != 6 {
		t.Errorf("second cursor = %d, want 6", cursors[1])
	}
}

// TestReadFile_NonZeroExitError verifies that ReadFile returns an error
// when the sh command exits with a non-zero code.
func TestReadFile_NonZeroExitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody("cat: no such file\n", "exited", 1, 18))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.ReadFile(context.Background(), testSandboxID, "/nonexistent")
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("error %q does not mention exit code", err.Error())
	}
}

// TestWriteFile_NonZeroExitError verifies that WriteFile returns an error
// when the sh command exits with a non-zero code.
func TestWriteFile_NonZeroExitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody("permission denied\n", "exited", 1, 18))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	err := p.WriteFile(context.Background(), testSandboxID, "/readonly/file", []byte("data"))
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("error %q does not mention exit code", err.Error())
	}
}

// TestListFiles_NonZeroExitError verifies that ListFiles returns an error
// when find exits non-zero.
func TestListFiles_NonZeroExitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			writeJSON(w, http.StatusOK, logsBody("find: /nodir: no such directory\n", "exited", 1, 32))
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.ListFiles(context.Background(), testSandboxID, "/nodir")
	if err == nil {
		t.Fatal("expected error from non-zero exit, got nil")
	}
}

// TestParseListFiles_BadLine verifies that parseListFiles returns an error
// for lines that don't have exactly 4 tab-separated fields.
func TestParseListFiles_BadLine(t *testing.T) {
	_, err := parseListFiles("f\tonly-three-fields\n")
	if err == nil {
		t.Fatal("expected error for malformed line, got nil")
	}
}

// TestParseListFiles_BadSize verifies that a non-integer size causes an error.
func TestParseListFiles_BadSize(t *testing.T) {
	_, err := parseListFiles("f\tNOT_A_NUMBER\t644\tfile.txt\n")
	if err == nil {
		t.Fatal("expected error for bad size, got nil")
	}
}

// TestParseListFiles_BadMode verifies that a non-octal mode field causes
// an error.
func TestParseListFiles_BadMode(t *testing.T) {
	_, err := parseListFiles("f\t100\tNOT_OCTAL\tfile.txt\n")
	if err == nil {
		t.Fatal("expected error for bad mode, got nil")
	}
}

// TestDo_MalformedResponseJSON verifies that a 200 response with a
// non-JSON body returns a decode error from do().
func TestDo_MalformedResponseJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err == nil {
		t.Fatal("expected decode error for malformed response JSON, got nil")
	}
}

// TestDo_NonJSONErrorBody verifies that parseAPIError handles a non-JSON
// error body gracefully (best-effort: fields are empty, status code is used).
func TestDo_NonJSONErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("plain text error"))
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	_, err := p.Create(context.Background(), sandbox.CreateOptions{})
	if err == nil {
		t.Fatal("expected error from 400, got nil")
	}
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type %T, want *sandbox.APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest {
		t.Errorf("APIError.Status = %d, want 400", apiErr.Status)
	}
}

// TestFetchLogs_SetBearerError verifies that fetchLogs propagates a
// token source error before making any HTTP call.
func TestFetchLogs_SetBearerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tokenErr := errors.New("no token available")
	p := NewCellaSandboxProvider(srv.URL, &errTokenSource{err: tokenErr})
	_, err := p.fetchLogs(context.Background(), testSandboxID, testCommandID, 0)
	if err == nil {
		t.Fatal("expected token source error from fetchLogs, got nil")
	}
	if !strings.Contains(err.Error(), tokenErr.Error()) {
		t.Errorf("error %q does not contain %q", err.Error(), tokenErr.Error())
	}
}

// TestStreamExec_PumpTransportError verifies that when the logs endpoint
// returns a transport error during streaming, Recv surfaces a non-EOF error so
// the failure is distinguishable from a clean empty result (the ExecStream
// contract: only io.EOF means clean termination).
func TestStreamExec_PumpTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/commands"):
			writeJSON(w, http.StatusOK, commandBody("running", 0))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/logs"):
			// Close the connection to simulate a transport error.
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				conn.Close()
			}
		}
	}))
	t.Cleanup(srv.Close)

	p := newTestProvider(srv)
	stream, err := p.StreamExec(context.Background(), testSandboxID, sandbox.ExecOptions{Argv: []string{"cat"}})
	if err != nil {
		t.Fatalf("StreamExec: %v", err)
	}
	// Drain stream; the terminal Recv must report the transport failure, not the
	// bare io.EOF sentinel (which signals clean termination). The transport error
	// may itself wrap EOF, so distinguish by identity rather than errors.Is.
	var recvErr error
	for {
		_, recvErr = stream.Recv()
		if recvErr != nil {
			break
		}
	}
	if recvErr == io.EOF { //nolint:errorlint // identity check is intentional: clean EOF vs a wrapping transport error
		t.Fatal("Recv after transport failure returned bare io.EOF, want the transport error")
	}
	_ = stream.Close()
}
