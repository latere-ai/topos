// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dialect

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// update records the stub's responses into testdata/replay again.
var update = flag.Bool("update", false, "record testdata/replay/*.stub.sse from the stub Lux")

// dialects are the three wire dialects and the Lux door each is sent to.
var dialects = []struct {
	d    ir.Dialect
	door string
}{
	{ir.DialectAnthropicMessages, "/anthropic"},
	{ir.DialectOpenAIResponses, "/openai"},
	{ir.DialectOpenAIChat, "/openai"},
}

// replayed is the response the stub records for a dialect: every block
// a same-family replay carries back that the dialect can express. Chat
// has no replayable reasoning, so its response is text and two
// parallel tool calls. The signatures and the encrypted content are the
// stub's, not a provider's.
func replayed(d ir.Dialect) ir.Response {
	calls := []ir.Block{
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_01", Name: "read", Args: json.RawMessage(`{"path":"main.go"}`)}},
		{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "call_02", Name: "grep", Args: json.RawMessage(`{"pattern":"func main"}`)}},
	}
	text := ir.Block{Type: ir.BlockText, Text: "Reading the entry point."}
	var blocks []ir.Block
	switch d {
	case ir.DialectAnthropicMessages:
		blocks = []ir.Block{
			{Type: ir.BlockThinking, Text: "Read main.go, then search for func main.", Signature: "stub-signature-01"},
			{Type: ir.BlockRedactedThinking, Redacted: "stub-redacted-01"},
			text,
		}
	case ir.DialectOpenAIResponses:
		raw := json.RawMessage(`{"type":"reasoning","id":"rs_01","summary":[{"type":"summary_text","text":"Read main.go, then search for func main."}],"encrypted_content":"stub-encrypted-01"}`)
		blocks = []ir.Block{{Type: ir.BlockOpaque, Opaque: &ir.Opaque{Dialect: d, Kind: "reasoning", Raw: raw}}, text}
	default:
		blocks = []ir.Block{text}
	}
	return ir.Response{Model: "m", Blocks: append(blocks, calls...), StopReason: ir.StopToolUse, Usage: ir.Usage{InputTokens: 1200, OutputTokens: 80}}
}

// record streams the dialect's replayed response through the stub and
// writes the bytes it sent to testdata/replay/<dialect>.stub.sse.
func record(t *testing.T, d ir.Dialect, door string) {
	t.Helper()
	stub := luxstub.New(t)
	stub.Script("m", luxstub.Reply{Response: replayed(d)})
	s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: models.Connection{BaseURL: stub.URL() + door, Model: "m", Dialect: d}})
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := drain(t, s)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join("testdata", "replay"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "replay", string(d)+".stub.sse"), res.RawResponse, 0o644); err != nil {
		t.Fatal(err)
	}
}

// serveBytes answers every request with a recorded stream, as the
// provider sent it.
func serveBytes(t *testing.T, raw []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A failed write is the client gone; the test reads the error
		// from its side.
		if _, err := w.Write(raw); err != nil {
			return
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// nextRequest folds a user message, the assistant message as the log
// stores it and a result for each of its calls, then builds the next
// request as the harness does, through the Lux codec, and encodes it
// with the dialect's backend.
func nextRequest(t *testing.T, d ir.Dialect, msg lux.Message) (map[string]any, ir.Loss) {
	t.Helper()
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	payloads := []struct {
		typ session.Type
		p   any
	}{
		{session.TypeUserMessage, session.UserMessage{Content: []lux.Block{{Type: ir.BlockText, Text: "Find the entry point."}}}},
		{session.TypeAgentMessage, session.AgentMessage{Message: msg, StopReason: ir.StopToolUse}},
	}
	for _, b := range msg.Blocks {
		if b.Type == ir.BlockToolUse {
			payloads = append(payloads, struct {
				typ session.Type
				p   any
			}{session.TypeToolResult, session.ToolResult{ToolUseID: b.ToolUse.ID, Content: []lux.Block{{Type: ir.BlockText, Text: "ok"}}, Outcome: "ok"}})
		}
	}
	var evs []session.Event
	for i, p := range payloads {
		e, err := session.NewEvent(p.typ, p.p, now)
		if err != nil {
			t.Fatal(err)
		}
		e.Seq = uint64(i + 1)
		evs = append(evs, e)
	}
	tr, err := session.Fold(evs, "")
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(1024)
	body, err := json.Marshal(lux.Request{Model: "m", Messages: tr.Messages, MaxTokens: &limit, Stream: true, ReasoningReplay: d == ir.DialectOpenAIResponses})
	if err != nil {
		t.Fatal(err)
	}
	req, err := (&lux.Frontend{}).DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	be, _, err := codec(d)
	if err != nil {
		t.Fatal(err)
	}
	native, err := be.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(native, &out); err != nil {
		t.Fatal(err)
	}
	return out, req.Loss
}

// frames are the data members of an SSE stream, [DONE] left out.
func frames(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		out = append(out, f)
	}
	return out
}

// sentTurn assembles the assistant turn from a recorded stream by the
// provider's own streaming rules, with no llmdialect code, in the shape
// the dialect's request carries it back: Messages content blocks, the
// Responses output items, the Chat assistant message.
func sentTurn(t *testing.T, d ir.Dialect, raw []byte) any {
	t.Helper()
	fs := frames(t, raw)
	switch d {
	case ir.DialectAnthropicMessages:
		var blocks []map[string]any
		partial := map[int]string{}
		for _, f := range fs {
			i := int(num(f["index"]))
			switch f["type"] {
			case "content_block_start":
				blocks = append(blocks, f["content_block"].(map[string]any))
			case "content_block_delta":
				b, delta := blocks[i], f["delta"].(map[string]any)
				switch delta["type"] {
				case "text_delta":
					b["text"] = str(b["text"]) + str(delta["text"])
				case "thinking_delta":
					b["thinking"] = str(b["thinking"]) + str(delta["thinking"])
				case "signature_delta":
					b["signature"] = str(b["signature"]) + str(delta["signature"])
				case "input_json_delta":
					partial[i] += str(delta["partial_json"])
				}
			case "content_block_stop":
				if p := partial[i]; p != "" {
					var input any
					if err := json.Unmarshal([]byte(p), &input); err != nil {
						t.Fatal(err)
					}
					blocks[i]["input"] = input
				}
			}
		}
		return blocks
	case ir.DialectOpenAIResponses:
		var items []map[string]any
		for _, f := range fs {
			if f["type"] == "response.output_item.done" {
				items = append(items, f["item"].(map[string]any))
			}
		}
		return items
	}
	msg := map[string]any{"role": "assistant"}
	var content string
	var calls []map[string]any
	for _, f := range fs {
		choices, _ := f["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		content += str(delta["content"])
		tcs, _ := delta["tool_calls"].([]any)
		for _, tc := range tcs {
			tc := tc.(map[string]any)
			i := int(num(tc["index"]))
			for len(calls) <= i {
				calls = append(calls, map[string]any{"type": "function", "function": map[string]any{"name": "", "arguments": ""}})
			}
			fn, _ := tc["function"].(map[string]any)
			if id := str(tc["id"]); id != "" {
				calls[i]["id"] = id
			}
			f := calls[i]["function"].(map[string]any)
			f["name"] = str(f["name"]) + str(fn["name"])
			f["arguments"] = str(f["arguments"]) + str(fn["arguments"])
		}
	}
	msg["content"] = content
	msg["tool_calls"] = calls
	return msg
}

// sentBack is the assistant turn in the next request: the assistant
// message of Messages and Chat, the items between the user's message
// and the call outputs of Responses.
func sentBack(t *testing.T, d ir.Dialect, next map[string]any) any {
	t.Helper()
	if d == ir.DialectOpenAIResponses {
		var items []map[string]any
		for _, it := range next["input"].([]any) {
			item := it.(map[string]any)
			if item["type"] == "function_call_output" || item["role"] == "user" {
				continue
			}
			items = append(items, item)
		}
		return items
	}
	for _, m := range next["messages"].([]any) {
		if msg := m.(map[string]any); msg["role"] == "assistant" {
			if d == ir.DialectAnthropicMessages {
				return msg["content"]
			}
			return msg
		}
	}
	t.Fatal("the next request carries no assistant message")
	return nil
}

// normalized is v as canonical JSON: members sorted, and for Responses
// the members the provider assigns to its output and a request does not
// carry back, an output item's id and status and an output text's
// annotations and logprobs. A reasoning item is carried back whole, so
// it keeps its id.
func normalized(t *testing.T, d ir.Dialect, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var c any
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if items, ok := c.([]any); ok && d == ir.DialectOpenAIResponses {
		for _, it := range items {
			item := it.(map[string]any)
			if item["type"] == "reasoning" {
				continue
			}
			delete(item, "id")
			delete(item, "status")
			parts, _ := item["content"].([]any)
			for _, p := range parts {
				delete(p.(map[string]any), "annotations")
				delete(p.(map[string]any), "logprobs")
			}
		}
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func num(v any) float64 { f, _ := v.(float64); return f }
func str(v any) string  { s, _ := v.(string); return s }

// features names what a decoded assistant message carries, for the
// suite's check that each dialect's recordings cover the row.
func features(msg lux.Message) []string {
	var out []string
	calls := 0
	for _, b := range msg.Blocks {
		switch {
		case b.Type == ir.BlockText:
			out = append(out, "text")
		case b.Type == ir.BlockThinking && b.Signature != "":
			out = append(out, "signed thinking")
		case b.Type == ir.BlockRedactedThinking:
			out = append(out, "redacted thinking")
		case b.Type == ir.BlockOpaque && b.Opaque != nil && bytes.Contains(b.Opaque.Raw, []byte(`"encrypted_content"`)):
			out = append(out, "encrypted reasoning")
		case b.Type == ir.BlockToolUse:
			calls++
		}
	}
	if calls > 1 {
		out = append(out, "parallel tool calls")
	}
	return out
}

// TestSameFamilyReplayIsLossless is the gate of spec 007: every
// recorded response of a dialect decodes to the Lux message the log
// stores, folds into the next request, and comes back in that request's
// native encoding as the provider sent it, with no loss. The recordings
// are testdata/replay/<dialect>.*.sse; a live recording is one more file
// there.
func TestSameFamilyReplayIsLossless(t *testing.T) {
	want := map[ir.Dialect][]string{
		ir.DialectAnthropicMessages: {"text", "signed thinking", "redacted thinking", "parallel tool calls"},
		ir.DialectOpenAIResponses:   {"text", "encrypted reasoning", "parallel tool calls"},
		ir.DialectOpenAIChat:        {"text", "parallel tool calls"},
	}
	for _, c := range dialects {
		t.Run(string(c.d), func(t *testing.T) {
			if *update {
				record(t, c.d, c.door)
			}
			files, err := filepath.Glob(filepath.Join("testdata", "replay", string(c.d)+".*.sse"))
			if err != nil || len(files) == 0 {
				t.Fatalf("no recordings of %s: %v", c.d, err)
			}
			var covered []string
			for _, f := range files {
				raw, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				s, err := (&Model{}).Stream(t.Context(), models.Request{IR: request("m"), Connection: models.Connection{BaseURL: serveBytes(t, raw), Model: "m", Dialect: c.d}})
				if err != nil {
					t.Fatal(err)
				}
				res, _, err := drain(t, s)
				if cerr := s.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				if !bytes.Equal(res.RawResponse, raw) {
					t.Fatalf("%s: the raw response is not the bytes received", f)
				}
				stored, err := session.Marshal(res.Message)
				if err != nil {
					t.Fatal(err)
				}
				var msg lux.Message
				if err := json.Unmarshal(stored, &msg); err != nil {
					t.Fatal(err)
				}
				covered = append(covered, features(msg)...)
				next, loss := nextRequest(t, c.d, msg)
				if len(loss.Fields()) != 0 {
					t.Errorf("%s: the replay lost %v", f, loss.Strings())
				}
				sent, back := normalized(t, c.d, sentTurn(t, c.d, raw)), normalized(t, c.d, sentBack(t, c.d, next))
				if sent != back {
					t.Errorf("%s: the replayed turn differs from the one sent\nsent %s\nback %s", f, sent, back)
				}
			}
			for _, w := range want[c.d] {
				if !slices.Contains(covered, w) {
					t.Errorf("no recording of %s carries %s", c.d, w)
				}
			}
		})
	}
}

// TestTheSuiteSeesADifference: the comparison the gate rests on fails a
// turn that came back changed, so a codec that dropped a signature or
// reordered a call would not pass it.
func TestTheSuiteSeesADifference(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "replay", string(ir.DialectAnthropicMessages)+".stub.sse"))
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(raw, []byte("stub-signature-01"), []byte("stub-signature-02"), 1)
	if bytes.Equal(changed, raw) {
		t.Fatal("the recording carries no signature to change")
	}
	d := ir.DialectAnthropicMessages
	if normalized(t, d, sentTurn(t, d, raw)) == normalized(t, d, sentTurn(t, d, changed)) {
		t.Fatal("a changed signature compares equal")
	}
}
