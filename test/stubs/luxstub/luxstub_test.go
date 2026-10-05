// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package luxstub

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/anthropic"
	"latere.ai/x/pkg/llmdialect/bridge"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/openairesp"
)

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

const chatBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestTheStubAnswersAsScripted(t *testing.T) {
	s := New(t)
	if code, _ := post(t, s.URL()+"/nowhere", chatBody); code != http.StatusNotFound {
		t.Fatalf("an unknown door: %d", code)
	}
	if code, _ := post(t, s.URL()+PathChat, `{`); code != http.StatusBadRequest {
		t.Fatalf("an undecodable body: %d", code)
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusNotFound || !strings.Contains(body, "no scripted reply") {
		t.Fatalf("no reply: %d %s", code, body)
	}
	s.Script("m",
		Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "one"}}}, Fail: &Failure{Status: 503, Times: 2}},
		Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "two"}}}, Expect: func(*ir.Request) error { return errors.New("wanted a tool result") }},
	)
	for range 2 {
		if code, _ := post(t, s.URL()+PathChat, chatBody); code != http.StatusServiceUnavailable {
			t.Fatalf("an injected failure: %d", code)
		}
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusOK || !strings.Contains(body, "one") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("the reply after two failures: %d %s", code, body)
	}
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusBadRequest || !strings.Contains(body, "wanted a tool result") {
		t.Fatalf("a failed expectation: %d %s", code, body)
	}
	if n := len(s.Requests()); n != 5 {
		t.Fatalf("%d requests recorded", n)
	}
}

// TestARawReplyIsServedByteForByte: a reply's raw body reaches the
// client as written, after the request passed its expectation.
func TestARawReplyIsServedByteForByte(t *testing.T) {
	s := New(t)
	const raw = "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\"}}]}}]}\n\ndata: [DONE]\n\n"
	s.Script("m", Reply{Raw: raw})
	if code, body := post(t, s.URL()+PathChat, chatBody); code != http.StatusOK || body != raw {
		t.Fatalf("raw reply: %d %q", code, body)
	}
}

func TestEventsFollowTheGrammar(t *testing.T) {
	evs := Events(ir.Response{Blocks: []ir.Block{
		{Type: ir.BlockThinking, Text: "t", Signature: "s"},
		{Type: ir.BlockRedactedThinking, Redacted: "r"},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "a", Name: "b", Args: []byte(`{}`)}},
	}})
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, string(e.Type))
	}
	want := "message_start block_start thinking_delta signature_delta block_stop block_start block_stop block_start args_delta block_stop message_delta message_stop"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events %s", got)
	}
	if evs[0].ID != "msg_stub" || evs[len(evs)-2].StopReason != ir.StopEndTurn || evs[5].Block.Redacted != "r" {
		t.Fatalf("defaults %+v", evs)
	}
}

func TestAnInjectedErrorEventFollowsThePartialStream(t *testing.T) {
	s := New(t)
	s.Script("m", Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "partial"}}}, Fail: &Failure{Event: "event: error\ndata: {}\n\n"}})
	code, body := post(t, s.URL()+PathChat, chatBody)
	if code != http.StatusOK || !bytes.HasSuffix([]byte(body), []byte("event: error\ndata: {}\n\n")) || strings.Contains(body, "[DONE]") {
		t.Fatalf("%d %q", code, body)
	}
}

func TestEachDoorListsTheModelsWithTheirFigures(t *testing.T) {
	s := New(t)
	s.Models(bridge.Model{Name: "vendor/m", ContextWindow: 1000, MaxOutputTokens: 100, InputModalities: []string{"text", "image"},
		Pricing: &bridge.ModelPricing{Currency: "USD", Input: "1.5", Output: "6"}})
	for path, want := range map[string]string{
		"/openai" + PathModels:    `"context_window":1000,"max_output_tokens":100`,
		"/anthropic" + PathModels: `"max_input_tokens":1000,"max_tokens":100`,
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), `"id":"vendor/m"`) || !strings.Contains(string(b), want) || !strings.Contains(string(b), `"input":"1.5"`) {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
		}
	}
	if l := s.Listed(); len(l) != 2 || l[0].Get("Authorization") != "Bearer k" || len(s.Requests()) != 0 {
		t.Fatalf("listed %v, %d model requests", l, len(s.Requests()))
	}
}

// TestAKeyIsAnsweredForTheModelsItSelects: with a selection, each door's
// list names the models the presented key selects alone, whichever header
// carries it, and a request on another model is recorded and refused
// model_not_allowed; a selection the test widens reaches the next request.
func TestAKeyIsAnsweredForTheModelsItSelects(t *testing.T) {
	s := New(t)
	s.Models(bridge.Model{Name: "vendor/a", ContextWindow: 1000, MaxOutputTokens: 100}, bridge.Model{Name: "vendor/b", ContextWindow: 2000, MaxOutputTokens: 200})
	s.Script("vendor/b", Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockText, Text: "b"}}}})
	var mu sync.Mutex
	selected := map[string][]string{"k": {"vendor/a"}}
	s.Select(func(key string) []string {
		mu.Lock()
		defer mu.Unlock()
		return selected[key]
	})
	list := func(path string, header http.Header) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, h := range []http.Header{{"Authorization": {"Bearer k"}}, {"X-Api-Key": {"k"}}, {"X-Goog-Api-Key": {"k"}}} {
		if got := list("/openai"+PathModels, h); !strings.Contains(got, `"id":"vendor/a"`) || strings.Contains(got, "vendor/b") {
			t.Fatalf("the list of %v: %s", h, got)
		}
	}
	if got := list("/anthropic"+PathModels, http.Header{"X-Api-Key": {"other"}}); strings.Contains(got, "vendor/") {
		t.Fatalf("a key that selects nothing listed %s", got)
	}
	call := func() (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+PathChat, strings.NewReader(strings.Replace(chatBody, `"m"`, `"vendor/b"`, 1)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer k")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(resp.Body)
		if cerr := resp.Body.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}
	if code, body := call(); code != http.StatusForbidden || !strings.Contains(body, "model_not_allowed") || len(s.Requests()) != 1 {
		t.Fatalf("a model the key does not select: %d %s, %d recorded", code, body, len(s.Requests()))
	}
	mu.Lock()
	selected["k"] = append(selected["k"], "vendor/b")
	mu.Unlock()
	if got := list("/openai"+PathModels, http.Header{"Authorization": {"Bearer k"}}); !strings.Contains(got, `"id":"vendor/b"`) {
		t.Fatalf("the list after the key widened: %s", got)
	}
	if code, body := call(); code != http.StatusOK || !strings.Contains(body, `"b"`) {
		t.Fatalf("the model after the key widened: %d %s", code, body)
	}
}

func TestAResponsesReasoningItemStreamsAsTheProviderSendsIt(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"weigh it"}],"encrypted_content":"gAAAA-stub"}`)
	s := New(t)
	s.Script("m", Reply{Response: ir.Response{Blocks: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{Dialect: ir.DialectOpenAIResponses, Kind: "reasoning", Raw: raw}},
		{Type: ir.BlockText, Text: "done"},
	}}})
	code, body := post(t, s.URL()+PathResponses, `{"model":"m","stream":true,"input":"hi"}`)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	dec := openairesp.NewBackend().NewEventDecoder(strings.NewReader(body))
	var blocks []ir.BlockType
	var opaque *ir.Opaque
	var thinking string
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch ev.Type {
		case ir.EventBlockStart:
			blocks = append(blocks, ev.Block.Type)
			if ev.Block.Opaque != nil {
				opaque = ev.Block.Opaque
			}
		case ir.EventThinkingDelta:
			thinking += ev.Delta
		}
	}
	if len(blocks) != 3 || blocks[0] != ir.BlockThinking || blocks[1] != ir.BlockOpaque || blocks[2] != ir.BlockText || thinking != "weigh it" {
		t.Fatalf("blocks %v, thinking %q", blocks, thinking)
	}
	if opaque == nil || !strings.Contains(string(opaque.Raw), `"encrypted_content":"gAAAA-stub"`) {
		t.Fatalf("opaque %+v", opaque)
	}
	s.Script("m", Reply{Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockOpaque, Opaque: &ir.Opaque{Dialect: ir.DialectOpenAIResponses, Kind: "reasoning", Raw: json.RawMessage(`[`)}}}}})
	if code, body := post(t, s.URL()+PathResponses, `{"model":"m","stream":true,"input":"hi"}`); code != http.StatusOK || strings.Contains(body, "response.completed") {
		t.Fatalf("an unreadable reasoning item: %d %s", code, body)
	}
}

func TestARedactedThinkingBlockStreamsAsTheMessagesAPISendsIt(t *testing.T) {
	s := New(t)
	s.Script("m", Reply{Response: ir.Response{Blocks: []ir.Block{
		{Type: ir.BlockOpaque, Opaque: &ir.Opaque{Dialect: ir.DialectOpenAIResponses, Kind: "reasoning", Raw: json.RawMessage(`{}`)}},
		{Type: ir.BlockRedactedThinking, Redacted: "r1"},
		{Type: ir.BlockText, Text: "done"},
	}}})
	code, body := post(t, s.URL()+PathMessages, `{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	dec := anthropic.NewBackend(anthropic.BackendOptions{}).NewEventDecoder(strings.NewReader(body))
	var blocks []ir.Block
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if ev.Type == ir.EventBlockStart {
			if ev.Index != len(blocks) {
				t.Fatalf("block %d started at index %d", len(blocks), ev.Index)
			}
			blocks = append(blocks, *ev.Block)
		}
	}
	if len(blocks) != 2 || blocks[0].Type != ir.BlockRedactedThinking || blocks[0].Redacted != "r1" || blocks[1].Type != ir.BlockText {
		t.Fatalf("blocks %+v", blocks)
	}
}

// TestAHoldPacesTheStream: a reply's hold runs before each event, after
// the events before it reached the client, so the client reads a tool
// call's start while the hold before its arguments still waits.
func TestAHoldPacesTheStream(t *testing.T) {
	s := New(t)
	read := make(chan string, 1)
	var mu sync.Mutex
	var seen []ir.EventType
	s.Script("m", Reply{
		Response: ir.Response{Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "a", Name: "write", Args: []byte(`{"path":"f"}`)}}}, StopReason: ir.StopToolUse},
		Hold: func(ev ir.Event) {
			mu.Lock()
			seen = append(seen, ev.Type)
			mu.Unlock()
			if ev.Type == ir.EventArgsDelta {
				if got := <-read; !strings.Contains(got, `"write"`) {
					t.Errorf("the client read %q before the arguments, want the call's start", got)
				}
			}
		},
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+PathChat, strings.NewReader(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	// The call's start arrives on its own: the hold before the arguments
	// waits for this read to say what it saw.
	var head []byte
	buf := make([]byte, 4096)
	for !bytes.Contains(head, []byte(`"write"`)) {
		n, err := resp.Body.Read(buf)
		head = append(head, buf[:n]...)
		if err != nil {
			t.Fatalf("the stream ended before the call's start: %v, %q", err, head)
		}
	}
	read <- string(head)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "path") || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("the rest of the stream: %q", rest)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(Events(ir.Response{Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{}}}})) {
		t.Fatalf("the hold ran for %v", seen)
	}
}
