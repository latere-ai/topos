package cella

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"latere.ai/x/agents/internal/sandbox"
)

// pollInterval is the delay between cursor-poll requests when waiting
// for a command to finish. 100 ms is a reasonable trade-off between
// latency and request volume.
const pollInterval = 100 * time.Millisecond

// ── Cella wire types ─────────────────────────────────────────────────────────
//
// These types are PRIVATE to this package. Upstream code must never import
// them; it only sees sandbox.Sandbox, sandbox.ExecResult, etc.

// cellaError is the flat error envelope returned by Cella on all 4xx/5xx
// responses (Content-Type: application/json).
//
//	{ "code": "not_found", "message": "...", "request_id": "..." }
type cellaError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// cellaSandbox is the Cella Sandbox DTO.
type cellaSandbox struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Backend   string `json:"backend"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	Tier      string `json:"tier"`
}

// cellaCreateSandboxReq is the POST /v1/sandboxes request body.
type cellaCreateSandboxReq struct {
	Name   string            `json:"name,omitempty"`
	Image  string            `json:"image,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
	Tier   string            `json:"tier,omitempty"`
	Policy string            `json:"policy,omitempty"`
}

// cellaCreateCommandReq is the POST /v1/sandboxes/{id}/commands request body.
type cellaCreateCommandReq struct {
	Argv   []string          `json:"argv"`
	Env    map[string]string `json:"env,omitempty"`
	Cwd    string            `json:"cwd,omitempty"`
	Detach bool              `json:"detach"`
}

// cellaCommand is the Cella Command DTO.
type cellaCommand struct {
	CommandID string `json:"command_id"`
	Phase     string `json:"phase"`
	ExitCode  int    `json:"exit_code"`
	StartedAt string `json:"started_at"`
	ExitedAt  string `json:"exited_at"`
}

// cellaLogsResp is the JSON cursor-mode response from
// GET /v1/sandboxes/{id}/commands/{cid}/logs?stream=false&cursor=N.
type cellaLogsResp struct {
	Bytes      string `json:"bytes"`
	NextCursor int64  `json:"next_cursor"`
	Phase      string `json:"phase"`
	ExitCode   int    `json:"exit_code"`
}

// ── CellaSandboxProvider ─────────────────────────────────────────────────────

// CellaSandboxProvider implements sandbox.SandboxProvider against Cella's
// public sandbox API. It is a thin net/http client; all sandbox policy and
// scheduling decisions stay inside Cella.
//
// All created sandboxes are labelled "kind=agent" so Cella's activity-surface
// and audit-join queries can scope to Topos-owned sandboxes.
//
// Credential handling: the provider calls TokenSource.Token on every outbound
// request and sets "Authorization: Bearer <token>". It never stores or caches
// the token; the TokenSource owns the refresh policy.
type CellaSandboxProvider struct {
	baseURL string
	tokens  TokenSource
	client  *http.Client
}

// Option is a functional option for CellaSandboxProvider construction.
type Option func(*CellaSandboxProvider)

// WithHTTPClient replaces the default http.Client used for API calls.
// Useful for tests and for injecting transports with TLS configuration.
func WithHTTPClient(c *http.Client) Option {
	return func(p *CellaSandboxProvider) { p.client = c }
}

// NewCellaSandboxProvider constructs a CellaSandboxProvider.
//
//   - baseURL is the Cella API base (e.g. "https://sandbox.latere.ai").
//     No trailing slash is required.
//   - tokens yields a bearer token per request (see TokenSource).
//   - opts are optional functional options (e.g. WithHTTPClient).
func NewCellaSandboxProvider(baseURL string, tokens TokenSource, opts ...Option) *CellaSandboxProvider {
	p := &CellaSandboxProvider{
		baseURL: strings.TrimRight(baseURL, "/"),
		tokens:  tokens,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// ── SandboxProvider interface ─────────────────────────────────────────────────

// Create provisions a new sandbox. It merges the caller's labels with the
// mandatory "kind=agent" label before sending to Cella. Returns 200 on
// success; Cella may return 409 (ErrConflict) or 429 / 503 (APIError).
func (p *CellaSandboxProvider) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	labels := map[string]string{"kind": "agent"}
	for k, v := range opts.Labels {
		labels[k] = v
	}
	req := cellaCreateSandboxReq{
		Name:   opts.Name,
		Image:  opts.Image,
		Env:    opts.Env,
		Labels: labels,
		Tier:   opts.Tier,
		Policy: opts.Policy,
	}
	var cs cellaSandbox
	if err := p.do(ctx, http.MethodPost, "/v1/sandboxes", req, &cs); err != nil {
		return sandbox.Sandbox{}, err
	}
	return toSandbox(cs), nil
}

// Destroy deletes the sandbox. It is idempotent: a 404 from Cella is
// treated as success per the spec (DELETE /v1/sandboxes/{id} is
// already idempotent in Cella, but we also swallow ErrNotFound to guard
// against timing races).
func (p *CellaSandboxProvider) Destroy(ctx context.Context, id string) error {
	err := p.do(ctx, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

// Exec runs argv to completion inside the sandbox and returns the result.
// It POSTs the command (detach=true), then polls the logs cursor until the
// phase leaves "running".
//
// Combined-stream note: Cella merges stdout and stderr into a single
// arrival-order stream. ExecResult.Stdout carries that combined output;
// ExecResult.Stderr is always nil for this backend. The interface keeps
// separate Stdout/Stderr for backends that separate the streams.
func (p *CellaSandboxProvider) Exec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	cid, err := p.startCommand(ctx, id, opts)
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	return p.collectLogs(ctx, id, cid)
}

// StreamExec starts a command and returns an ExecStream for incremental
// consumption. The stream polls Cella's cursor API and delivers chunks
// through an ExecStream until the command terminates. The caller MUST call
// Close when done.
func (p *CellaSandboxProvider) StreamExec(ctx context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecStream, error) {
	cid, err := p.startCommand(ctx, id, opts)
	if err != nil {
		return nil, err
	}
	// Derive a cancellable context so Close can unwind the pump promptly even if
	// the caller stops draining (otherwise pump blocks on the buffered channel
	// until the parent ctx ends, leaking the goroutine and its poll requests).
	streamCtx, cancel := context.WithCancel(ctx)
	s := &execStream{
		ch:     make(chan []byte, 16),
		cancel: cancel,
	}
	go s.pump(streamCtx, p, id, cid)
	return s, nil
}

// ReadFile reads a file from the sandbox using base64-safe cat via Exec.
//
// Command: sh -lc 'base64 < "$0"' path
//
// The base64 encoding is necessary for binary safety — Cella's command output
// passes through JSON encoding which would corrupt arbitrary bytes.
// Non-zero exit code is returned as an error.
func (p *CellaSandboxProvider) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	res, err := p.Exec(ctx, id, sandbox.ExecOptions{
		// sh -lc SCRIPT $0: the shell sets $0 to the first extra arg.
		// Use $0 in the script to reference the path without quoting
		// ambiguity in the argv layer.
		Argv: []string{"sh", "-lc", `base64 < "$0"`, path},
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: ReadFile %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: ReadFile %q: exit %d: %s",
			path, res.ExitCode, strings.TrimSpace(string(res.Stdout)))
	}
	// Strip any trailing newline that base64(1) appends before decoding.
	encoded := strings.TrimRight(string(res.Stdout), "\n")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("sandbox: ReadFile %q: base64 decode: %w", path, err)
	}
	return data, nil
}

// WriteFile writes data to a file in the sandbox filesystem. Cella commands
// take no stdin, so the data is base64-encoded and embedded in the argv:
//
//	sh -lc 'mkdir -p "$(dirname "$0")"; printf %s "$1" | base64 -d > "$0"' path b64data
//
// $0 = path, $1 = base64-encoded data. This is bounded-size only; large
// file bulk transfer is the job of the lift/drop lifecycle spec.
func (p *CellaSandboxProvider) WriteFile(ctx context.Context, id, path string, data []byte) error {
	b64 := base64.StdEncoding.EncodeToString(data)
	script := `mkdir -p "$(dirname "$0")"; printf %s "$1" | base64 -d > "$0"`
	res, err := p.Exec(ctx, id, sandbox.ExecOptions{
		Argv: []string{"sh", "-lc", script, path, b64},
	})
	if err != nil {
		return fmt.Errorf("sandbox: WriteFile %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("sandbox: WriteFile %q: exit %d: %s",
			path, res.ExitCode, strings.TrimSpace(string(res.Stdout)))
	}
	return nil
}

// ListFiles lists immediate children of path in the sandbox filesystem.
// It runs:
//
//	sh -lc 'find "$0" -maxdepth 1 -mindepth 1 -printf "%y\t%s\t%m\t%f\n"' path
//
// Output format per line: type(f|d)\tsize\toctal_mode\tname
// The parser and the shell format agree; trailing newlines are tolerated.
func (p *CellaSandboxProvider) ListFiles(ctx context.Context, id, path string) ([]sandbox.FileInfo, error) {
	// Use printf format: type TAB size TAB octal_mode TAB filename NEWLINE
	// %y = file type (f=regular, d=directory, l=symlink, ...)
	// %s = file size in bytes
	// %m = permission bits in octal
	// %f = file name (base name only)
	script := `find "$0" -maxdepth 1 -mindepth 1 -printf "%y\t%s\t%m\t%f\n"`
	res, err := p.Exec(ctx, id, sandbox.ExecOptions{
		Argv: []string{"sh", "-lc", script, path},
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: ListFiles %q: %w", path, err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("sandbox: ListFiles %q: exit %d: %s",
			path, res.ExitCode, strings.TrimSpace(string(res.Stdout)))
	}
	return parseListFiles(string(res.Stdout))
}

// HealthCheck returns nil if the sandbox is in state "running", ErrNotFound
// if the sandbox does not exist, and an *APIError for any other backend
// error or for a non-running state.
func (p *CellaSandboxProvider) HealthCheck(ctx context.Context, id string) error {
	var cs cellaSandbox
	if err := p.do(ctx, http.MethodGet, "/v1/sandboxes/"+id, nil, &cs); err != nil {
		return err
	}
	if cs.State != "running" {
		return &sandbox.APIError{
			Status:  0,
			Code:    "sandbox_not_running",
			Message: fmt.Sprintf("sandbox %s is in state %q, want running", id, cs.State),
		}
	}
	return nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// startCommand POSTs a command to Cella with detach=true and returns the
// command_id.
func (p *CellaSandboxProvider) startCommand(ctx context.Context, sandboxID string, opts sandbox.ExecOptions) (string, error) {
	req := cellaCreateCommandReq{
		Argv:   opts.Argv,
		Env:    opts.Env,
		Cwd:    opts.Cwd,
		Detach: true,
	}
	var cmd cellaCommand
	path := "/v1/sandboxes/" + sandboxID + "/commands"
	if err := p.do(ctx, http.MethodPost, path, req, &cmd); err != nil {
		return "", err
	}
	return cmd.CommandID, nil
}

// collectLogs polls GET /v1/sandboxes/{id}/commands/{cid}/logs?stream=false
// advancing the cursor until phase != "running", then assembles an ExecResult.
// It respects ctx cancellation between polls.
func (p *CellaSandboxProvider) collectLogs(ctx context.Context, sandboxID, commandID string) (sandbox.ExecResult, error) {
	var (
		buf    bytes.Buffer
		cursor int64
	)
	for {
		lr, err := p.fetchLogs(ctx, sandboxID, commandID, cursor)
		if err != nil {
			return sandbox.ExecResult{}, err
		}
		buf.WriteString(lr.Bytes)
		cursor = lr.NextCursor
		if lr.Phase != "running" {
			return sandbox.ExecResult{
				Stdout:   buf.Bytes(),
				ExitCode: lr.ExitCode,
				Phase:    lr.Phase,
			}, nil
		}
		// Wait before the next poll, but respect cancellation.
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// fetchLogs fetches one page of logs from the cursor.
func (p *CellaSandboxProvider) fetchLogs(ctx context.Context, sandboxID, commandID string, cursor int64) (cellaLogsResp, error) {
	rawPath := fmt.Sprintf("/v1/sandboxes/%s/commands/%s/logs", sandboxID, commandID)
	u, err := url.Parse(p.baseURL + rawPath)
	if err != nil {
		return cellaLogsResp{}, err
	}
	q := u.Query()
	q.Set("stream", "false")
	q.Set("cursor", fmt.Sprintf("%d", cursor))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return cellaLogsResp{}, err
	}
	if err := p.setBearer(ctx, req); err != nil {
		return cellaLogsResp{}, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return cellaLogsResp{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return cellaLogsResp{}, parseAPIError(resp)
	}
	var lr cellaLogsResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return cellaLogsResp{}, fmt.Errorf("cella: decode logs response: %w", err)
	}
	return lr, nil
}

// do executes an HTTP request against the Cella API.
//   - method/path: HTTP verb and path relative to baseURL.
//   - body: marshalled as JSON when non-nil; omitted (no body) when nil.
//   - out: the response JSON is decoded into out when non-nil and the
//     response status is 2xx. Pass nil for methods that return no body
//     (e.g. DELETE 204).
func (p *CellaSandboxProvider) do(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cella: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("cella: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := p.setBearer(ctx, req); err != nil {
		return err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		// 204: no body expected.
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("cella: decode response: %w", err)
		}
	}
	return nil
}

// setBearer fetches a token from the TokenSource and sets the Authorization
// header on req.
func (p *CellaSandboxProvider) setBearer(ctx context.Context, req *http.Request) error {
	tok, err := p.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("cella: token source: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return nil
}

// parseAPIError reads a Cella error envelope from resp.Body and converts it
// to a typed Go error.
//
// Mapping:
//   - 404 → sandbox.ErrNotFound
//   - 409 → sandbox.ErrConflict
//   - others → *sandbox.APIError with code/message/request_id from body
func parseAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var ce cellaError
	_ = json.Unmarshal(body, &ce) // best-effort; fields may be empty on non-JSON bodies

	switch resp.StatusCode {
	case http.StatusNotFound:
		return sandbox.ErrNotFound
	case http.StatusConflict:
		return sandbox.ErrConflict
	default:
		return &sandbox.APIError{
			Status:    resp.StatusCode,
			Code:      ce.Code,
			Message:   ce.Message,
			RequestID: ce.RequestID,
		}
	}
}

// toSandbox converts a Cella wire sandbox to the interface-layer handle.
func toSandbox(cs cellaSandbox) sandbox.Sandbox {
	return sandbox.Sandbox{
		ID:        cs.ID,
		Name:      cs.Name,
		State:     sandbox.SandboxState(cs.State),
		Tier:      cs.Tier,
		CreatedAt: cs.CreatedAt,
	}
}

// isNotFound reports whether err is or wraps sandbox.ErrNotFound.
func isNotFound(err error) bool {
	return errors.Is(err, sandbox.ErrNotFound)
}

// parseListFiles parses the TAB-delimited output produced by the ListFiles
// find(1) command:
//
//	type\tsize\toctal_mode\tname\n
//
// type is "f" (regular), "d" (directory), or another character for other
// entry types. Trailing newlines and blank lines are silently skipped.
func parseListFiles(output string) ([]sandbox.FileInfo, error) {
	var entries []sandbox.FileInfo
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			return nil, fmt.Errorf("sandbox: ListFiles: unexpected line %q", line)
		}
		typ, sizeStr, modeStr, name := parts[0], parts[1], parts[2], parts[3]

		var size int64
		if _, err := fmt.Sscanf(sizeStr, "%d", &size); err != nil {
			return nil, fmt.Errorf("sandbox: ListFiles: parse size %q in line %q: %w", sizeStr, line, err)
		}

		var mode uint32
		if _, err := fmt.Sscanf(modeStr, "%o", &mode); err != nil {
			return nil, fmt.Errorf("sandbox: ListFiles: parse mode %q in line %q: %w", modeStr, line, err)
		}

		entries = append(entries, sandbox.FileInfo{
			Name:  name,
			Size:  size,
			Mode:  mode,
			IsDir: typ == "d",
		})
	}
	return entries, nil
}

// ── execStream ────────────────────────────────────────────────────────────────

// execStream implements sandbox.ExecStream using cursor-poll delivery.
type execStream struct {
	ch        chan []byte
	cancel    context.CancelFunc // cancels pump's context; invoked by Close
	closeOnce sync.Once
	mu        sync.Mutex
	result    sandbox.ExecResult
	err       error // transport/cancellation error, surfaced by Recv after the channel closes
}

// pump runs in a goroutine, polling Cella and sending chunks to ch until
// the command terminates or ctx is cancelled.
func (s *execStream) pump(ctx context.Context, p *CellaSandboxProvider, sandboxID, commandID string) {
	defer close(s.ch)
	var cursor int64
	var buf bytes.Buffer
	for {
		lr, err := p.fetchLogs(ctx, sandboxID, commandID, cursor)
		if err != nil {
			// Transport / context errors: record the error so Recv can surface it
			// (per the ExecStream contract a non-EOF error signals a transport
			// failure) instead of being indistinguishable from a clean empty result.
			s.mu.Lock()
			s.err = err
			s.mu.Unlock()
			return
		}
		if lr.Bytes != "" {
			chunk := []byte(lr.Bytes)
			buf.Write(chunk)
			select {
			case s.ch <- chunk:
			case <-ctx.Done():
				s.mu.Lock()
				s.err = ctx.Err()
				s.mu.Unlock()
				return
			}
		}
		cursor = lr.NextCursor
		if lr.Phase != "running" {
			s.mu.Lock()
			s.result = sandbox.ExecResult{
				Stdout:   buf.Bytes(),
				ExitCode: lr.ExitCode,
				Phase:    lr.Phase,
			}
			s.mu.Unlock()
			return
		}
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.err = ctx.Err()
			s.mu.Unlock()
			return
		case <-time.After(pollInterval):
		}
	}
}

// Recv returns the next chunk from the stream. Once the channel is drained it
// returns the recorded transport/cancellation error if one occurred, otherwise
// io.EOF to signal clean termination (per the ExecStream contract).
func (s *execStream) Recv() ([]byte, error) {
	chunk, ok := <-s.ch
	if !ok {
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return chunk, nil
}

// Result returns the terminal ExecResult. Callers MUST drain Recv until
// io.EOF before calling Result.
func (s *execStream) Result() sandbox.ExecResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

// Close cancels the pump goroutine's context so it unwinds promptly, releasing
// the in-flight poll regardless of drain state (per the ExecStream contract).
// Safe to call multiple times.
func (s *execStream) Close() error {
	s.closeOnce.Do(s.cancel)
	return nil
}
