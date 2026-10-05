// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package luxstub is the stub Lux of spec 026: an HTTP server on
// loopback that serves the Messages, Responses and Chat Completions
// doors, each door's model list, and Lux's discovery document, decodes
// each request with that dialect's frontend codec, and streams the
// scripted reply for the request's model back through the same codec.
// It records every request and injects the failures a reply names. It
// is a test artifact.
package luxstub

import (
	"cmp"
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
	"latere.ai/x/pkg/llmdialect/bridge"
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
	// PathModels is each door's model list, under the door's path.
	PathModels = "/v1/models"
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
	// Raw is a stream body served byte for byte in place of Response:
	// the SSE a provider sent that the frontend encoders cannot write,
	// such as tool call arguments that are not JSON or parallel calls
	// whose arguments interleave.
	Raw string
	// Hold runs before each event of the stream is written, once the
	// events before it reached the client, so a test paces a response
	// the way a model streams one: a wait before a tool_use block, or
	// arguments that take a while.
	Hold func(ir.Event)
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
	models   []bridge.Model
	listed   []http.Header
	// published is the root the discovery document names the doors
	// under; "" is the stub's own URL.
	published string
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

// Publish has the discovery document name the doors under root, as a
// Lux whose public URL is root names them while it is also reached at
// the stub's own address, such as an in-cluster Service.
func (s *Server) Publish(root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.published = root
}

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

// Models sets the entries every door's model list answers, with the
// figures each carries, as a Lux Model declares them.
func (s *Server) Models(models ...bridge.Model) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = append([]bridge.Model(nil), models...)
}

// Listed returns the headers of every model list request so far.
func (s *Server) Listed() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.listed...)
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
	if wire, ok := listWire(r.URL.Path); ok && r.Method == http.MethodGet {
		s.list(w, r, wire)
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
	if reply.Raw != "" {
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, reply.Raw); err != nil {
			return
		}
		return
	}
	resp := reply.Response
	if reply.Respond != nil {
		reply.Respond(req, &resp)
	}
	s.stream(w, fe, resp, false, reply.Hold)
}

func (s *Server) fail(w http.ResponseWriter, fe llmdialect.Frontend, reply Reply, f Failure) {
	if f.Cut || f.Event != "" {
		s.stream(w, fe, reply.Response, true, nil)
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
// each the published root plus the dialect, as a Lux at the root names
// them.
func (s *Server) discovery(w http.ResponseWriter) {
	s.mu.Lock()
	root := cmp.Or(s.published, s.srv.URL)
	s.mu.Unlock()
	doors := map[string]string{}
	for _, d := range []string{"anthropic", "openai", "gemini", "lux"} {
		doors[d] = root + "/" + d
	}
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client is gone; the stub has nothing left
	// to answer.
	if err := json.NewEncoder(w).Encode(map[string]any{"name": "lux", "doors": doors}); err != nil {
		return
	}
}

// listWire is the wire of the door whose model list path is p.
func listWire(p string) (bridge.Wire, bool) {
	switch p {
	case "/anthropic" + PathModels:
		return bridge.WireAnthropic, true
	case "/openai" + PathModels:
		return bridge.WireOpenAI, true
	}
	return "", false
}

// list answers a door's model list in that door's shape.
func (s *Server) list(w http.ResponseWriter, r *http.Request, wire bridge.Wire) {
	s.mu.Lock()
	s.listed = append(s.listed, r.Header.Clone())
	body := bridge.ModelList(wire, s.models)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the client is gone; the stub has nothing left
	// to answer.
	if _, err := w.Write(body); err != nil {
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
// frontend encoder; cut stops before the terminal events. The blocks the
// frontend encoder cannot stream are written as the provider streams
// them (native). A hold runs before each event, after what was written
// before it is flushed to the client.
func (s *Server) stream(w http.ResponseWriter, fe llmdialect.Frontend, resp ir.Response, cut bool, hold func(ir.Event)) {
	w.Header().Set("Content-Type", "text/event-stream")
	enc := fe.NewEventEncoder(w)
	evs := Events(resp)
	if cut {
		evs = evs[:len(evs)-2]
	}
	flusher, _ := w.(http.Flusher)
	for _, ev := range evs {
		if hold != nil {
			if flusher != nil {
				flusher.Flush()
			}
			hold(ev)
		}
		if handled, err := native(w, fe.Name(), resp, ev); handled {
			if err != nil {
				return
			}
			continue
		}
		if err := enc.Encode(ev); err != nil {
			return
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// native writes the start or the stop of a block the dialect's frontend
// encoder cannot stream, as the provider streams it, and reports whether
// ev was such a block's: a Responses reasoning item, an opaque block the
// frontend drops, and a Messages redacted thinking block, which it
// refuses.
func native(w io.Writer, d ir.Dialect, resp ir.Response, ev ir.Event) (bool, error) {
	if (ev.Type != ir.EventBlockStart && ev.Type != ir.EventBlockStop) || ev.Index >= len(resp.Blocks) {
		return false, nil
	}
	b := resp.Blocks[ev.Index]
	switch {
	case d == ir.DialectOpenAIResponses && b.Opaque != nil && b.Opaque.Dialect == d && b.Opaque.Kind == "reasoning":
		if ev.Type == ir.EventBlockStart {
			return true, writeReasoning(w, ev.Index, b.Opaque.Raw)
		}
		return true, nil
	case d == ir.DialectAnthropicMessages && b.Type == ir.BlockRedactedThinking:
		// The frontend drops opaque blocks from the wire's indexes, so
		// the block's index counts none of them.
		index := ev.Index
		for _, before := range resp.Blocks[:ev.Index] {
			if before.Type == ir.BlockOpaque {
				index--
			}
		}
		frame := map[string]any{"type": "content_block_stop", "index": index}
		if ev.Type == ir.EventBlockStart {
			frame = map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "redacted_thinking", "data": b.Redacted}}
		}
		return true, writeFrame(w, frame)
	}
	return false, nil
}

// writeFrame writes one SSE frame named by its type member.
func writeFrame(w io.Writer, f map[string]any) error {
	b, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("luxstub: encode a %s frame: %w", f["type"], err)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f["type"], b)
	return err
}

// writeReasoning writes a Responses reasoning item as the provider
// streams one: the item added with an empty summary, each summary part
// as a delta, and the item done as raw holds it, its encrypted content
// included.
func writeReasoning(w io.Writer, index int, raw json.RawMessage) error {
	var item struct {
		ID      string `json:"id"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return fmt.Errorf("luxstub: the reasoning item: %w", err)
	}
	frames := []map[string]any{{"type": "response.output_item.added", "output_index": index, "item": map[string]any{"type": "reasoning", "id": item.ID, "summary": []any{}}}}
	for i, part := range item.Summary {
		frames = append(frames, map[string]any{"type": "response.reasoning_summary_text.delta", "item_id": item.ID, "output_index": index, "summary_index": i, "delta": part.Text})
	}
	frames = append(frames, map[string]any{"type": "response.output_item.done", "output_index": index, "item": raw})
	for _, f := range frames {
		if err := writeFrame(w, f); err != nil {
			return err
		}
	}
	return nil
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
		case ir.BlockOpaque:
			head.Opaque = b.Opaque
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
