// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package mcp reaches a remote Model Context Protocol server over
// streamable HTTP (specs 021 and 069): revision 2026-07-28 first, with a
// message per POST and its metadata in the body and in headers, and the
// initialize handshake of an earlier revision for a server that answers
// 400 to the current one. A server's tools are offered to the model under
// mcp__<connection>__<tool>.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"latere.ai/x/pkg/otel"
)

// The protocol revisions the client speaks: Revision first, and Legacy
// in the handshake it falls back to.
const (
	Revision = "2026-07-28"
	Legacy   = "2025-11-25"
)

// MaxResponse is the most bytes of one answer the client reads.
const MaxResponse = 16 << 20

// The JSON-RPC error codes the current revision defines; a 400 whose body
// is an error of another code, or no JSON-RPC error at all, is a server of
// an earlier revision.
const (
	minModernCode = -32099
	maxModernCode = -32020
)

// CodeNotConnected is the error code a connection service answers 409
// with for a connection the person has to connect again (spec 069).
const CodeNotConnected = "connection_not_connected"

// NotConnected is a connection service's answer that the connection is
// not connected: its reason, the connection's state, and the scopes the
// other service asked for, when it named them.
type NotConnected struct {
	Reason string
	State  string
	Scopes []string
}

func (e *NotConnected) Error() string {
	return "the connection is not connected: " + cmpOr(e.Reason, "no reason given")
}

// RPCError is a JSON-RPC error the server answered.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("the server answered error %d: %s", e.Code, e.Message)
}

// StatusError is an HTTP answer the client does not read as the protocol's:
// its status and the start of its body.
type StatusError struct {
	Status int
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("the server answered %d %s: %s", e.Status, http.StatusText(e.Status), e.Body)
}

// errLegacy is a 400 to a request of the current revision whose body is
// not an error of that revision: the server speaks an earlier one.
var errLegacy = errors.New("mcp: the server answered 400 with no error of the current revision")

// Client is one server's client. Token answers the bearer of each request,
// asked again for every request; Version is the client's own version in
// clientInfo. It is safe for concurrent use.
type Client struct {
	url     string
	token   func(context.Context) (string, error)
	http    *http.Client
	version string

	ids atomic.Int64
	mu  sync.Mutex
	// legacy is a server of an earlier revision, reached in the session
	// its handshake opened at the negotiated version.
	legacy     bool
	sessionID  string
	negotiated string
}

// NewClient is the client of the server at url, over hc, an instrumented
// client when nil.
func NewClient(url string, token func(context.Context) (string, error), hc *http.Client, version string) *Client {
	if hc == nil {
		hc = otel.HTTPClient()
	}
	return &Client{url: url, token: token, http: hc, version: version}
}

// response is a JSON-RPC response.
type response struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// call sends one request and answers its result: in the current revision
// until the server shows it speaks an earlier one, and in that revision's
// session after. name is the request's Mcp-Name, header its further
// headers.
func (c *Client) call(ctx context.Context, method string, params map[string]any, name string, header http.Header) (json.RawMessage, error) {
	c.mu.Lock()
	legacy := c.legacy
	c.mu.Unlock()
	if !legacy {
		res, err := c.modern(ctx, method, params, name, header)
		if !errors.Is(err, errLegacy) {
			return res, err
		}
		if err := c.initialize(ctx); err != nil {
			return nil, err
		}
	}
	res, err := c.earlier(ctx, method, params)
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		// A server that ended the session answers 404 to its id; one new
		// session is opened and the request sent again.
		if err := c.initialize(ctx); err != nil {
			return nil, err
		}
		return c.earlier(ctx, method, params)
	}
	return res, err
}

// modern sends a request of the current revision.
func (c *Client) modern(ctx context.Context, method string, params map[string]any, name string, header http.Header) (json.RawMessage, error) {
	p := maps.Clone(params)
	if p == nil {
		p = map[string]any{}
	}
	p["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    Revision,
		"io.modelcontextprotocol/clientInfo":         map[string]string{"name": "topos", "version": c.version},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	h := header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("MCP-Protocol-Version", Revision)
	h.Set("Mcp-Method", method)
	if name != "" {
		h.Set("Mcp-Name", headerValue(name))
	}
	return c.send(ctx, method, p, h, true)
}

// earlier sends a request of the earlier revision, in its session.
func (c *Client) earlier(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	id, version := c.sessionID, c.negotiated
	c.mu.Unlock()
	h := http.Header{}
	h.Set("MCP-Protocol-Version", version)
	if id != "" {
		h.Set("Mcp-Session-Id", id)
	}
	return c.send(ctx, method, params, h, false)
}

// initialize makes the earlier revision's handshake: initialize, whose
// answer names the version and the session, then
// notifications/initialized in that session.
func (c *Client) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": Legacy,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "topos", "version": c.version},
	}
	req, err := c.request(ctx, map[string]any{"jsonrpc": "2.0", "id": c.ids.Add(1), "method": "initialize", "params": params}, http.Header{})
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	defer closeBody(resp)
	res, err := c.answer(resp, idOf(req), false)
	if err != nil {
		return fmt.Errorf("mcp: initialize: %w", err)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(res, &init); err != nil || init.ProtocolVersion == "" {
		return fmt.Errorf("mcp: initialize answered no protocol version: %s", snippet(res))
	}
	c.mu.Lock()
	c.legacy, c.sessionID, c.negotiated = true, resp.Header.Get("Mcp-Session-Id"), init.ProtocolVersion
	c.mu.Unlock()
	h := http.Header{}
	h.Set("MCP-Protocol-Version", init.ProtocolVersion)
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		h.Set("Mcp-Session-Id", id)
	}
	note, err := c.request(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}, h)
	if err != nil {
		return err
	}
	nresp, err := c.http.Do(note)
	if err != nil {
		return fmt.Errorf("mcp: notifications/initialized: %w", err)
	}
	defer closeBody(nresp)
	if nresp.StatusCode/100 != 2 {
		return fmt.Errorf("mcp: notifications/initialized: %w", statusError(nresp))
	}
	return nil
}

// send posts one request and reads its answer; modern is whether the
// request is of the current revision, whose 400 may say the server speaks
// an earlier one.
func (c *Client) send(ctx context.Context, method string, params map[string]any, h http.Header, modern bool) (json.RawMessage, error) {
	req, err := c.request(ctx, map[string]any{"jsonrpc": "2.0", "id": c.ids.Add(1), "method": method, "params": params}, h)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %s: %w", method, err)
	}
	defer closeBody(resp)
	return c.answer(resp, idOf(req), modern)
}

// request is a POST of body with the transport's headers, h and the
// bearer, its id in the context of the request for answer to match.
func (c *Client) request(ctx context.Context, body map[string]any, h http.Header) (*http.Request, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("mcp: encode the request: %w", err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: the session's token: %w", err)
	}
	req, err := http.NewRequestWithContext(withID(ctx, body["id"]), http.MethodPost, c.url, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	maps.Copy(req.Header, h)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

type idKey struct{}

func withID(ctx context.Context, id any) context.Context { return context.WithValue(ctx, idKey{}, id) }

// idOf is the JSON of the id a request carries.
func idOf(req *http.Request) string {
	b, err := json.Marshal(req.Context().Value(idKey{}))
	if err != nil {
		return ""
	}
	return string(b)
}

// answer reads the answer to the request of id: one JSON object, or an
// event stream read until the response that carries id.
func (c *Client) answer(resp *http.Response, id string, modern bool) (json.RawMessage, error) {
	body := io.LimitReader(resp.Body, MaxResponse)
	switch {
	case resp.StatusCode == http.StatusConflict:
		return nil, conflict(resp)
	case resp.StatusCode == http.StatusBadRequest && modern:
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("mcp: read the answer: %w", err)
		}
		var r response
		if json.Unmarshal(b, &r) == nil && r.Error != nil && r.Error.Code >= minModernCode && r.Error.Code <= maxModernCode {
			return nil, r.Error
		}
		return nil, errLegacy
	case resp.StatusCode/100 != 2:
		return nil, statusError(resp)
	}
	var r response
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var err error
		if r, err = readStream(body, id); err != nil {
			return nil, err
		}
	} else if err := json.NewDecoder(body).Decode(&r); err != nil {
		return nil, fmt.Errorf("mcp: the answer is not a JSON-RPC response: %w", err)
	}
	if r.Error != nil {
		return nil, r.Error
	}
	return r.Result, nil
}

// readStream reads an event stream until the response that carries id;
// notifications and other messages on it are read past.
func readStream(r io.Reader, id string) (response, error) {
	br := bufio.NewReader(r)
	var data []string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && len(data) > 0:
			var resp response
			if json.Unmarshal([]byte(strings.Join(data, "\n")), &resp) == nil && string(bytes.TrimSpace(resp.ID)) == id && (resp.Result != nil || resp.Error != nil) {
				return resp, nil
			}
			data = data[:0]
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
		if err == io.EOF {
			return response{}, errors.New("mcp: the stream ended before the response")
		}
		if err != nil {
			return response{}, fmt.Errorf("mcp: read the stream: %w", err)
		}
	}
}

// conflict is a 409: a connection service's connection_not_connected, or
// the status otherwise.
func conflict(resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("mcp: read the answer: %w", err)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Reason string   `json:"reason"`
				State  string   `json:"state"`
				Scopes []string `json:"scopes"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Code == CodeNotConnected {
		d := e.Error.Details
		return &NotConnected{Reason: d.Reason, State: d.State, Scopes: d.Scopes}
	}
	return &StatusError{Status: resp.StatusCode, Body: snippet(b)}
}

func statusError(resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	if err != nil {
		return fmt.Errorf("mcp: read the answer to %d: %w", resp.StatusCode, err)
	}
	return &StatusError{Status: resp.StatusCode, Body: snippet(b)}
}

// snippet is the start of a body, at most 512 bytes on one line.
func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// closeBody drains a little of an answer and closes it, so the connection
// is kept.
func closeBody(resp *http.Response) {
	_, _ = io.CopyN(io.Discard, resp.Body, 4<<10)
	_ = resp.Body.Close()
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// headerValue is a value as a header of the transport carries it: as it
// is when it is visible ASCII and spaces with none at either end and not
// the base64 sentinel's form, and in the sentinel's form otherwise.
func headerValue(v string) string {
	safe := v != "" && strings.TrimSpace(v) == v && !(strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?="))
	for i := 0; safe && i < len(v); i++ {
		safe = v[i] == '\t' || (v[i] >= 0x20 && v[i] <= 0x7e)
	}
	if safe || v == "" {
		return v
	}
	return "=?base64?" + base64Std(v) + "?="
}
