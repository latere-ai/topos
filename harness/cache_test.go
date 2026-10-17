// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
)

// encodeTurn is a turn's request for model on the OpenAI door's Chat
// Completions, as the harness builds and the runner encodes it: a system
// prompt with its date, a user message, a tool call, and its result.
func encodeTurn(t *testing.T, model string) []byte {
	t.Helper()
	msgs := []lux.Message{
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockText, Text: "read a.txt"}}},
		{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "t1", Name: "read", Args: json.RawMessage(`{"path":"a.txt"}`)}}}},
		{Role: ir.RoleUser, Blocks: []lux.Block{{Type: ir.BlockToolResult, ToolResult: &lux.ToolResult{ToolUseID: "t1", Blocks: []lux.Block{{Type: ir.BlockText, Text: "contents"}}}}}},
	}
	req, err := buildRequest(requestParts{Model: model, System: []lux.Block{{Type: ir.BlockText, Text: "HARNESS"}}, Date: "Today is 2026-10-17.", Messages: msgs, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	conn := models.Connection{BaseURL: "https://lux.example/openai", Model: model, Dialect: ir.DialectOpenAIChat}
	body, err := (&dialect.Model{}).Encode(models.Request{IR: req, Connection: conn})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

const cacheMark = `"cache_control":{"type":"ephemeral"}`

// TestATurnOnChatCompletionsKeepsItsBreakpoints: an Anthropic or a Qwen
// model a Lux installation names under its vendor and serves through a
// gateway on Chat Completions is reached through the OpenAI door, and
// its providers, Anthropic's and Alibaba, cache only what is marked. A
// turn's breakpoints, after the system prompt and after the latest tool
// result, reach the wire as cache_control, so each turn reads the
// conversation so far from the provider's cache instead of paying for
// it again.
func TestATurnOnChatCompletionsKeepsItsBreakpoints(t *testing.T) {
	for _, model := range []string{"anthropic/claude-sonnet-5.5", "qwen/qwen3.8-27b"} {
		t.Run(model, func(t *testing.T) {
			body := encodeTurn(t, model)
			var sent struct {
				Messages []struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatal(err)
			}
			var marked []string
			for _, m := range sent.Messages {
				if strings.Contains(string(m.Content), cacheMark) {
					marked = append(marked, m.Role)
				}
			}
			if strings.Join(marked, ",") != "system,user,tool" {
				t.Fatalf("breakpoints on %v, want system, user and tool: %s", marked, body)
			}
			if system := string(sent.Messages[0].Content); !strings.Contains(system[strings.Index(system, cacheMark):], "Today is") {
				t.Fatalf("the date is not after the system breakpoint: %s", system)
			}
		})
	}
}

// TestATurnOnChatCompletionsMarksNothingForAModelThatCachesOnItsOwn: the
// providers of an OpenAI, Google or open-weights model cache a repeated
// prefix with no marks, and a Gemini model given one writes a new paid
// cache every turn, so the turn carries no cache_control.
func TestATurnOnChatCompletionsMarksNothingForAModelThatCachesOnItsOwn(t *testing.T) {
	for _, model := range []string{"openai/gpt-6.1-sol", "google/gemini-3.6-flash", "google/gemma-4-26b-a4b-it", "deepseek/deepseek-v4-flash"} {
		if body := encodeTurn(t, model); strings.Contains(string(body), "cache_control") {
			t.Errorf("%s: the turn carries cache_control: %s", model, body)
		}
	}
}
