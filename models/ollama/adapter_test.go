package ollama_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"latere.ai/x/agents/internal/models"
	"latere.ai/x/agents/internal/models/ollama"
)

// ---------------------------------------------------------------------------
// Canned NDJSON bodies
// ---------------------------------------------------------------------------

// cannedNDJSON returns a multi-chunk NDJSON body that exercises the full
// normalized event sequence: text → tool_call_done → usage → done.
//
// Chunks emitted:
//  1. Text fragment "Hello " (not done)
//  2. Tool call for "get_weather" with {"city":"London"} (not done)
//  3. Terminal chunk: done=true, done_reason, eval counts
func cannedNDJSON(doneReason string) string {
	if doneReason == "" {
		doneReason = "stop"
	}
	return strings.Join([]string{
		`{"message":{"role":"assistant","content":"Hello ","tool_calls":null},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_weather","arguments":{"city":"London"}}}]},"done":false}`,
		`{"message":{"role":"assistant","content":"","tool_calls":null},"done":true,"done_reason":"` + doneReason + `","prompt_eval_count":42,"eval_count":17}`,
	}, "\n") + "\n"
}

// cannedTextOnlyNDJSON returns a minimal NDJSON body with text only (no tools).
func cannedTextOnlyNDJSON(doneReason string) string {
	if doneReason == "" {
		doneReason = "stop"
	}
	return strings.Join([]string{
		`{"message":{"role":"assistant","content":"world"},"done":false}`,
		`{"message":null,"done":true,"done_reason":"` + doneReason + `","prompt_eval_count":10,"eval_count":5}`,
	}, "\n") + "\n"
}

// ---------------------------------------------------------------------------
// Helper: start a fake Ollama NDJSON server
// ---------------------------------------------------------------------------

type capturedRequest struct {
	req  *http.Request
	body []byte
}

func fakeServer(t *testing.T, body string) (*httptest.Server, func() *capturedRequest) {
	t.Helper()
	ch := make(chan *capturedRequest, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		select {
		case ch <- &capturedRequest{req: r, body: reqBody}:
		default:
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
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
	defer s.Close()
	var events []models.Event
	for {
		ev, err := s.Recv()
		if err == io.EOF {
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
// AC 1: normalized event sequence (text → tool_call_done → usage → done)
// ---------------------------------------------------------------------------

func TestNormalizedEventSequence(t *testing.T) {
	ts, _ := fakeServer(t, cannedNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

	stream, err := a.Stream(context.Background(), models.Request{
		System:    "You are helpful.",
		Messages:  []models.Message{{Role: "user", Content: "weather?"}},
		MaxTokens: 1024,
		Tools: []models.ToolDef{
			{
				Name:        "get_weather",
				Description: "Get weather for a city",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)

	var textDeltas []string
	var toolCallDones []*models.ToolCall
	var usages []*models.Usage
	var doneEvents []models.Event

	for _, ev := range events {
		switch ev.Kind {
		case models.KindTextDelta:
			textDeltas = append(textDeltas, ev.TextDelta)
		case models.KindToolCallDone:
			toolCallDones = append(toolCallDones, ev.ToolCall)
		case models.KindUsage:
			usages = append(usages, ev.Usage)
		case models.KindDone:
			doneEvents = append(doneEvents, ev)
		}
	}

	// Text delta.
	if got := strings.Join(textDeltas, ""); got != "Hello " {
		t.Errorf("text content = %q, want %q", got, "Hello ")
	}

	// One tool call done.
	if len(toolCallDones) != 1 {
		t.Fatalf("tool call dones = %d, want 1", len(toolCallDones))
	}
	tc := toolCallDones[0]
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want %q", tc.Name, "get_weather")
	}
	if tc.ID == "" {
		t.Error("ToolCall.ID must not be empty (synthesized)")
	}
	// Input must be valid JSON with correct city.
	var inputMap map[string]any
	if err := json.Unmarshal(tc.Input, &inputMap); err != nil {
		t.Errorf("ToolCall.Input is not valid JSON: %v (raw: %s)", err, tc.Input)
	}
	if inputMap["city"] != "London" {
		t.Errorf("ToolCall.Input[city] = %v, want London", inputMap["city"])
	}

	// One Usage event with correct counts.
	if len(usages) != 1 {
		t.Fatalf("usage events = %d, want 1", len(usages))
	}
	u := usages[0]
	if u.InputTokens != 42 {
		t.Errorf("Usage.InputTokens = %d, want 42", u.InputTokens)
	}
	if u.OutputTokens != 17 {
		t.Errorf("Usage.OutputTokens = %d, want 17", u.OutputTokens)
	}
	if u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 {
		t.Error("Ollama adapter should leave cache token fields zero")
	}

	// Exactly one Done.
	if len(doneEvents) != 1 {
		t.Fatalf("done events = %d, want 1", len(doneEvents))
	}
	// Tool call was seen → StopToolUse.
	if doneEvents[0].StopReason != models.StopToolUse {
		t.Errorf("StopReason = %q, want %q", doneEvents[0].StopReason, models.StopToolUse)
	}
}

// ---------------------------------------------------------------------------
// AC 2: stop reason mapping
// ---------------------------------------------------------------------------

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		doneReason string
		want       models.StopReason
	}{
		{"stop", models.StopEndTurn},
		{"", models.StopEndTurn},
		{"length", models.StopMaxTokens},
	}

	for _, tc := range cases {
		t.Run(tc.doneReason, func(t *testing.T) {
			ts, _ := fakeServer(t, cannedTextOnlyNDJSON(tc.doneReason))
			a := ollama.New(ts.URL, "llama3.1")
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

// StopToolUse when tool calls are seen, regardless of done_reason.
func TestStopReasonToolUseOverride(t *testing.T) {
	// Even with done_reason="stop", if tool calls were emitted → StopToolUse.
	ts, _ := fakeServer(t, cannedNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 256,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)
	for _, ev := range events {
		if ev.Kind == models.KindDone {
			if ev.StopReason != models.StopToolUse {
				t.Errorf("StopReason = %q, want StopToolUse (tool_calls seen)", ev.StopReason)
			}
			return
		}
	}
	t.Error("no KindDone event received")
}

// ---------------------------------------------------------------------------
// AC 3: tool round-trip (ToolDef in → ToolCall out, Input intact)
// ---------------------------------------------------------------------------

func TestToolRoundTrip(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`)

	ts, getReq := fakeServer(t, cannedNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

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
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}
	if len(wireReq.Tools) != 1 {
		t.Fatalf("wire tools = %d, want 1", len(wireReq.Tools))
	}
	fn := wireReq.Tools[0].Function
	if fn.Name != "get_weather" {
		t.Errorf("wire tool name = %q, want %q", fn.Name, "get_weather")
	}
	if wireReq.Tools[0].Type != "function" {
		t.Errorf("wire tool type = %q, want function", wireReq.Tools[0].Type)
	}
	// Parameters must match the input schema.
	var wantSchema, gotSchema any
	json.Unmarshal(inputSchema, &wantSchema)
	json.Unmarshal(fn.Parameters, &gotSchema)
	wantJSON, _ := json.Marshal(wantSchema)
	gotJSON, _ := json.Marshal(gotSchema)
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("parameters mismatch: got %s, want %s", gotJSON, wantJSON)
	}

	// Verify assembled ToolCall.Input from stream.
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
// AC 4: usage mapping (InputTokens, OutputTokens)
// ---------------------------------------------------------------------------

func TestUsageMapping(t *testing.T) {
	ts, _ := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")
	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := drainStream(t, stream)

	for _, ev := range events {
		if ev.Kind == models.KindUsage {
			if ev.Usage.InputTokens != 10 {
				t.Errorf("InputTokens = %d, want 10", ev.Usage.InputTokens)
			}
			if ev.Usage.OutputTokens != 5 {
				t.Errorf("OutputTokens = %d, want 5", ev.Usage.OutputTokens)
			}
			return
		}
	}
	t.Error("no KindUsage event received")
}

// ---------------------------------------------------------------------------
// AC 5: header + path assertion (POST /api/chat)
// ---------------------------------------------------------------------------

func TestRequestHeaderAndPath(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

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
	if got := captured.req.Method; got != http.MethodPost {
		t.Errorf("method = %q, want POST", got)
	}
	if got := captured.req.URL.Path; got != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", got)
	}
	if got := captured.req.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// ---------------------------------------------------------------------------
// AC 6: system prompt → leading system message
// ---------------------------------------------------------------------------

func TestSystemPromptInjected(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

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
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}
	if len(wireReq.Messages) == 0 {
		t.Fatal("no messages in wire request")
	}
	first := wireReq.Messages[0]
	if first.Role != "system" {
		t.Errorf("first message role = %q, want system", first.Role)
	}
	if first.Content != "You are a helpful assistant." {
		t.Errorf("system content = %q, want %q", first.Content, "You are a helpful assistant.")
	}
}

// ---------------------------------------------------------------------------
// AC 7: HTTP error response surfaces as *APIError
// ---------------------------------------------------------------------------

func TestHTTPErrorResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer ts.Close()

	a := ollama.New(ts.URL, "nonexistent")
	_, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err == nil {
		t.Fatal("expected error for 404 response, got nil")
	}
	apiErr, ok := err.(*ollama.APIError)
	if !ok {
		t.Errorf("expected *ollama.APIError, got %T: %v", err, err)
	} else if apiErr.Status != http.StatusNotFound {
		t.Errorf("APIError.Status = %d, want %d", apiErr.Status, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------------
// AC 8: after KindDone, next Recv returns io.EOF
// ---------------------------------------------------------------------------

func TestRecvAfterDoneReturnsEOF(t *testing.T) {
	ts, _ := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

	stream, err := a.Stream(context.Background(), models.Request{
		Messages:  []models.Message{{Role: "user", Content: "hi"}},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var sawDone bool
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			if !sawDone {
				t.Error("got io.EOF without ever seeing KindDone")
			}
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Kind == models.KindDone {
			sawDone = true
			// Next call must be io.EOF.
			_, nextErr := stream.Recv()
			if nextErr != io.EOF {
				t.Errorf("after KindDone, Recv returned %v, want io.EOF", nextErr)
			}
			break
		}
	}
}

// ---------------------------------------------------------------------------
// AC 9: tool-role message down-conversion
// ---------------------------------------------------------------------------

func TestToolMessageDownConversion(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", Content: ""},
			{Role: "tool", ToolResults: []models.ToolResult{
				{CallID: "call_1", Content: "Sunny, 25°C"},
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
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}

	var foundTool bool
	for _, m := range wireReq.Messages {
		if m.Role == "tool" {
			foundTool = true
			if m.Content != "Sunny, 25°C" {
				t.Errorf("tool message content = %q, want %q", m.Content, "Sunny, 25°C")
			}
		}
	}
	if !foundTool {
		t.Error("no tool-role message found in wire request")
	}
}

// ---------------------------------------------------------------------------
// AC 10: assistant tool-call history preserved in multi-turn replay
// ---------------------------------------------------------------------------

func TestAssistantToolCallsInHistory(t *testing.T) {
	ts, getReq := fakeServer(t, cannedTextOnlyNDJSON("stop"))
	a := ollama.New(ts.URL, "llama3.1")

	stream, err := a.Stream(context.Background(), models.Request{
		Messages: []models.Message{
			{Role: "user", Content: "What is the weather?"},
			{
				Role: "assistant",
				ToolCalls: []models.ToolCall{
					{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Paris"}`)},
				},
			},
			{Role: "tool", ToolResults: []models.ToolResult{
				{CallID: "call_1", Content: "Sunny, 25°C"},
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
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(captured.body, &wireReq); err != nil {
		t.Fatalf("decode wire request: %v", err)
	}

	var foundAssistantToolCall bool
	for _, m := range wireReq.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			foundAssistantToolCall = true
			fn := m.ToolCalls[0].Function
			if fn.Name != "get_weather" {
				t.Errorf("assistant tool_call name = %q, want get_weather", fn.Name)
			}
			var args map[string]any
			if err := json.Unmarshal(fn.Arguments, &args); err != nil {
				t.Fatalf("tool_call arguments not valid JSON: %v", err)
			}
			if args["city"] != "Paris" {
				t.Errorf("tool_call arguments[city] = %v, want Paris", args["city"])
			}
		}
	}
	if !foundAssistantToolCall {
		t.Error("assistant message with tool_calls not found in wire request (multi-turn tool history dropped)")
	}
}
