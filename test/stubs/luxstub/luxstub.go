// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package luxstub is the stub Lux of spec 026: an HTTP server on
// loopback that serves the Messages, Responses and Chat Completions
// doors and Lux's discovery document, decodes each request with that dialect's frontend codec, and
// streams the scripted reply for the request's model back through the
// same codec. It records every request and injects the failures a reply
// names. It is a test artifact.
package luxstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect"
	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/openaichat"
	"latere.ai/x/pkg/llmdialect/openairesp"
)

// The door paths, under the stub's URL.
const (
	PathMessages  = "/anthropic/v1/messages"
	PathResponses = "/openai/v1/responses"
	PathChat      = "/openai/v1/chat/completions"
	// PathDiscovery is Lux's discovery document, which names the doors.
	PathDiscovery = "/.well-known/lux"
)

// Failure is an injected failure before a reply.
type Failure struct {
	Status     int
	RetryAfter string
	Body       string
	// Times is how many requests fail before the reply is served; zero
	// is once.
	Times int
	// Cut ends the stream before its terminal events instead of
	// answering with an HTTP error.
	Cut bool
	// Event is raw SSE written after the stream's first events, in
	// place of the rest: an error event of the dialect.
	Event string
}

// Reply is one scripted answer.
type Reply struct {
	Response ir.Response
	Fail     *Failure
	// Expect checks the decoded request; an error answers 400.
	Expect func(*ir.Request) error
	// Respond edits the response when it is served, for a reply that
	// depends on what the session has done by then.
	Respond func(*ir.Request, *ir.Response)
}

// Recorded is one request the stub received.
type Recorded struct {
	Dialect ir.Dialect
	Model   string
	Header  http.Header
	Body    []byte
	Request *ir.Request
}

// Server is the stub.
type Server struct {
	srv *httptest.Server

	mu       sync.Mutex
	replies  map[string][]Reply
	failed   map[string]int
	requests []Recorded
}

// New starts a stub for the test and closes it when the test ends.
func New(t testing.TB) *Server {
	s := &Server{replies: map[string][]Reply{}, failed: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the stub's base URL; a connection's base URL is URL plus
// "/anthropic" or "/openai".
func (s *Server) URL() string { return s.srv.URL }

// Script queues replies for a model, served in order.
func (s *Server) Script(model string, replies ...Reply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replies[model] = append(s.replies[model], replies...)
}

// Requests returns every request so far.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.requests...)
}

func frontend(path string) (llmdialect.Frontend, ir.Dialect, bool) {
	switch {
	case strings.HasSuffix(path, PathMessages):
		return anthropic.NewFrontend(), ir.DialectAnthropicMessages, true
	case strings.HasSuffix(path, PathResponses):
		return openairesp.NewFrontend(), ir.DialectOpenAIResponses, true
	case strings.HasSuffix(path, PathChat):
		return openaichat.NewFrontend(), ir.DialectOpenAIChat, true
	}
	return nil, "", false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == PathDiscovery {
		s.discovery(w)
		return
	}
	fe, d, ok := frontend(r.URL.Path)
	if !ok || r.Method != http.MethodPost {
		http.Error(w, `{"error":{"type":"not_found_error","message":"no such door"}}`, http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, err := fe.DecodeRequest(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, Recorded{Dialect: d, Model: req.Model, Header: r.Header.Clone(), Body: body, Request: req})
	queue := s.replies[req.Model]
	if len(queue) == 0 {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("no scripted reply for model %q", req.Model))
		return
	}
	reply := queue[0]
	if f := reply.Fail; f != nil && s.failed[req.Model] < max(f.Times, 1) {
		s.failed[req.Model]++
		s.mu.Unlock()
		s.fail(w, fe, reply, *f)
		return
	}
	s.replies[req.Model] = queue[1:]
	s.failed[req.Model] = 0
	s.mu.Unlock()
	if reply.Expect != nil {
		if err := reply.Expect(req); err != nil {
			writeError(w, http.StatusBadRequest, "expectation_failed", err.Error())
			return
		}
	}
	resp := reply.Response
	if reply.Respond != nil {
		reply.Respond(req, &resp)
	}
	s.stream(w, fe, resp, false)
}

func (s *Server) fail(w http.ResponseWriter, fe llmdialect.Frontend, reply Reply, f Failure) {
	if f.Cut || f.Event != "" {
		s.stream(w, fe, reply.Response, true)
		if f.Event != "" {
			if _, err := io.WriteString(w, f.Event); err != nil {
				return
			}
		}
		return
	}
	if f.RetryAfter != "" {
		w.Header().Set("Retry-After", f.RetryAfter)
	}
	body := f.Body
	if body == "" {
		body = `{"error":{"type":"overloaded_error","message":"injected"}}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(f.Status)
	if _, err := io.WriteString(w, body); err != nil {
		return
	}
}

// discovery answers Lux's discovery document with the stub's doors,
// each the stub's URL plus the dialect, as a Lux at the root names them.
func (s *Server) discovery(w http.ResponseWriter) {
	doors := map[string]string{}
	for _, d := range []string{"anthropic", "openai", "gemini", "lux"} {
		doors[d] = s.srv.URL + "/" + d
	}
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client is gone; the stub has nothing left
	// to answer.
	if err := json.NewEncoder(w).Encode(map[string]any{"name": "lux", "doors": doors}); err != nil {
		return
	}
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failed write means the client is gone; the stub has nothing left
	// to answer.
	if _, err := fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, msg); err != nil {
		return
	}
}

// stream writes the response as the dialect's SSE, through its
// frontend encoder; cut stops before the terminal events.
func (s *Server) stream(w http.ResponseWriter, fe llmdialect.Frontend, resp ir.Response, cut bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	enc := fe.NewEventEncoder(w)
	evs := Events(resp)
	if cut {
		evs = evs[:len(evs)-2]
	}
	for _, ev := range evs {
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// Events is the IR event sequence of a whole response: the stream
// grammar with one delta per block.
func Events(resp ir.Response) []ir.Event {
	id := resp.ID
	if id == "" {
		id = "msg_stub"
	}
	start := ir.Usage{InputTokens: resp.Usage.InputTokens, CacheReadInputTokens: resp.Usage.CacheReadInputTokens, CacheWriteInputTokens: resp.Usage.CacheWriteInputTokens}
	evs := []ir.Event{{Type: ir.EventMessageStart, ID: id, Model: resp.Model, Usage: &start}}
	for i, b := range resp.Blocks {
		head := ir.Block{Type: b.Type}
		switch b.Type {
		case ir.BlockToolUse:
			head.ToolUse = &ir.ToolUse{ID: b.ToolUse.ID, Name: b.ToolUse.Name}
		case ir.BlockRedactedThinking:
			head.Redacted = b.Redacted
		}
		evs = append(evs, ir.Event{Type: ir.EventBlockStart, Index: i, Block: &head})
		switch b.Type {
		case ir.BlockText:
			evs = append(evs, ir.Event{Type: ir.EventTextDelta, Index: i, Delta: b.Text})
		case ir.BlockThinking:
			evs = append(evs, ir.Event{Type: ir.EventThinkingDelta, Index: i, Delta: b.Text})
			if b.Signature != "" {
				evs = append(evs, ir.Event{Type: ir.EventSignatureDelta, Index: i, Delta: b.Signature})
			}
		case ir.BlockToolUse:
			evs = append(evs, ir.Event{Type: ir.EventArgsDelta, Index: i, Delta: string(b.ToolUse.Args)})
		}
		evs = append(evs, ir.Event{Type: ir.EventBlockStop, Index: i})
	}
	usage := resp.Usage
	stop := resp.StopReason
	if stop == "" {
		stop = ir.StopEndTurn
	}
	return append(evs,
		ir.Event{Type: ir.EventMessageDelta, StopReason: stop, Usage: &usage},
		ir.Event{Type: ir.EventMessageStop},
	)
}
