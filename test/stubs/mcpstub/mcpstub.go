// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package mcpstub is a stub of a remote Model Context Protocol server over
// streamable HTTP, as a connection service serves one at a connection's
// MCP address (spec 069): an HTTP server on loopback that speaks revision
// 2026-07-28, holding each request's mirrored headers to its body as the
// transport says, or, in its legacy mode, only revision 2025-11-25 with
// its initialize handshake and session. It lists the tools a test gives
// it, a page at a time, answers a call as one JSON object or an event
// stream with a progress notification before the response, answers every
// request 409 connection_not_connected once a test disconnects it, and
// records every request. It is a test artifact.
package mcpstub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The revisions the stub speaks.
const (
	Revision = "2026-07-28"
	Legacy   = "2025-11-25"
)

// Tool is one tool the stub lists, and how it answers a call: Answer is
// the call's result object for its arguments, sent after a progress
// notification on an event stream when Stream is set.
type Tool struct {
	Name        string                         `json:"name"`
	Description string                         `json:"description,omitempty"`
	InputSchema json.RawMessage                `json:"inputSchema"`
	Answer      func(args json.RawMessage) any `json:"-"`
	Stream      bool                           `json:"-"`
	// NotConnected answers a call 409 connection_not_connected with this
	// reason, as a connection service does when the other service asks
	// for more than the person granted; empty answers it.
	NotConnected string `json:"-"`
}

// Options configure the stub.
type Options struct {
	// Admit reports whether a request's bearer is one the stub takes; nil
	// takes any, as a connection service takes any session token it
	// verifies.
	Admit func(bearer string) bool
	// Legacy speaks revision 2025-11-25 alone: a request of the current
	// revision is answered 400 with no JSON-RPC error.
	Legacy bool
	// PageSize is how many tools a page of tools/list holds; zero lists
	// them all on one.
	PageSize int
	Tools    []Tool
}

// Request is one request the stub received.
type Request struct {
	Method string
	Header http.Header
	Body   []byte
}

// Server is the stub.
type Server struct {
	srv *httptest.Server
	o   Options

	mu       sync.Mutex
	requests []Request
	sessions map[string]bool
	next     int
	// reason, set, answers every request 409 connection_not_connected.
	reason string
	scopes []string
}

// New starts the stub, closed when the test ends.
func New(t testing.TB, o Options) *Server {
	t.Helper()
	s := &Server{o: o, sessions: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the server's MCP address.
func (s *Server) URL() string { return s.srv.URL + "/mcp" }

// Requests are the requests received so far, oldest first.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Disconnect answers every later request 409 connection_not_connected with
// reason and scopes, as a connection service does for a connection the
// person has to connect again.
func (s *Server) Disconnect(reason string, scopes ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reason, s.scopes = reason, scopes
}

// EndSessions forgets every session of the legacy mode, so the next
// request in one is answered 404.
func (s *Server) EndSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.sessions)
}

// message is a JSON-RPC request or notification.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Cursor    string          `json:"cursor"`
		Meta      map[string]any  `json:"_meta"`
	} `json:"params"`
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil || r.Method != http.MethodPost {
		http.Error(w, "not one JSON-RPC message", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: m.Method, Header: r.Header.Clone(), Body: body})
	reason, scopes := s.reason, s.scopes
	s.mu.Unlock()
	switch {
	case s.o.Admit != nil && !s.o.Admit(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")):
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case reason != "":
		notConnected(w, reason, scopes)
		return
	case !strings.Contains(r.Header.Get("Accept"), "application/json") || !strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
		http.Error(w, "accept both answers", http.StatusNotAcceptable)
		return
	}
	if s.o.Legacy {
		s.legacy(w, r, m)
		return
	}
	if why := mismatch(r.Header, m); why != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32020, "message": "Header mismatch: " + why}})
		return
	}
	s.answer(w, m)
}

// mismatch is why a request's mirrored headers differ from its body, ""
// when they agree.
func mismatch(h http.Header, m message) string {
	version, _ := m.Params.Meta["io.modelcontextprotocol/protocolVersion"].(string)
	switch {
	case h.Get("MCP-Protocol-Version") != Revision || version != Revision:
		return fmt.Sprintf("MCP-Protocol-Version %q, the body's %q", h.Get("MCP-Protocol-Version"), version)
	case h.Get("Mcp-Method") != m.Method:
		return fmt.Sprintf("Mcp-Method %q, the body's %q", h.Get("Mcp-Method"), m.Method)
	case m.Method == "tools/call" && decode(h.Get("Mcp-Name")) != m.Params.Name:
		return fmt.Sprintf("Mcp-Name %q, the body's %q", h.Get("Mcp-Name"), m.Params.Name)
	}
	return ""
}

// decode is a header value in the transport's base64 form decoded.
func decode(v string) string {
	if inner, ok := strings.CutPrefix(v, "=?base64?"); ok {
		if inner, ok = strings.CutSuffix(inner, "?="); ok {
			if b, err := base64.StdEncoding.DecodeString(inner); err == nil {
				return string(b)
			}
		}
	}
	return v
}

// legacy speaks revision 2025-11-25: a request of the current revision is
// a 400 with no JSON-RPC error, initialize opens a session, and every
// later request names a session the stub holds.
func (s *Server) legacy(w http.ResponseWriter, r *http.Request, m message) {
	if r.Header.Get("MCP-Protocol-Version") == Revision {
		http.Error(w, "Bad Request: no valid session", http.StatusBadRequest)
		return
	}
	switch m.Method {
	case "initialize":
		s.mu.Lock()
		s.next++
		id := "sess-" + strconv.Itoa(s.next)
		s.sessions[id] = true
		s.mu.Unlock()
		w.Header().Set("Mcp-Session-Id", id)
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{
			"protocolVersion": Legacy, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "mcpstub", "version": "1"},
		}})
		return
	}
	s.mu.Lock()
	known := s.sessions[r.Header.Get("Mcp-Session-Id")]
	s.mu.Unlock()
	switch {
	case !known:
		http.Error(w, "no such session", http.StatusNotFound)
	case r.Header.Get("MCP-Protocol-Version") != Legacy:
		http.Error(w, "unsupported protocol version", http.StatusBadRequest)
	case m.Method == "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	default:
		s.answer(w, m)
	}
}

// answer answers tools/list and tools/call, and any other method with the
// JSON-RPC error method not found.
func (s *Server) answer(w http.ResponseWriter, m message) {
	switch m.Method {
	case "tools/list":
		start := 0
		if m.Params.Cursor != "" {
			n, err := strconv.Atoi(m.Params.Cursor)
			if err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "bad cursor"}})
				return
			}
			start = n
		}
		end := len(s.o.Tools)
		if s.o.PageSize > 0 {
			end = min(start+s.o.PageSize, end)
		}
		page := map[string]any{"resultType": "complete", "tools": s.o.Tools[start:end]}
		if end < len(s.o.Tools) {
			page["nextCursor"] = strconv.Itoa(end)
		}
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": page})
	case "tools/call":
		i := slices.IndexFunc(s.o.Tools, func(t Tool) bool { return t.Name == m.Params.Name })
		switch {
		case i >= 0 && s.o.Tools[i].NotConnected != "":
			notConnected(w, s.o.Tools[i].NotConnected, nil)
			return
		case i < 0 || s.o.Tools[i].Answer == nil:
			writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32602, "message": "Unknown tool: " + m.Params.Name}})
			return
		}
		t := s.o.Tools[i]
		res := map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": t.Answer(m.Params.Arguments)}
		if !t.Stream {
			writeJSON(w, http.StatusOK, res)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		progress := map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": "p", "progress": 1}}
		for _, e := range []any{progress, res} {
			b, err := json.Marshal(e)
			if err != nil {
				return
			}
			if _, err := fmt.Fprintf(w, ": keep-alive\n\nevent: message\ndata: %s\n\n", b); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "Method not found"}})
	}
}

// notConnected answers 409 connection_not_connected with reason and
// scopes, in a connection service's error envelope.
func notConnected(w http.ResponseWriter, reason string, scopes []string) {
	writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]any{
		"code": "connection_not_connected", "message": "This connection needs to be connected again.",
		"details": map[string]any{"reason": reason, "retryable": false, "state": "needs_reconnect", "scopes": scopes},
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
