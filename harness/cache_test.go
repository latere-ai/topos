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

// TestATurnOnChatCompletionsKeepsItsBreakpointsForAnAnthropicModel: an
// Anthropic model a Lux installation names under the anthropic vendor
// and serves through a gateway on Chat Completions is reached through
// the OpenAI door. A turn's breakpoints, after the system prompt and
// after the latest tool result, reach the wire as cache_control, so each
// turn reads the conversation so far from the provider's cache instead
// of paying for it again.
func TestATurnOnChatCompletionsKeepsItsBreakpointsForAnAnthropicModel(t *testing.T) {
	const model = "anthropic/claude-sonnet-5.5"
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
	var sent struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatal(err)
	}
	const mark = `"cache_control":{"type":"ephemeral"}`
	var marked []string
	for _, m := range sent.Messages {
		if strings.Contains(string(m.Content), mark) {
			marked = append(marked, m.Role)
		}
	}
	if strings.Join(marked, ",") != "system,user,tool" {
		t.Fatalf("breakpoints on %v, want system, user and tool: %s", marked, body)
	}
	if system := string(sent.Messages[0].Content); !strings.Contains(system[strings.Index(system, mark):], "Today is") {
		t.Fatalf("the date is not after the system breakpoint: %s", system)
	}
}
