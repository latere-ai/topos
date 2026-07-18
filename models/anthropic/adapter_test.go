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

// ---------------------------------------------------------------------------
// Canned SSE bodies
// ---------------------------------------------------------------------------

// cannedSSE returns a multi-event SSE body that exercises the full normalized
// event sequence: text → tool_use → usage → done.
//
// Events emitted:
//  1. message_start with input usage
//  2. content_block_start (text)
//  3. content_block_delta (text_delta "Hello ")
//  4. content_block_delta (text_delta "world")
//  5. content_block_stop (text)
//  6. content_block_start (tool_use, index=1)
//  7. content_block_delta (input_json_delta fragment 1)
//  8. content_block_delta (input_json_delta fragment 2)
//  9. content_block_stop (tool_use, index=1)
//
// 10. message_delta (stop_reason=tool_use, output usage)
// 11. message_stop
func cannedSSE(stopReason string) string {
	if stopReason == "" {
		stopReason = "tool_use"
	}
	return fmt.Sprintf(`event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-opus-4-8","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":0,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_abc","name":"get_weather","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"London\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":17,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}

`, stopReason)
}

// cannedThinkingSSE returns an SSE body with a thinking block to test
// ProviderEvent passthrough.
const cannedThinkingSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_think","type":"message","role":"assistant","content":[],"model":"claude-opus-4-8","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me think..."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"42"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}

`

// cannedUsageSSE has a message_start carrying a non-zero initial output_tokens
// (3) and a message_delta carrying the cumulative final output_tokens (17), as
// a live Anthropic response does. Consumers accumulate KindUsage events, so the
// correct total is the cumulative delta value (17), not the sum (20).
const cannedUsageSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_u","type":"message","role":"assistant","content":[],"model":"claude-opus-4-8","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":3,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":17,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}

event: message_stop
data: {"type":"message_stop"}

`

// TestUsageDoesNotDoubleCountOutputTokens confirms message_start's initial
// output_tokens is not summed with message_delta's cumulative output_tokens.
func TestUsageDoesNotDoubleCountOutputTokens(t *testing.T) {
	ts, _ := fakeServer(t, cannedUsageSSE)
	a := anthropic.New("test-key", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var totalIn, totalOut, totalCacheR, totalCacheW int
	for _, ev := range drainStream(t, stream) {
		if ev.Kind == models.KindUsage {
			totalIn += ev.Usage.InputTokens
			totalOut += ev.Usage.OutputTokens
			totalCacheR += ev.Usage.CacheReadTokens
			totalCacheW += ev.Usage.CacheWriteTokens
		}
	}
	// Accumulated output must equal the cumulative message_delta value (17), not
	// message_start(3) + message_delta(17) = 20.
	if totalOut != 17 {
		t.Errorf("accumulated OutputTokens = %d, want 17 (cumulative delta, not double-counted)", totalOut)
	}
	// Input/cache come authoritatively from message_start and must be unaffected.
	if totalIn != 42 {
		t.Errorf("accumulated InputTokens = %d, want 42", totalIn)
	}
	if totalCacheR != 10 || totalCacheW != 5 {
		t.Errorf("accumulated cache tokens = (%d,%d), want (10,5)", totalCacheR, totalCacheW)
	}
}

// ---------------------------------------------------------------------------
// Helper: start a fake Anthropic SSE server
// ---------------------------------------------------------------------------

// capturedRequest holds the captured request and body from a fake server.
type capturedRequest struct {
	req  *http.Request
	body []byte
}

func fakeServer(t *testing.T, body string) (*httptest.Server, func() *capturedRequest) {
	t.Helper()
	ch := make(chan *capturedRequest, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(reqBody)))
		select {
		case ch <- &capturedRequest{req: r, body: reqBody}:
		default:
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))

	t.Cleanup(ts.Close)

	return ts, func() *capturedRequest {
		select {
		case c := <-ch:
			return c
		default:
			return nil
		}
	}
}

// drainStream reads all events from a stream and returns them.
func drainStream(t *testing.T, s models.Stream) []models.Event {
	t.Helper()
	defer s.Close() //nolint:errcheck
	var events []models.Event
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv error: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

// ---------------------------------------------------------------------------
// AC 1: normalized event sequence (text → tool_use → usage → done)
// ---------------------------------------------------------------------------

func TestNormalizedEventSequence(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("tool_use"))
	_ = getReq

	a := anthropic.New("test-key", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		System:    "You are a helpful assistant.",
		Messages:  []models.Message{{Role: "user", Content: "What is the weather?"}},
		MaxTokens: 1024,
		Tools: []models.ToolDef{
			{
				Name:        "get_weather",
				Description: "Get the weather for a city",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)

	// Categorise events by kind.
	var textDeltas []string
	var toolCallDones []*models.ToolCall
	var toolCallDeltas []string
	var usages []*models.Usage
	var doneEvents []models.Event
	var providerEvents []*models.ProviderEvent

	for _, ev := range events {
		switch ev.Kind {
		case models.KindTextDelta:
			textDeltas = append(textDeltas, ev.TextDelta)
		case models.KindToolCallDelta:
			toolCallDeltas = append(toolCallDeltas, ev.ToolCallDelta)
		case models.KindToolCallDone:
			toolCallDones = append(toolCallDones, ev.ToolCall)
		case models.KindUsage:
			usages = append(usages, ev.Usage)
		case models.KindDone:
			doneEvents = append(doneEvents, ev)
		case models.KindProviderEvent:
			providerEvents = append(providerEvents, ev.ProviderEvent)
		}
	}

	// Text deltas: "Hello " + "world".
	if got := strings.Join(textDeltas, ""); got != "Hello world" {
		t.Errorf("text content = %q, want %q", got, "Hello world")
	}

	// Tool call deltas: two fragments.
	if len(toolCallDeltas) != 2 {
		t.Errorf("tool call deltas = %d, want 2", len(toolCallDeltas))
	}

	// Tool call done: one call with correct ID, name, and assembled input.
	if len(toolCallDones) != 1 {
		t.Fatalf("tool call dones = %d, want 1", len(toolCallDones))
	}
	tc := toolCallDones[0]
	if tc.ID != "toolu_abc" {
		t.Errorf("ToolCall.ID = %q, want %q", tc.ID, "toolu_abc")
	}
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, "get_weather")
	}

	// Input must be valid JSON.
	var inputMap map[string]any
	if err := json.Unmarshal(tc.Input, &inputMap); err != nil {
		t.Errorf("ToolCall.Input is not valid JSON: %v (raw: %s)", err, tc.Input)
	}
	if inputMap["city"] != "London" {
		t.Errorf("ToolCall.Input[city] = %v, want %q", inputMap["city"], "London")
	}

	// At least one Usage event.
	if len(usages) == 0 {
		t.Error("no Usage events received")
	}
	// First usage: from message_start.
	u0 := usages[0]
	if u0.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42", u0.InputTokens)
	}
	if u0.CacheReadTokens != 10 {
		t.Errorf("Usage.CacheReadTokens = %d, want 10", u0.CacheReadTokens)
	}
	if u0.CacheWriteTokens != 5 {
		t.Errorf("Usage.CacheWriteTokens = %d, want 5", u0.CacheWriteTokens)
	}

	// Exactly one Done event.
	if len(doneEvents) != 1 {
		t.Fatalf("done events = %d, want 1", len(doneEvents))
	}
	if doneEvents[0].StopReason != models.StopToolUse {
		t.Errorf("StopReason = %q, want %q", doneEvents[0].StopReason, models.StopToolUse)
	}

	// At least one ProviderEvent (content_block_start for tool_use).
	if len(providerEvents) == 0 {
		t.Error("expected at least one ProviderEvent (tool_use block start)")
	}
}

// ---------------------------------------------------------------------------
// AC 2: stop reason mapping
// ---------------------------------------------------------------------------

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		anthropicReason string
		want            models.StopReason
	}{
		{"end_turn", models.StopEndTurn},
		{"tool_use", models.StopToolUse},
		{"max_tokens", models.StopMaxTokens},
		{"stop_sequence", models.StopSequence},
	}

	for _, tc := range cases {
		t.Run(tc.anthropicReason, func(t *testing.T) {
			ts, _ := fakeServer(t, cannedSSE(tc.anthropicReason))
			a := anthropic.New("test-key", ts.URL)
			stream, err := a.Stream(context.Background(), models.Request{
				Messages:  []models.Message{{Role: "user", Content: "hi"}},
				MaxTokens: 256,
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			events := drainStream(t, stream)
			var got models.StopReason
			for _, ev := range events {
				if ev.Kind == models.KindDone {
					got = ev.StopReason
				}
			}
			if got != tc.want {
				t.Errorf("StopReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC 3: tool round-trip (ToolDef in → ToolCall out, Input intact)
// ---------------------------------------------------------------------------

func TestToolRoundTrip(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)

	ts, getReq := fakeServer(t, cannedSSE("tool_use"))
	a := anthropic.New("test-key", ts.URL)

	req := models.Request{
		Messages:  []models.Message{{Role: "user", Content: "weather?"}},
		MaxTokens: 256,
		Tools: []models.ToolDef{
			{
				Name:        "get_weather",
				Description: "Get weather",
				InputSchema: inputSchema,
			},
		},
	}

	stream, err := a.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)

	// Verify wire request: tool schema must be passed through intact.
	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}
	var wireReq struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}
	if len(wireReq.Tools) != 1 {
		t.Fatalf("wire tools = %d, want 1", len(wireReq.Tools))
	}
	if wireReq.Tools[0].Name != "get_weather" {
		t.Errorf("wire tool name = %q, want %q", wireReq.Tools[0].Name, "get_weather")
	}
	// InputSchema must be passed through verbatim (modulo JSON encoding).
	var wantSchema, gotSchema any
	_ = json.Unmarshal(inputSchema, &wantSchema)
	_ = json.Unmarshal(wireReq.Tools[0].InputSchema, &gotSchema)
	wantJSON, _ := json.Marshal(wantSchema)
	gotJSON, _ := json.Marshal(gotSchema)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("input_schema mismatch: got %s, want %s", gotJSON, wantJSON)
	}

	// Verify the assembled ToolCall.Input from the stream.
	for _, ev := range events {
		if ev.Kind == models.KindToolCallDone {
			var got map[string]any
			if err := json.Unmarshal(ev.ToolCall.Input, &got); err != nil {
				t.Fatalf("ToolCall.Input not valid JSON: %v", err)
			}
			if got["city"] != "London" {
				t.Errorf("ToolCall.Input[city] = %v, want London", got["city"])
			}
			return
		}
	}
	t.Error("no KindToolCallDone event received")
}

// ---------------------------------------------------------------------------
// AC 4: ProviderEvent passthrough (thinking delta)
// ---------------------------------------------------------------------------

func TestProviderEventPassthrough(t *testing.T) {
	ts, _ := fakeServer(t, cannedThinkingSSE)
	a := anthropic.New("test-key", ts.URL)

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:       []models.Message{{Role: "user", Content: "think hard"}},
		MaxTokens:      1024,
		ThinkingBudget: 500,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)

	// Must have at least one ProviderEvent with Type containing "thinking".
	var foundThinking bool
	for _, ev := range events {
		if ev.Kind == models.KindProviderEvent {
			if strings.Contains(ev.ProviderEvent.Type, "thinking") {
				foundThinking = true
				// Must have Raw JSON.
				if len(ev.ProviderEvent.Raw) == 0 {
					t.Error("ProviderEvent.Raw is empty")
				}
				// Must not appear as any decision-bearing kind.
				if ev.Kind == models.KindDone || ev.Kind == models.KindUsage ||
					ev.Kind == models.KindTextDelta || ev.Kind == models.KindToolCallDone {
					t.Errorf("thinking event appeared as decision-bearing kind %q", ev.Kind)
				}
			}
		}
	}
	if !foundThinking {
		t.Error("no thinking ProviderEvent found")
	}

	// There should still be a text delta ("42") and a Done event.
	var hasText, hasDone bool
	for _, ev := range events {
		if ev.Kind == models.KindTextDelta && ev.TextDelta == "42" {
			hasText = true
		}
		if ev.Kind == models.KindDone {
			hasDone = true
		}
	}
	if !hasText {
		t.Error("expected text delta '42' not found")
	}
	if !hasDone {
		t.Error("expected Done event not found")
	}
}

// ---------------------------------------------------------------------------
// AC 5: Authorization header + anthropic-version + content-type assertions
// ---------------------------------------------------------------------------

func TestRequestHeaders(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("my-secret-key", ts.URL)

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hello"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}

	if got := captured.req.Header.Get("x-api-key"); got != "my-secret-key" {
		t.Errorf("x-api-key = %q, want %q", got, "my-secret-key")
	}
	if got := captured.req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
	}
	if got := captured.req.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := captured.req.Method; got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}
	if got := captured.req.URL.Path; got != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", got)
	}
}

// TestOAuthTokenHeaders verifies that WithOAuthToken sends the credential as a
// Bearer token (not x-api-key) and enables the oauth-2025-04-20 beta, which the
// Anthropic API requires for OAuth access tokens (sk-ant-oat...).
func TestOAuthTokenHeaders(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("sk-ant-oat01-secret", ts.URL, anthropic.WithOAuthToken())

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hello"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}

	if got := captured.req.Header.Get("Authorization"); got != "Bearer sk-ant-oat01-secret" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-ant-oat01-secret")
	}
	if got := captured.req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q, want empty (OAuth uses Bearer)", got)
	}
	if got := captured.req.Header.Get("anthropic-beta"); !strings.Contains(got, "oauth-2025-04-20") {
		t.Errorf("anthropic-beta = %q, want it to contain oauth-2025-04-20", got)
	}
}

// TestBearerSourcePerCallNoBeta verifies WithBearerSource sends the token as a
// Bearer (not x-api-key), calls the source once per request so a rotated token
// is picked up, and — unlike WithOAuthToken — adds NO anthropic-beta header
// (the token terminates at Lux, not Anthropic).
func TestBearerSourcePerCallNoBeta(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))

	tokens := []string{"tok-1", "tok-2"}
	var calls int
	a := anthropic.New("", ts.URL, anthropic.WithBearerSource(func(context.Context) (string, error) {
		tok := tokens[calls]
		calls++
		return tok, nil
	}))

	for i, want := range []string{"Bearer tok-1", "Bearer tok-2"} {
		stream, err := a.Stream(context.Background(), models.Request{
			Messages:  []models.Message{{Role: "user", Content: "hi"}},
			MaxTokens: 64,
		})
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		drainStream(t, stream)

		captured := getReq()
		if captured == nil {
			t.Fatalf("request %d: none captured", i)
		}
		if got := captured.req.Header.Get("Authorization"); got != want {
			t.Errorf("request %d: Authorization = %q, want %q (rotated per call)", i, got, want)
		}
		if got := captured.req.Header.Get("x-api-key"); got != "" {
			t.Errorf("request %d: x-api-key = %q, want empty", i, got)
		}
		if got := captured.req.Header.Get("anthropic-beta"); strings.Contains(got, "oauth") {
			t.Errorf("request %d: anthropic-beta = %q, must NOT carry the oauth beta", i, got)
		}
	}
	if calls != 2 {
		t.Errorf("bearer source called %d times, want 2 (once per request)", calls)
	}
}

// TestBearerSourceErrorAborts verifies a bearer-source error surfaces as a
// Stream error rather than sending a request with no/garbage credential.
func TestBearerSourceErrorAborts(t *testing.T) {
	ts, _ := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("", ts.URL, anthropic.WithBearerSource(func(context.Context) (string, error) {
		return "", errBearer
	}))
	_, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err == nil {
		t.Fatal("expected Stream to fail when the bearer source errors")
	}
}

var errBearer = fmt.Errorf("token unavailable")

// TestOAuthTokenBetasMergeWithThinking verifies the OAuth beta and the
// per-request thinking beta are both present (comma-joined) when a thinking
// budget is requested, and that the adapter's stored betas are not mutated
// across requests.
func TestOAuthTokenBetasMergeWithThinking(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("sk-ant-oat01-secret", ts.URL, anthropic.WithOAuthToken())

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:       []models.Message{{Role: "user", Content: "hello"}},
		MaxTokens:      64,
		ThinkingBudget: 1024,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}
	got := captured.req.Header.Get("anthropic-beta")
	if !strings.Contains(got, "oauth-2025-04-20") || !strings.Contains(got, "interleaved-thinking-2025-05-14") {
		t.Errorf("anthropic-beta = %q, want both oauth and thinking betas", got)
	}

	// A second request without a thinking budget must carry only the OAuth beta,
	// proving the thinking beta was not appended to the adapter's stored slice.
	ts2, getReq2 := fakeServer(t, cannedSSE("end_turn"))
	a2 := anthropic.New("sk-ant-oat01-secret", ts2.URL, anthropic.WithOAuthToken())
	// Warm-up request with thinking, then a plain one on the same adapter.
	s1, err := a2.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 64, ThinkingBudget: 1024,
	})
	if err != nil {
		t.Fatalf("Stream warm-up: %v", err)
	}
	drainStream(t, s1)
	_ = getReq2()
	s2, err := a2.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi again"}}, MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream plain: %v", err)
	}
	drainStream(t, s2)
	got2 := getReq2().req.Header.Get("anthropic-beta")
	if strings.Contains(got2, "interleaved-thinking") {
		t.Errorf("second request anthropic-beta = %q, thinking beta leaked across requests", got2)
	}
}

// ---------------------------------------------------------------------------
// AC 6: Prompt caching — cache_control on system block
// ---------------------------------------------------------------------------

func TestPromptCaching(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("test-key", ts.URL)

	stream, err := a.Stream(context.Background(), models.Request{
		System:    "You are a helpful assistant.",
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}

	var wireReq struct {
		System []struct {
			Type         string `json:"type"`
			Text         string `json:"text"`
			CacheControl *struct {
				Type string `json:"type"`
			} `json:"cache_control"`
		} `json:"system"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}
	if len(wireReq.System) == 0 {
		t.Fatal("system block is absent")
	}
	sys := wireReq.System[0]
	if sys.CacheControl == nil {
		t.Fatal("system block has no cache_control")
	}
	if sys.CacheControl.Type != "ephemeral" {
		t.Errorf("system cache_control.type = %q, want ephemeral", sys.CacheControl.Type)
	}
}

// ---------------------------------------------------------------------------
// AC 7: tool message down-conversion (tool role → user with tool_result)
// ---------------------------------------------------------------------------

func TestToolMessageDownConversion(t *testing.T) {
	ts, getReq := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("test-key", ts.URL)

	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "What is the weather?"},
			{Role: "assistant", ToolCalls: []models.ToolCall{
				{ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
			}},
			{Role: "tool", ToolResults: []models.ToolResult{
				{CallID: "toolu_1", Content: "Sunny, 25°C", IsError: false},
			}},
		},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	captured := getReq()
	if captured == nil {
		t.Fatal("no request captured")
	}

	var wireReq struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
				Content   string `json:"content"`
				IsError   bool   `json:"is_error"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}

	// The "tool" message must be down-converted to a "user" message with
	// a "tool_result" content block.
	var foundToolResult bool
	for _, wm := range wireReq.Messages {
		if wm.Role != "user" {
			continue
		}
		for _, wc := range wm.Content {
			if wc.Type == "tool_result" {
				foundToolResult = true
				if wc.ToolUseID != "toolu_1" {
					t.Errorf("tool_result tool_use_id = %q, want toolu_1", wc.ToolUseID)
				}
				if wc.Content != "Sunny, 25°C" {
					t.Errorf("tool_result content = %q, want \"Sunny, 25°C\"", wc.Content)
				}
				if wc.IsError {
					t.Error("tool_result is_error should be false")
				}
			}
		}
	}
	if !foundToolResult {
		t.Error("tool_result content block not found in wire messages")
	}
}

// ---------------------------------------------------------------------------
// AC 8: Close mid-stream cancels the request
// ---------------------------------------------------------------------------

func TestCloseMidStream(t *testing.T) {
	// Server that streams slowly (never finishes — sends header + one event).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Flush one event then block until the client disconnects.
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"x\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":0}}}\n\n")
		flusher.Flush()
		// Block until request context is cancelled.
		<-r.Context().Done()
	}))
	defer ts.Close()

	a := anthropic.New("test-key", ts.URL)
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Receive the first event (message_start → KindUsage).
	ev, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if ev.Kind != models.KindUsage {
		t.Errorf("first event kind = %q, want KindUsage", ev.Kind)
	}

	// Close mid-stream: must not panic and must return nil or a closed-pipe error.
	if err := stream.Close(); err != nil {
		// A "use of closed network connection" or similar is acceptable.
		t.Logf("Close returned (acceptable): %v", err)
	}

	// After Close, Recv should return an error (not hang).
	_, recvErr := stream.Recv()
	if recvErr == nil {
		t.Error("expected error after Close, got nil")
	}
}

// ---------------------------------------------------------------------------
// AC 9: After KindDone, next Recv returns io.EOF
// ---------------------------------------------------------------------------

func TestRecvAfterDoneReturnsEOF(t *testing.T) {
	ts, _ := fakeServer(t, cannedSSE("end_turn"))
	a := anthropic.New("test-key", ts.URL)

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close() //nolint:errcheck

	var sawDone bool
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if !sawDone {
				t.Error("got io.EOF without ever seeing KindDone")
			}
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Kind == models.KindDone {
			// Next call must be io.EOF.
			_, nextErr := stream.Recv()
			if !errors.Is(nextErr, io.EOF) {
				t.Errorf("after KindDone, Recv returned %v, want io.EOF", nextErr)
			}
			break
		}
	}
}

// ---------------------------------------------------------------------------
// AC 10: HTTP error response surfaces as error (not panic)
// ---------------------------------------------------------------------------

func TestHTTPErrorResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"type":"authentication_error","message":"invalid api key"}}`, http.StatusUnauthorized)
	}))
	defer ts.Close()

	a := anthropic.New("bad-key", ts.URL)
	_, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
	var apiErr *anthropic.APIError
	if !isAPIError(err, &apiErr) {
		t.Errorf("expected *anthropic.APIError, got %T: %v", err, err)
	} else if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusUnauthorized)
	}
}

// isAPIError checks if err is (or wraps) an *anthropic.APIError.
func isAPIError(err error, target **anthropic.APIError) bool {
	return errors.As(err, target)
}

// ---------------------------------------------------------------------------
// Stub package tests
// ---------------------------------------------------------------------------

// (These live in separate test files to keep package references clean;
//  the actual stub tests are in openai/adapter_test.go and gemini/adapter_test.go)
