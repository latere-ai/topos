// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"latere.ai/x/pkg/llmdialect/lux"
)

// TestMarshalWritesTheStoredForm pins the bytes of the log: keys sorted,
// HTML left as it is, invalid UTF-8 written as U+FFFD, and a raw value
// with repeated names passed through.
func TestMarshalWritesTheStoredForm(t *testing.T) {
	for _, c := range []struct {
		name string
		in   any
		want string
	}{
		{"sorted keys", map[string]int{"b": 1, "a": 2}, `{"a":2,"b":1}`},
		{"html", map[string]string{"k": "<a & b>"}, `{"k":"<a & b>"}`},
		{"invalid utf-8", map[string]string{"k": "a\xffb"}, "{\"k\":\"a�b\"}"},
		{"repeated names in a raw value", map[string]jsontext.Value{"k": jsontext.Value(`{"a":1,"a":2}`)}, `{"k":{"a":1,"a":2}}`},
		{"no trailing newline", []int{1}, `[1]`},
	} {
		got, err := Marshal(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %q, %v, want %q", c.name, got, err, c.want)
		}
	}
}

// TestUnmarshalReadsWhatOtherSystemsWrote: a payload embeds model and
// process output, so repeated names (the last wins) and invalid UTF-8
// are read.
func TestUnmarshalReadsWhatOtherSystemsWrote(t *testing.T) {
	var v struct {
		A int    `json:"a"`
		S string `json:"s"`
	}
	if err := Unmarshal([]byte("{\"a\":1,\"a\":2,\"s\":\"x\xffy\"}"), &v); err != nil || v.A != 2 || v.S != "x�y" {
		t.Fatalf("%+v, %v", v, err)
	}
	if err := Unmarshal([]byte(`{"a":1`), &v); err == nil {
		t.Fatal("a truncated document decoded")
	}
}

// TestTheLuxWireTypesKeepTheirV1Omissions: the Lux wire types omit a
// false cache_hint and is_error, a zero reasoning_tokens and an absent
// raw value, and keep the empty object {} a call without arguments has.
func TestTheLuxWireTypesKeepTheirV1Omissions(t *testing.T) {
	msg := lux.Message{Role: "assistant", Blocks: []lux.Block{
		{Type: "text", Text: "hi"},
		{Type: "text", Text: "marked", CacheHint: true},
		{Type: "tool_use", ToolUse: &lux.ToolUse{ID: "t1", Name: "ls", Args: jsontext.Value(`{}`)}},
		{Type: "tool_use", ToolUse: &lux.ToolUse{ID: "t2", Name: "ls"}},
		{Type: "tool_result", ToolResult: &lux.ToolResult{ToolUseID: "t1", Blocks: []lux.Block{{Type: "text", Text: "ok"}}}},
		{Type: "tool_result", ToolResult: &lux.ToolResult{ToolUseID: "t2", IsError: true}},
	}}
	got, err := Marshal(msg)
	want := `{"role":"assistant","blocks":[{"type":"text","text":"hi"},{"type":"text","text":"marked","cache_hint":true},` +
		`{"type":"tool_use","tool_use":{"id":"t1","name":"ls","args":{}}},{"type":"tool_use","tool_use":{"id":"t2","name":"ls"}},` +
		`{"type":"tool_result","tool_result":{"tool_use_id":"t1","blocks":[{"type":"text","text":"ok"}]}},` +
		`{"type":"tool_result","tool_result":{"tool_use_id":"t2","is_error":true}}]}`
	if err != nil || string(got) != want {
		t.Fatalf("%s, %v\nwant %s", got, err, want)
	}
	cost := int64(7)
	got, err = Marshal(lux.Usage{InputTokens: 3, CostMicroUSD: &cost})
	if err != nil || string(got) != `{"input_tokens":3,"output_tokens":0,"cost_usd_micro":7}` {
		t.Fatalf("usage %s, %v", got, err)
	}
	got, err = Marshal(lux.Usage{ReasoningTokens: 2})
	if err != nil || string(got) != `{"input_tokens":0,"output_tokens":0,"reasoning_tokens":2}` {
		t.Fatalf("usage with reasoning %s, %v", got, err)
	}
	got, err = json.Marshal(lux.ServerTool{Type: "web_search", Config: jsontext.Value(`{}`)})
	if err != nil || string(got) != `{"type":"web_search","config":{}}` {
		t.Fatalf("server tool %s, %v", got, err)
	}
	got, err = json.Marshal(lux.WebSearch{UserLocation: jsontext.Value(`{}`)})
	if err != nil || string(got) != `{"user_location":{}}` {
		t.Fatalf("web search %s, %v", got, err)
	}
}
