// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package rpc serves a sandbox.Provider over a byte stream, so the control plane
// can drive a remote machine's filesystem and exec as its sandbox (mode 2,
// interactive-session-modes). It is transport-agnostic: Serve and NewClient run
// over any io.ReadWriteCloser (an in-memory net.Pipe in tests, a tunnel stream in
// production).
//
// Scope (walking skeleton): the unary Provider methods — Create, Destroy, Exec,
// ReadFile, WriteFile, ListFiles, HealthCheck. StreamExec (streaming output),
// per-call exec consent, and tunnel wiring are named follow-on leaves in
// session-laptop-sandbox-rpc; the client's StreamExec returns ErrStreamUnsupported
// here. One request is in flight at a time per connection (the loop dispatches
// tools sequentially); stream multiplexing belongs to the transport (yamux) below.
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

// ErrStreamUnsupported is returned by the client's StreamExec: streaming exec over
// the wire is a named follow-on leaf, out of the skeleton's scope.
var ErrStreamUnsupported = errors.New("sandbox/rpc: StreamExec not supported over this transport yet")

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
		resp := dispatch(ctx, provider, &req)
		if err := enc.Encode(&resp); err != nil {
			return fmt.Errorf("sandbox/rpc: encode response: %w", err)
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

// StreamExec is not supported over this transport yet (named follow-on leaf).
func (c *client) StreamExec(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecStream, error) {
	return nil, ErrStreamUnsupported
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
