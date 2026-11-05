// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// benchProse returns n bytes of deterministic text with characters JSON
// escapes, as a tool's output has.
func benchProse(n int) string {
	const unit = "Issue 4217 \"JSON v2 migration\" is open, assigned to ada & blocked by <4102>.\n\t"
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

var benchSizes = []struct {
	name string
	n    int
}{{"small", 256}, {"medium", 10 << 10}, {"large", 200 << 10}}

// benchCallResult is the JSON-RPC response to a tools/call whose text is n
// bytes.
func benchCallResult(b *testing.B, n int) []byte {
	b.Helper()
	out, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 7,
		"result": map[string]any{"resultType": "complete", "isError": false,
			"content": []any{map[string]any{"type": "text", "text": benchProse(n)}}},
	})
	if err != nil {
		b.Fatal(err)
	}
	return out
}

func benchResponse(typ string, body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {typ}}, Body: io.NopCloser(bytes.NewReader(body))}
}

// benchSchema is the input schema of a tracker tool, with a header mark.
const benchSchema = `{"type":"object","properties":{"project":{"type":"string","x-mcp-header":"Project","description":"The project key."},"title":{"type":"string","description":"The issue title."},"labels":{"type":"array","items":{"type":"string"}},"assignee":{"type":"object","properties":{"name":{"type":"string"},"id":{"type":"integer"}}}},"required":["project","title"]}`

// BenchmarkJSONRPCCallResultDecode is a tools/call answered as one JSON
// object: the response read and its result read into the tool's Result.
func BenchmarkJSONRPCCallResultDecode(b *testing.B) {
	c := &Client{}
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			body := benchCallResult(b, s.n)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				res, err := c.answer(benchResponse("application/json", body), "7", true)
				if err != nil {
					b.Fatal(err)
				}
				var r Result
				if err := json.Unmarshal(res, &r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONRPCStreamDecode is a tools/call answered on an event
// stream: a progress notification, then the response, read until the one
// that carries the request's id.
func BenchmarkJSONRPCStreamDecode(b *testing.B) {
	c := &Client{}
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			note := `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"p1","progress":1,"total":2}}`
			stream := []byte("event: message\ndata: " + note + "\n\nevent: message\ndata: " + string(benchCallResult(b, s.n)) + "\n\n")
			b.ReportAllocs()
			b.SetBytes(int64(len(stream)))
			for b.Loop() {
				res, err := c.answer(benchResponse("text/event-stream", stream), "7", true)
				if err != nil {
					b.Fatal(err)
				}
				var r Result
				if err := json.Unmarshal(res, &r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONRPCToolListDecode is a server's tools/list page read when a
// session connects: 4, 40 and 400 tools make the small, medium and large
// pages.
func BenchmarkJSONRPCToolListDecode(b *testing.B) {
	c := &Client{}
	for _, s := range []struct {
		name  string
		tools int
	}{{"small", 4}, {"medium", 40}, {"large", 400}} {
		b.Run(s.name, func(b *testing.B) {
			list := make([]Tool, s.tools)
			for i := range list {
				list[i] = Tool{Name: fmt.Sprintf("issue_tool_%03d", i), Description: "Creates, updates or searches the issues of a project in the tracker and answers the matching ones.", InputSchema: json.RawMessage(benchSchema)}
			}
			body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 3, "result": map[string]any{"tools": list}})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				res, err := c.answer(benchResponse("application/json", body), "3", true)
				if err != nil {
					b.Fatal(err)
				}
				var page struct {
					Tools      []Tool `json:"tools"`
					NextCursor string `json:"nextCursor"`
				}
				if err := json.Unmarshal(res, &page); err != nil {
					b.Fatal(err)
				}
				if len(page.Tools) != s.tools {
					b.Fatalf("%d tools", len(page.Tools))
				}
			}
		})
	}
}

// BenchmarkJSONToolSchemaHeaderParamsDecode is a listed tool's input
// schema read for its x-mcp-header marks, once per tool a session loads.
func BenchmarkJSONToolSchemaHeaderParamsDecode(b *testing.B) {
	raw := json.RawMessage(benchSchema)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for b.Loop() {
		ps, ok := headerParams(raw)
		if !ok || len(ps) != 1 {
			b.Fatal("header marks")
		}
	}
}
