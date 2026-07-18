// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package lux

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/topos/models"
)

// sse builds one lux SSE frame.
func sse(name, data string) string {
	return "event: " + name + "\ndata: " + data + "\n\n"
}

func fullStream() string {
	return sse("message_start", `{"type":"message_start","id":"msg_1","model":"m","index":0,"usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":3,"cache_write_input_tokens":4}}`) +
		sse("block_start", `{"type":"block_start","index":0,"block":{"type":"text"}}`) +
		sse("text_delta", `{"type":"text_delta","index":0,"delta":"Let me "}`) +
		sse("text_delta", `{"type":"text_delta","index":0,"delta":"check."}`) +
		sse("block_stop", `{"type":"block_stop","index":0}`) +
		sse("block_start", `{"type":"block_start","index":1,"block":{"type":"thinking"}}`) +
		sse("thinking_delta", `{"type":"thinking_delta","index":1,"delta":"hmm"}`) +
		sse("block_stop", `{"type":"block_stop","index":1}`) +
		sse("block_start", `{"type":"block_start","index":2,"block":{"type":"tool_use","tool_use":{"id":"tu_1","name":"bash"}}}`) +
		sse("args_delta", `{"type":"args_delta","index":2,"delta":"{\"cmd\":"}`) +
		sse("args_delta", `{"type":"args_delta","index":2,"delta":"\"ls\"}"}`) +
		sse("block_stop", `{"type":"block_stop","index":2}`) +
		sse("message_delta", `{"type":"message_delta","index":0,"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":30}}`) +
		sse("message_stop", `{"type":"message_stop","index":0}`)
}

func streamServer(t *testing.T, body string, capture *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, capture)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func recvAll(t *testing.T, st models.Stream) []models.Event {
	t.Helper()
	var out []models.Event
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestStreamEventMapping(t *testing.T) {
	var captured map[string]any
	srv := streamServer(t, fullStream(), &captured)
	a := New("lux_k", srv.URL, WithModel("my-model"))
	if a.Model() != "my-model" {
		t.Fatalf("Model() = %q", a.Model())
	}

	st, err := a.Stream(context.Background(), models.Request{
		System:         "be brief",
		MaxTokens:      256,
		Temperature:    0.5,
		ThinkingBudget: 1024,
		Tools:          []models.ToolDef{{Name: "bash", Description: "run"}},
		Messages: []models.Message{
			{Role: "user", Content: "list files"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	events := recvAll(t, st)

	// Request wire shape.
	if captured["model"] != "my-model" || captured["max_tokens"].(float64) != 256 {
		t.Fatalf("bad wire request: %v", captured)
	}
	if captured["temperature"].(float64) != 0.5 {
		t.Fatalf("bad temperature: %v", captured["temperature"])
	}
	if captured["reasoning"].(map[string]any)["budget_tokens"].(float64) != 1024 {
		t.Fatalf("bad reasoning: %v", captured["reasoning"])
	}
	sys := captured["system"].([]any)[0].(map[string]any)
	if sys["text"] != "be brief" || sys["cache_hint"] != true {
		t.Fatalf("bad system: %v", sys)
	}
	// Nil tool schema gets the empty-object default.
	tool := captured["tools"].([]any)[0].(map[string]any)
	if tool["input_schema"].(map[string]any)["type"] != "object" {
		t.Fatalf("bad tool schema: %v", tool)
	}
	if captured["stream"] != true {
		t.Fatalf("stream not forced on: %v", captured["stream"])
	}

	// Event mapping.
	var text, args strings.Builder
	var kinds []string
	var toolCall *models.ToolCall
	var usage models.Usage
	var stop models.StopReason
	for _, ev := range events {
		kinds = append(kinds, string(ev.Kind))
		switch ev.Kind {
		case models.KindTextDelta:
			text.WriteString(ev.TextDelta)
		case models.KindToolCallDelta:
			args.WriteString(ev.ToolCallDelta)
		case models.KindToolCallDone:
			toolCall = ev.ToolCall
		case models.KindUsage:
			usage.Add(*ev.Usage)
		case models.KindDone:
			stop = ev.StopReason
		}
	}
	if text.String() != "Let me check." {
		t.Fatalf("text = %q", text.String())
	}
	if toolCall == nil || toolCall.ID != "tu_1" || toolCall.Name != "bash" || string(toolCall.Input) != `{"cmd":"ls"}` {
		t.Fatalf("bad tool call: %#v", toolCall)
	}
	if args.String() != `{"cmd":"ls"}` {
		t.Fatalf("bad tool deltas: %q", args.String())
	}
	// message_start usage + message_delta usage accumulated.
	if usage.InputTokens != 24 || usage.OutputTokens != 30 || usage.CacheReadTokens != 3 || usage.CacheWriteTokens != 4 {
		t.Fatalf("bad usage: %#v", usage)
	}
	if stop != models.StopToolUse {
		t.Fatalf("stop = %q", stop)
	}
	// Thinking frames surface as provider events, not silently dropped.
	joined := strings.Join(kinds, " ")
	if !strings.Contains(joined, string(models.KindProviderEvent)) {
		t.Fatalf("no provider events in %s", joined)
	}
	// After EOF the stream stays EOF.
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF after done, got %v", err)
	}
}

func TestMessageMapping(t *testing.T) {
	var captured map[string]any
	srv := streamServer(t, fullStream(), &captured)
	a := New("lux_k", srv.URL)

	st, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "run it"},
			{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "tu_1", Name: "bash"}}},
			{Role: "tool", ToolResults: []models.ToolResult{{CallID: "tu_1", Content: "ok", IsError: true}}},
			{Role: "assistant"}, // empty assistant turn gets an empty text block
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	msgs := captured["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	tu := asst["blocks"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["tool_use"].(map[string]any)["args"].(map[string]any) == nil {
		t.Fatalf("bad assistant tool_use (nil args must default to {}): %v", tu)
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "user" {
		t.Fatalf("tool message must become a user turn: %v", toolMsg)
	}
	tr := toolMsg["blocks"].([]any)[0].(map[string]any)["tool_result"].(map[string]any)
	if tr["tool_use_id"] != "tu_1" || tr["is_error"] != true || tr["blocks"].([]any)[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("bad tool_result: %v", tr)
	}
	empty := msgs[3].(map[string]any)["blocks"].([]any)[0].(map[string]any)
	if empty["type"] != "text" {
		t.Fatalf("bad empty assistant turn: %v", empty)
	}
}

func TestMessageMappingErrors(t *testing.T) {
	a := New("k", "http://unused")
	for _, req := range []models.Request{
		{Messages: []models.Message{{Role: "wizard"}}},
		{Messages: []models.Message{{Role: "tool"}}}, // no ToolResults
	} {
		if _, err := a.Stream(context.Background(), req); err == nil {
			t.Fatalf("want error for %#v", req.Messages)
		}
	}
}

func TestStreamErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer srv.Close()
	_, err := New("k", srv.URL).Stream(context.Background(), models.Request{Messages: []models.Message{{Role: "user", Content: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "rate_limit_error") {
		t.Fatalf("want rate limit error, got %v", err)
	}
}

func TestMidStreamError(t *testing.T) {
	body := sse("message_start", `{"type":"message_start","id":"m1","model":"m","index":0}`) +
		sse("error", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)
	srv := streamServer(t, body, nil)
	st, err := New("k", srv.URL).Stream(context.Background(), models.Request{Messages: []models.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// message_start without usage surfaces as a provider event.
	ev, err := st.Recv()
	if err != nil || ev.Kind != models.KindProviderEvent {
		t.Fatalf("first event: %#v err=%v", ev, err)
	}
	if _, err := st.Recv(); err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Fatalf("want mid-stream error, got %v", err)
	}
}

func TestDefaults(t *testing.T) {
	var captured map[string]any
	srv := streamServer(t, fullStream(), &captured)
	a := New("", srv.URL, WithBearerSource(func(context.Context) (string, error) { return "jwt-1", nil }))
	if a.Model() != defaultModel {
		t.Fatalf("default model = %q", a.Model())
	}
	st, err := a.Stream(context.Background(), models.Request{Messages: []models.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if captured["max_tokens"].(float64) != 8192 {
		t.Fatalf("default max_tokens = %v", captured["max_tokens"])
	}
	if _, ok := captured["temperature"]; ok {
		t.Fatal("zero temperature must be omitted")
	}
	if _, ok := captured["reasoning"]; ok {
		t.Fatal("zero thinking budget must be omitted")
	}
}

func TestBearerSource(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fullStream())
	}))
	defer srv.Close()
	a := New("", srv.URL, WithBearerSource(func(context.Context) (string, error) { return "jwt-2", nil }))
	st, err := a.Stream(context.Background(), models.Request{Messages: []models.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	if gotAuth != "Bearer jwt-2" {
		t.Fatalf("auth = %q", gotAuth)
	}
}

func TestEmptyToolArgsDefault(t *testing.T) {
	body := sse("message_start", `{"type":"message_start","id":"m1","model":"m","index":0,"usage":{"input_tokens":1,"output_tokens":0}}`) +
		sse("block_start", `{"type":"block_start","index":0,"block":{"type":"tool_use","tool_use":{"id":"tu_9","name":"ping"}}}`) +
		sse("block_stop", `{"type":"block_stop","index":0}`) +
		sse("message_delta", `{"type":"message_delta","index":0,"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`) +
		sse("message_stop", `{"type":"message_stop","index":0}`)
	srv := streamServer(t, body, nil)
	st, err := New("k", srv.URL).Stream(context.Background(), models.Request{Messages: []models.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for _, ev := range recvAll(t, st) {
		if ev.Kind == models.KindToolCallDone {
			if string(ev.ToolCall.Input) != "{}" {
				t.Fatalf("empty args must default to {}: %q", ev.ToolCall.Input)
			}
			return
		}
	}
	t.Fatal("no ToolCallDone event")
}
