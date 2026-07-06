// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package rpc serves a sandbox.Provider over a byte stream, so the control plane
// can drive a remote machine's filesystem and exec as its sandbox (mode 2,
// interactive-session-modes). It is transport-agnostic: Serve and NewClient run
// over any io.ReadWriteCloser (an in-memory net.Pipe in tests, a tunnel stream in
// production).
//
// Scope: all Provider methods — the unary set (Create, Destroy, Exec, ReadFile,
// WriteFile, ListFiles, HealthCheck) plus StreamExec (streaming output as a
// sequence of frames terminated by the final ExecResult). Per-call exec consent
// and tunnel wiring are named follow-on leaves in session-laptop-sandbox-rpc. One
// request is in flight at a time per connection (the loop dispatches tools
// sequentially, and a StreamExec holds the connection until its stream is closed);
// stream multiplexing belongs to the transport (yamux) below.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"latere.ai/x/topos/sandbox"
)

// streamFrame is one message in a StreamExec response: an output chunk, or the
// terminal frame (EOF) carrying the final ExecResult, or an error. The server
// emits a sequence of {Chunk} frames followed by exactly one {EOF, Result} (or a
// single {Err}); the client reads them in order.
type streamFrame struct {
	Chunk  []byte              `json:"chunk,omitempty"`
	EOF    bool                `json:"eof,omitempty"`
	Result *sandbox.ExecResult `json:"result,omitempty"`
	Err    *errEnvelope        `json:"err,omitempty"`
}

// request is the wire request envelope. Only the fields relevant to Method are
// populated; the rest stay zero.
type request struct {
	Method string                 `json:"method"`
	ID     string                 `json:"id,omitempty"`
	Path   string                 `json:"path,omitempty"`
	Data   []byte                 `json:"data,omitempty"`
	Create *sandbox.CreateOptions `json:"create,omitempty"`
	Exec   *sandbox.ExecOptions   `json:"exec,omitempty"`
}

// response is the wire response envelope. Err is set on failure; otherwise the
// result field matching the request Method is populated.
type response struct {
	Sandbox *sandbox.Sandbox    `json:"sandbox,omitempty"`
	Exec    *sandbox.ExecResult `json:"exec,omitempty"`
	Data    []byte              `json:"data,omitempty"`
	Files   []sandbox.FileInfo  `json:"files,omitempty"`
	Err     *errEnvelope        `json:"err,omitempty"`
}

// errEnvelope carries a provider error across the wire so the client can
// reconstruct the sentinel and errors.Is it.
type errEnvelope struct {
	Kind string            `json:"kind"` // notfound | conflict | confined | api | other
	Msg  string            `json:"msg"`
	API  *sandbox.APIError `json:"api,omitempty"`
}

func toEnvelope(err error) *errEnvelope {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		return &errEnvelope{Kind: "notfound", Msg: err.Error()}
	case errors.Is(err, sandbox.ErrConflict):
		return &errEnvelope{Kind: "conflict", Msg: err.Error()}
	case errors.Is(err, sandbox.ErrConfined):
		return &errEnvelope{Kind: "confined", Msg: err.Error()}
	case errors.Is(err, sandbox.ErrConsentDenied):
		return &errEnvelope{Kind: "consent", Msg: err.Error()}
	}
	var apiErr *sandbox.APIError
	if errors.As(err, &apiErr) {
		return &errEnvelope{Kind: "api", Msg: err.Error(), API: apiErr}
	}
	return &errEnvelope{Kind: "other", Msg: err.Error()}
}

// toError reconstructs a client-side error from the envelope, wrapping the
// matching sentinel so errors.Is works across the wire.
func (e *errEnvelope) toError() error {
	if e == nil {
		return nil
	}
	switch e.Kind {
	case "notfound":
		return fmt.Errorf("%w: %s", sandbox.ErrNotFound, e.Msg)
	case "conflict":
		return fmt.Errorf("%w: %s", sandbox.ErrConflict, e.Msg)
	case "confined":
		return fmt.Errorf("%w: %s", sandbox.ErrConfined, e.Msg)
	case "consent":
		return fmt.Errorf("%w: %s", sandbox.ErrConsentDenied, e.Msg)
	case "api":
		if e.API != nil {
			return e.API
		}
	}
	return errors.New(e.Msg)
}

// Serve reads Provider requests off conn and invokes provider until conn closes
// or a decode error occurs. It returns nil on a clean EOF. Intended composition
// (mode 2): Serve(ctx, conn, sandbox.Confine(host, root)).
func Serve(ctx context.Context, conn io.ReadWriteCloser, provider sandbox.Provider) error {
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("sandbox/rpc: decode request: %w", err)
		}
		if req.Method == "StreamExec" {
			if err := serveStream(ctx, enc, provider, &req); err != nil {
				return err
			}
			continue
		}
		resp := dispatch(ctx, provider, &req)
		if err := enc.Encode(&resp); err != nil {
			return fmt.Errorf("sandbox/rpc: encode response: %w", err)
		}
	}
}

// serveStream runs a StreamExec on the provider and forwards its output as a
// sequence of chunk frames terminated by an EOF frame carrying the final result
// (or a single error frame). It writes exactly one terminal frame so the client's
// stream always ends.
func serveStream(ctx context.Context, enc *json.Encoder, p sandbox.Provider, req *request) error {
	opts := sandbox.ExecOptions{}
	if req.Exec != nil {
		opts = *req.Exec
	}
	stream, err := p.StreamExec(ctx, req.ID, opts)
	if err != nil {
		return enc.Encode(&streamFrame{Err: toEnvelope(err)})
	}
	defer func() { _ = stream.Close() }()
	for {
		chunk, rerr := stream.Recv()
		if len(chunk) > 0 {
			if err := enc.Encode(&streamFrame{Chunk: chunk}); err != nil {
				return fmt.Errorf("sandbox/rpc: encode stream chunk: %w", err)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				res := stream.Result()
				return enc.Encode(&streamFrame{EOF: true, Result: &res})
			}
			return enc.Encode(&streamFrame{Err: toEnvelope(rerr)})
		}
	}
}

// dispatch invokes the one Provider method named by the request and shapes the
// response (result or error envelope).
func dispatch(ctx context.Context, p sandbox.Provider, req *request) response {
	switch req.Method {
	case "Create":
		opts := sandbox.CreateOptions{}
		if req.Create != nil {
			opts = *req.Create
		}
		sb, err := p.Create(ctx, opts)
		if err != nil {
			return response{Err: toEnvelope(err)}
		}
		return response{Sandbox: &sb}
	case "Destroy":
		return response{Err: toEnvelope(p.Destroy(ctx, req.ID))}
	case "Exec":
		opts := sandbox.ExecOptions{}
		if req.Exec != nil {
			opts = *req.Exec
		}
		res, err := p.Exec(ctx, req.ID, opts)
		if err != nil {
			return response{Err: toEnvelope(err)}
		}
		return response{Exec: &res}
	case "ReadFile":
		data, err := p.ReadFile(ctx, req.ID, req.Path)
		if err != nil {
			return response{Err: toEnvelope(err)}
		}
		return response{Data: data}
	case "WriteFile":
		return response{Err: toEnvelope(p.WriteFile(ctx, req.ID, req.Path, req.Data))}
	case "ListFiles":
		files, err := p.ListFiles(ctx, req.ID, req.Path)
		if err != nil {
			return response{Err: toEnvelope(err)}
		}
		return response{Files: files}
	case "HealthCheck":
		return response{Err: toEnvelope(p.HealthCheck(ctx, req.ID))}
	default:
		return response{Err: &errEnvelope{Kind: "other", Msg: "unknown method " + req.Method}}
	}
}

// client is a sandbox.Provider that marshals each call over conn to a Serve peer.
type client struct {
	mu   sync.Mutex // one in-flight request at a time per connection
	enc  *json.Encoder
	dec  *json.Decoder
	conn io.Closer
}

// NewClient returns a sandbox.Provider backed by a Serve peer on the other end of
// conn. The caller owns conn's lifetime; Close on the returned provider closes it.
func NewClient(conn io.ReadWriteCloser) sandbox.Provider {
	return &client{
		enc:  json.NewEncoder(conn),
		dec:  json.NewDecoder(conn),
		conn: conn,
	}
}

// call performs one synchronous request/response round-trip.
func (c *client) call(req *request) (response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(req); err != nil {
		return response{}, fmt.Errorf("sandbox/rpc: send %s: %w", req.Method, err)
	}
	var resp response
	if err := c.dec.Decode(&resp); err != nil {
		return response{}, fmt.Errorf("sandbox/rpc: recv %s: %w", req.Method, err)
	}
	return resp, nil
}

func (c *client) Create(_ context.Context, opts sandbox.CreateOptions) (sandbox.Sandbox, error) {
	resp, err := c.call(&request{Method: "Create", Create: &opts})
	if err != nil {
		return sandbox.Sandbox{}, err
	}
	if resp.Err != nil {
		return sandbox.Sandbox{}, resp.Err.toError()
	}
	if resp.Sandbox == nil {
		return sandbox.Sandbox{}, errors.New("sandbox/rpc: Create: empty response")
	}
	return *resp.Sandbox, nil
}

func (c *client) Destroy(_ context.Context, id string) error {
	resp, err := c.call(&request{Method: "Destroy", ID: id})
	if err != nil {
		return err
	}
	return resp.Err.toError()
}

func (c *client) Exec(_ context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecResult, error) {
	resp, err := c.call(&request{Method: "Exec", ID: id, Exec: &opts})
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	if resp.Err != nil {
		return sandbox.ExecResult{}, resp.Err.toError()
	}
	if resp.Exec == nil {
		return sandbox.ExecResult{}, errors.New("sandbox/rpc: Exec: empty response")
	}
	return *resp.Exec, nil
}

// StreamExec starts a streaming command on the peer. It holds the connection's
// write/read lock for the stream's whole lifetime (one in-flight call per
// connection), so the caller MUST Close the returned stream to release it. The
// first frame is read eagerly so an immediate StreamExec error surfaces here.
func (c *client) StreamExec(_ context.Context, id string, opts sandbox.ExecOptions) (sandbox.ExecStream, error) {
	c.mu.Lock()
	if err := c.enc.Encode(&request{Method: "StreamExec", ID: id, Exec: &opts}); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("sandbox/rpc: send StreamExec: %w", err)
	}
	s := &clientStream{c: c}
	// Peek the first frame: a StreamExec that fails to start returns its error
	// here rather than on the first Recv.
	var fr streamFrame
	if err := c.dec.Decode(&fr); err != nil {
		s.release()
		return nil, fmt.Errorf("sandbox/rpc: recv StreamExec: %w", err)
	}
	if fr.Err != nil {
		s.release()
		return nil, fr.Err.toError()
	}
	s.pending = &fr
	return s, nil
}

// clientStream is a sandbox.ExecStream reading streamFrames off the connection.
type clientStream struct {
	c        *client
	pending  *streamFrame // the eagerly-read (or last-decoded) frame not yet consumed
	result   sandbox.ExecResult
	done     bool
	released bool
}

// release drops the connection lock exactly once.
func (s *clientStream) release() {
	if !s.released {
		s.released = true
		s.c.mu.Unlock()
	}
}

func (s *clientStream) next() (*streamFrame, error) {
	if s.pending != nil {
		fr := s.pending
		s.pending = nil
		return fr, nil
	}
	var fr streamFrame
	if err := s.c.dec.Decode(&fr); err != nil {
		return nil, fmt.Errorf("sandbox/rpc: recv stream frame: %w", err)
	}
	return &fr, nil
}

func (s *clientStream) Recv() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	for {
		fr, err := s.next()
		if err != nil {
			s.done = true
			return nil, err
		}
		if fr.Err != nil {
			s.done = true
			return nil, fr.Err.toError()
		}
		if fr.EOF {
			s.done = true
			if fr.Result != nil {
				s.result = *fr.Result
			}
			return nil, io.EOF
		}
		if len(fr.Chunk) > 0 {
			return fr.Chunk, nil
		}
		// An empty non-terminal frame: skip and read the next.
	}
}

func (s *clientStream) Result() sandbox.ExecResult { return s.result }

// Close drains any remaining frames to keep the connection in sync, then releases
// the connection lock. Safe to call multiple times.
func (s *clientStream) Close() error {
	// Drain to the terminal frame so the next call on this connection reads a
	// fresh response, not our leftovers. Recv sets s.done on EOF or any error, so
	// the loop always terminates; the drained values are intentionally discarded.
	for !s.done {
		_, _ = s.Recv()
	}
	s.release()
	return nil
}

func (c *client) ReadFile(_ context.Context, id, path string) ([]byte, error) {
	resp, err := c.call(&request{Method: "ReadFile", ID: id, Path: path})
	if err != nil {
		return nil, err
	}
	if resp.Err != nil {
		return nil, resp.Err.toError()
	}
	return resp.Data, nil
}

func (c *client) WriteFile(_ context.Context, id, path string, data []byte) error {
	resp, err := c.call(&request{Method: "WriteFile", ID: id, Path: path, Data: data})
	if err != nil {
		return err
	}
	return resp.Err.toError()
}

func (c *client) ListFiles(_ context.Context, id, path string) ([]sandbox.FileInfo, error) {
	resp, err := c.call(&request{Method: "ListFiles", ID: id, Path: path})
	if err != nil {
		return nil, err
	}
	if resp.Err != nil {
		return nil, resp.Err.toError()
	}
	return resp.Files, nil
}

func (c *client) HealthCheck(_ context.Context, id string) error {
	resp, err := c.call(&request{Method: "HealthCheck", ID: id})
	if err != nil {
		return err
	}
	return resp.Err.toError()
}

// Close closes the underlying connection.
func (c *client) Close() error { return c.conn.Close() }
