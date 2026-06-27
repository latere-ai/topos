// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package ollama_test

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
	"latere.ai/x/topos/models/ollama"
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

// TestDefaultModelOnWire verifies New("") defaults the model tag to "llama3.1"
// and that the default appears in the request body.
func TestDefaultModelOnWire(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wireReq.Model != "llama3.1" {
		t.Errorf("wire model = %q, want llama3.1 (default)", wireReq.Model)
	}
}

// TestCanceledContextAndDefaultHost verifies that with the default host
// (New("","")) a request issued under an already-canceled context fails at the
// HTTP layer with a wrapped error rather than hanging or panicking. This also
// exercises the empty-host and empty-model defaults deterministically, without
// depending on whether anything is listening on localhost:11434.
func TestCanceledContextAndDefaultHost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	a := ollama.New("", "")
	_, err := a.Stream(ctx, models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err == nil {
		t.Fatal("expected an error for a canceled context, got nil")
	}
	if !strings.Contains(err.Error(), "ollama: http") {
		t.Errorf("error = %v, want it to wrap 'ollama: http'", err)
	}
}

// TestAPIErrorString verifies the APIError message format.
func TestAPIErrorString(t *testing.T) {
	e := &ollama.APIError{Status: 500, Body: "boom"}
	if got := e.Error(); got != "ollama: API error 500: boom" {
		t.Errorf("Error() = %q", got)
	}
}

// TestNilToolSchemaAndOptions verifies buildRequest fills a default schema for a
// tool with no parameters and forwards temperature + num_predict options.
func TestNilToolSchemaAndOptions(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:    []models.Message{{Role: "user", Content: "hi"}},
		Temperature: 0.5,
		MaxTokens:   128,
		Tools:       []models.ToolDef{{Name: "noop", Description: "does nothing"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Options *struct {
			Temperature *float64 `json:"temperature"`
			NumPredict  *int     `json:"num_predict"`
		} `json:"options"`
		Tools []struct {
			Function struct {
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	if wireReq.Options == nil {
		t.Fatal("options block absent")
	}
	if wireReq.Options.Temperature == nil || *wireReq.Options.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", wireReq.Options.Temperature)
	}
	if wireReq.Options.NumPredict == nil || *wireReq.Options.NumPredict != 128 {
		t.Errorf("num_predict = %v, want 128", wireReq.Options.NumPredict)
	}
	if len(wireReq.Tools) != 1 {
		t.Fatalf("wire tools = %d, want 1", len(wireReq.Tools))
	}
	var schema map[string]any
	if err := json.Unmarshal(wireReq.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatalf("default schema not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("default schema type = %v, want object", schema["type"])
	}
}

// TestAssistantToolCallNilInput verifies an assistant history tool call with no
// input is encoded with empty-object arguments ("{}").
func TestAssistantToolCallNilInput(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []models.ToolCall{{ID: "c1", Name: "noop"}}},
		},
		MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)

	var wireReq struct {
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []struct {
				Function struct {
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(getReq().body, &wireReq); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	var found bool
	for _, m := range wireReq.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 {
			found = true
			if string(m.ToolCalls[0].Function.Arguments) != "{}" {
				t.Errorf("arguments = %s, want {}", m.ToolCalls[0].Function.Arguments)
			}
		}
	}
	if !found {
		t.Error("assistant tool call not found on wire")
	}
}

// TestBuildRequestErrors verifies unknown roles and result-less tool messages
// abort Stream at the build stage.
func TestBuildRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		msg  models.Message
		want string
	}{
		{"unknown role", models.Message{Role: "wizard", Content: "x"}, "unknown message role"},
		{"tool without results", models.Message{Role: "tool"}, "no ToolResults"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := ollama.New("http://example.invalid", "llama3.1")
			_, err := a.Stream(context.Background(), models.Request{
				Messages: []models.Message{tc.msg}, MaxTokens: 16,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestEmptyToolArgumentsDefault verifies a tool call delivered with no arguments
// field yields a non-empty, valid-JSON ToolCall.Input ("{}").
func TestEmptyToolArgumentsDefault(t *testing.T) {
	body := strings.Join([]string{
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"ping"}}]},"done":false}`,
		`{"message":null,"done":true,"done_reason":"stop","prompt_eval_count":1,"eval_count":1}`,
	}, "\n") + "\n"

	ts, _ := fakeServer(t, body)
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "ping"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var found bool
	for _, ev := range drainStream(t, stream) {
		if ev.Kind == models.KindToolCallDone {
			found = true
			if string(ev.ToolCall.Input) != "{}" {
				t.Errorf("ToolCall.Input = %s, want {}", ev.ToolCall.Input)
			}
		}
	}
	if !found {
		t.Error("no KindToolCallDone event")
	}
}

// TestUnknownDoneReasonPassthrough verifies an unrecognized done_reason (with no
// tool calls) is passed through verbatim.
func TestUnknownDoneReasonPassthrough(t *testing.T) {
	ts, _ := fakeServer(t, cannedTextOnlyNDJSON("guardrail"))
	a := ollama.New(ts.URL, "llama3.1")
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
	if got != models.StopReason("guardrail") {
		t.Errorf("StopReason = %q, want guardrail (passthrough)", got)
	}
}

// TestMalformedNDJSON verifies a malformed JSON chunk surfaces as a Recv error
// (no panic) after a valid leading chunk's events were delivered intact.
func TestMalformedNDJSON(t *testing.T) {
	body := `{"message":{"role":"assistant","content":"partial"},"done":false}` + "\n" +
		`{not valid json}` + "\n"

	ts, _ := fakeServer(t, body)
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events, derr := drainUntilErr(stream)
	if derr == nil || !strings.Contains(derr.Error(), "decode chunk") {
		t.Errorf("error = %v, want a 'decode chunk' error", derr)
	}
	var text string
	for _, ev := range events {
		if ev.Kind == models.KindDone {
			t.Error("KindDone emitted on a malformed stream")
		}
		if ev.Kind == models.KindTextDelta {
			text += ev.TextDelta
		}
	}
	if text != "partial" {
		t.Errorf("pre-error text = %q, want %q", text, "partial")
	}
}

// TestContextCancelMidStream verifies cancelling the request context mid-stream
// causes the next Recv to fail rather than hang or return a clean Done.
func TestContextCancelMidStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"hi"},"done":false}`+"\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a := ollama.New(ts.URL, "llama3.1")
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
	if ev.Kind != models.KindTextDelta || ev.TextDelta != "hi" {
		t.Errorf("first event = %+v, want text delta 'hi'", ev)
	}

	cancel()
	if _, err := stream.Recv(); err == nil {
		t.Error("expected Recv to fail after context cancel, got nil")
	}
}
