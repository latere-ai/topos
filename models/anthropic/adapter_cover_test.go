// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/anthropic"
)

// drainUntilErr reads events until a non-EOF error (or EOF) is hit, returning
// the events delivered before the stop and the terminating error (nil on EOF).
func drainUntilErr(s models.Stream) (events []models.Event, err error) {
	defer s.Close() //nolint:errcheck
	for {
		ev, rerr := s.Recv()
		if errors.Is(rerr, io.EOF) {
			return events, nil
		}
		if rerr != nil {
			return events, rerr
		}
		events = append(events, ev)
	}
}

// TestModelOverride verifies WithModel overrides both the reported model id and
// the model field sent on the wire, while the default New uses the baked-in id.
func TestModelOverride(t *testing.T) {
	if got := anthropic.New("k", "").Model(); got != "claude-opus-4-7" {
		t.Errorf("default Model() = %q, want claude-opus-4-7", got)
	}

	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("k", ts.URL, anthropic.WithModel("claude-sonnet-4-5"))
	if got := a.Model(); got != "claude-sonnet-4-5" {
		t.Errorf("Model() = %q, want claude-sonnet-4-5", got)
	}
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wireReq.Model != "claude-sonnet-4-5" {
		t.Errorf("wire model = %q, want claude-sonnet-4-5", wireReq.Model)
	}
}

// TestDefaultMaxTokens verifies buildRequest substitutes a safe default when the
// caller leaves MaxTokens at zero.
func TestDefaultMaxTokens(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wireReq.MaxTokens != 8192 {
		t.Errorf("wire max_tokens = %d, want 8192 (default)", wireReq.MaxTokens)
	}
}

// TestWithHTTPClientTransportError verifies WithHTTPClient injects the client
// and that a transport-level failure surfaces as a Stream error (not a panic).
func TestWithHTTPClientTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial tcp: simulated network failure")
	})}
	a := anthropic.New("k", "http://example.invalid", anthropic.WithHTTPClient(client))
	_, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
	if !strings.Contains(err.Error(), "anthropic: http") {
		t.Errorf("error = %v, want it to wrap 'anthropic: http'", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestAPIErrorString verifies the APIError message format.
func TestAPIErrorString(t *testing.T) {
	e := &anthropic.APIError{Status: 503, Body: "overloaded"}
	if got := e.Error(); got != "anthropic: API error 503: overloaded" {
		t.Errorf("Error() = %q", got)
	}
}

// TestNilToolSchemaAndTemperature verifies buildRequest fills a default schema
// for a tool with no InputSchema and forwards a non-zero temperature.
func TestNilToolSchemaAndTemperature(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:    []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens:   16,
		Temperature: 0.7,
		Tools:       []models.ToolDef{{Name: "noop", Description: "does nothing"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Temperature *float64 `json:"temperature"`
		Tools       []struct {
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wireReq.Temperature == nil || *wireReq.Temperature != 0.7 {
		t.Errorf("wire temperature = %v, want 0.7", wireReq.Temperature)
	}
	if len(wireReq.Tools) != 1 {
		t.Fatalf("wire tools = %d, want 1", len(wireReq.Tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(wireReq.Tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("default schema not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("default schema type = %v, want object", schema["type"])
	}
}

// TestAssistantToolCallAndEmptyContent verifies wire conversion of an assistant
// message carrying a tool call with nil input (→ "{}") and an assistant message
// with neither text nor tool calls (→ a single empty text block).
func TestAssistantToolCallAndEmptyContent(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "t1", Name: "noop"}}},
			{Role: "tool", ToolResults: []models.ToolResult{{CallID: "t1", Content: "ok"}}},
			{Role: "assistant"}, // no content, no tool calls → empty text block
		},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}

	var sawToolUse, sawEmptyText bool
	for _, m := range wireReq.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.Content {
			if c.Type == "tool_use" && string(c.Input) == "{}" {
				sawToolUse = true
			}
		}
		if len(m.Content) == 1 && m.Content[0].Type == "text" && m.Content[0].Text == "" {
			sawEmptyText = true
		}
	}
	if !sawToolUse {
		t.Error("assistant tool_use with default {} input not found")
	}
	if !sawEmptyText {
		t.Error("empty assistant message did not produce a single empty text block")
	}
}

// TestBuildRequestUnknownRole verifies an unknown message role aborts Stream at
// the request-build stage with a wrapped error.
func TestBuildRequestUnknownRole(t *testing.T) {
	a := anthropic.New("k", "http://example.invalid")
	_, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "wizard", Content: "abracadabra"}}, MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected build error for unknown role")
	}
	if !strings.Contains(err.Error(), "build request") || !strings.Contains(err.Error(), "wizard") {
		t.Errorf("error = %v, want it to mention build request and the role", err)
	}
}

// TestToolMessageNoResults verifies a "tool" message with no ToolResults is a
// build error.
func TestToolMessageNoResults(t *testing.T) {
	a := anthropic.New("k", "http://example.invalid")
	_, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "tool"}}, MaxTokens: 16,
	})
	if err == nil || !strings.Contains(err.Error(), "no ToolResults") {
		t.Errorf("error = %v, want a 'no ToolResults' build error", err)
	}
}

// TestUnknownStopReasonPassthrough verifies an unrecognized stop_reason is
// passed through verbatim.
func TestUnknownStopReasonPassthrough(t *testing.T) {
	ts, _ := fakeServer(t, cannedSSE("pause_turn"))
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var got models.StopReason
	for _, ev := range drainStream(t, stream) {
		if ev.Kind == models.KindDone {
			got = ev.StopReason
		}
	}
	if got != models.StopReason("pause_turn") {
		t.Errorf("StopReason = %q, want pause_turn (passthrough)", got)
	}
}

// TestProviderEventPassthroughVariants verifies signature_delta, an unknown
// content_block_delta type, and an unknown top-level event are all surfaced as
// ProviderEvents (never as decision-bearing kinds), and the stream still
// completes normally.
func TestProviderEventPassthroughVariants(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"abc"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"redaction_delta","foo":"bar"}}

event: cobblestone
data: {"type":"cobblestone","note":"unknown top-level event"}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}

`
	ts, _ := fakeServer(t, body)
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	types := map[string]bool{}
	var sawDone bool
	for _, ev := range drainStream(t, stream) {
		if ev.Kind == models.KindProviderEvent {
			types[ev.ProviderEvent.Type] = true
		}
		if ev.Kind == models.KindDone {
			sawDone = true
		}
	}
	for _, want := range []string{"signature_delta", "content_block_delta.redaction_delta", "cobblestone"} {
		if !types[want] {
			t.Errorf("missing ProviderEvent type %q (got %v)", want, types)
		}
	}
	if !sawDone {
		t.Error("stream with provider events never reached Done")
	}
}

// TestEOFBeforeDone verifies a stream truncated before message_stop delivers the
// events it did send and then returns io.EOF without ever emitting KindDone.
func TestEOFBeforeDone(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}

`
	ts, _ := fakeServer(t, body)
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events, derr := drainUntilErr(stream)
	if derr != nil {
		t.Fatalf("expected clean io.EOF, got %v", derr)
	}
	var text string
	for _, ev := range events {
		if ev.Kind == models.KindDone {
			t.Error("KindDone emitted on a truncated stream")
		}
		if ev.Kind == models.KindTextDelta {
			text += ev.TextDelta
		}
	}
	if text != "partial" {
		t.Errorf("pre-truncation text = %q, want %q", text, "partial")
	}
}

// TestScannerErrTooLong verifies a single SSE data line larger than the 1 MiB
// scanner buffer surfaces as a wrapped read error after earlier events were
// delivered intact.
func TestScannerErrTooLong(t *testing.T) {
	huge := strings.Repeat("x", 2*1024*1024)
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + huge + `"}}` + "\n\n"

	ts, _ := fakeServer(t, body)
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events, derr := drainUntilErr(stream)
	if derr == nil {
		t.Fatal("expected a read error on the oversized line")
	}
	if !strings.Contains(derr.Error(), "stream read") {
		t.Errorf("error = %v, want it to wrap 'stream read'", derr)
	}
	// The earlier message_start usage must still have been delivered.
	if len(events) == 0 || events[0].Kind != models.KindUsage {
		t.Errorf("pre-error events = %v, want a leading Usage event", events)
	}
}

// TestMalformedEventJSON verifies that a malformed JSON payload on each known
// SSE event type surfaces as a Recv error (no panic), while a valid leading
// event is still delivered first.
func TestMalformedEventJSON(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
	}{
		{"message_start", "message_start"},
		{"content_block_start", "content_block_start"},
		{"content_block_delta", "content_block_delta"},
		{"content_block_stop", "content_block_stop"},
		{"message_delta", "message_delta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A valid message_start precedes the malformed event so we can assert
			// pre-error delivery — except when message_start itself is the target.
			var sb strings.Builder
			expectPre := tc.eventType != "message_start"
			if expectPre {
				sb.WriteString("event: message_start\n")
				sb.WriteString(`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}` + "\n\n")
			}
			sb.WriteString("event: " + tc.eventType + "\n")
			sb.WriteString("data: {not valid json}\n\n")

			ts, _ := fakeServer(t, sb.String())
			a := anthropic.New("k", ts.URL)
			stream, err := a.Stream(context.Background(), models.Request{
				Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			events, derr := drainUntilErr(stream)
			if derr == nil {
				t.Fatal("expected a parse error, got clean EOF")
			}
			if !strings.Contains(derr.Error(), "parse "+tc.eventType) {
				t.Errorf("error = %v, want it to mention 'parse %s'", derr, tc.eventType)
			}
			if expectPre {
				if len(events) != 1 || events[0].Kind != models.KindUsage {
					t.Errorf("pre-error events = %v, want exactly one Usage event", events)
				}
			}
		})
	}
}

// TestStreamErrorEvent verifies an SSE "error" event aborts Recv with an error.
func TestStreamErrorEvent(t *testing.T) {
	body := `event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}

`
	ts, _ := fakeServer(t, body)
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, derr := drainUntilErr(stream)
	if derr == nil || !strings.Contains(derr.Error(), "stream error event") {
		t.Errorf("error = %v, want a 'stream error event' error", derr)
	}
}

// TestContextCancelMidStream verifies cancelling the request context mid-stream
// causes the next Recv to fail rather than hang or return a clean Done.
func TestContextCancelMidStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}`+"\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := anthropic.New("k", ts.URL)
	stream, err := a.Stream(ctx, models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if ev.Kind != models.KindUsage {
		t.Errorf("first event = %q, want KindUsage", ev.Kind)
	}

	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Error("expected Recv to fail after context cancel, got nil")
	}
}
