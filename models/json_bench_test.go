// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"
)

// benchProse returns n bytes of deterministic source-like text with
// characters JSON escapes.
func benchProse(n int) string {
	const unit = "func main() {\n\tfmt.Println(\"hello, <world> & friends\")\n}\n"
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// benchArgs are the arguments of a write call whose content is n bytes.
func benchArgs(b *testing.B, n int) string {
	b.Helper()
	raw, err := json.Marshal(map[string]any{"path": "internal/server/generated.go", "content": benchProse(n)})
	if err != nil {
		b.Fatal(err)
	}
	return string(raw)
}

var benchSizes = []struct {
	name string
	n    int
}{{"small", 256}, {"medium", 10 << 10}, {"large", 200 << 10}}

// benchResponse is a model's answer: some text and one write call.
func benchResponse(b *testing.B, n int) *ir.Response {
	b.Helper()
	return &ir.Response{
		ID: "msg_01XFDUDYJgAACzvnptvVoYEL", Model: "claude-haiku-5-5",
		Blocks: []ir.Block{
			{Type: ir.BlockText, Text: "I'll write the file now, then run the tests."},
			{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_01A9XkQ2", Name: "write", Args: json.RawMessage(benchArgs(b, n))}},
		},
		StopReason: ir.StopToolUse,
		Usage:      ir.Usage{InputTokens: 1200, OutputTokens: 80},
	}
}

// BenchmarkJSONLuxMessageRoundTrip is the response of every model step
// encoded as the Lux wire response and read back into the message the log
// keeps.
func BenchmarkJSONLuxMessageRoundTrip(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			resp := benchResponse(b, s.n)
			msg, _, err := LuxMessage(resp)
			if err != nil {
				b.Fatal(err)
			}
			enc, err := json.Marshal(msg)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(enc)))
			for b.Loop() {
				if _, _, err := LuxMessage(resp); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONAccumulatorToolArgsDecode is the stream accumulator settling
// a tool call whose arguments arrived as fragments of 64 bytes: the text is
// checked to be one JSON value when the block stops and again when the
// message does.
func BenchmarkJSONAccumulatorToolArgsDecode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			args := benchArgs(b, s.n)
			events := []ir.Event{
				{Type: ir.EventMessageStart, ID: "msg_01XFDUDYJgAACzvnptvVoYEL", Model: "claude-haiku-5-5"},
				{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: "toolu_01A9XkQ2", Name: "write"}}},
			}
			for rest := args; rest != ""; {
				n := min(64, len(rest))
				events = append(events, ir.Event{Type: ir.EventArgsDelta, Index: 0, Delta: rest[:n]})
				rest = rest[n:]
			}
			events = append(events,
				ir.Event{Type: ir.EventBlockStop, Index: 0},
				ir.Event{Type: ir.EventMessageDelta, StopReason: ir.StopToolUse},
				ir.Event{Type: ir.EventMessageStop})
			b.ReportAllocs()
			b.SetBytes(int64(len(args)))
			for b.Loop() {
				var a Accumulator
				for _, ev := range events {
					if err := a.Add(ev); err != nil {
						b.Fatal(err)
					}
				}
				if a.InvalidArgs() != nil {
					b.Fatal("invalid arguments")
				}
			}
		})
	}
}
