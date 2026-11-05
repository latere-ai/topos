// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// benchProse returns n bytes of deterministic prose-like text with a few
// characters JSON escapes, as a model's answer or a file's content has.
func benchProse(n int) string {
	const unit = "The runner reads \"main.go\", finds func main() and wires the store.\n\tIt then appends the batch & returns <ok> to the caller. "
	if n <= 0 {
		return ""
	}
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// benchSizes are the payload sizes of a small reply, a typical tool
// output and a large file read.
var benchSizes = []struct {
	name string
	n    int
}{{"small", 512}, {"medium", 10 << 10}, {"large", 200 << 10}}

var benchTime = time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)

// benchAgentMessage is an agent.message payload whose text is n bytes.
func benchAgentMessage(n int) AgentMessage {
	return AgentMessage{
		Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{
			{Type: ir.BlockText, Text: benchProse(n)},
			{Type: ir.BlockToolUse, ToolUse: &lux.ToolUse{ID: "toolu_01A9XkQ2", Name: "read", Args: json.RawMessage(`{"path":"session/event.go","offset":120,"limit":80}`)}},
		}},
		StopReason: ir.StopToolUse,
		Request:    "evt_01JBX5Z8M2Q4R6T8V0W2Y4A6C8",
	}
}

// benchToolResult is a tool.result payload whose output is n bytes.
func benchToolResult(n int) ToolResult {
	return ToolResult{
		ToolUseID:  "toolu_01A9XkQ2",
		Content:    []lux.Block{{Type: ir.BlockText, Text: benchProse(n)}},
		Outcome:    "ok",
		DurationMS: 42,
		Meta:       json.RawMessage(`{"sha256":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","cwd":"/work/topos"}`),
	}
}

// benchEvent is the event the log holds for payload p.
func benchEvent(b *testing.B, typ Type, p any) Event {
	b.Helper()
	e, err := NewEvent(typ, p, benchTime)
	if err != nil {
		b.Fatal(err)
	}
	e.ID = "evt_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
	e.Seq = 4217
	e.SessionID = "ses_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
	e.Turn, e.Step = 12, 3
	return e
}

// BenchmarkJSONEventMarshal is the canonical event encoding every store,
// the SSE stream and the log file share.
func BenchmarkJSONEventMarshal(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			e := benchEvent(b, TypeAgentMessage, benchAgentMessage(s.n))
			enc, err := Marshal(e)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(enc)))
			for b.Loop() {
				if _, err := Marshal(e); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONEventUnmarshal is reading a stored or streamed event back.
func BenchmarkJSONEventUnmarshal(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			enc, err := Marshal(benchEvent(b, TypeAgentMessage, benchAgentMessage(s.n)))
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(enc)))
			for b.Loop() {
				var e Event
				if err := json.Unmarshal(enc, &e); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONNewEventMarshal is the harness appending a tool.result: the
// payload encoded in the canonical form.
func BenchmarkJSONNewEventMarshal(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			p := benchToolResult(s.n)
			e := benchEvent(b, TypeToolResult, p)
			b.ReportAllocs()
			b.SetBytes(int64(len(e.Payload)))
			for b.Loop() {
				if _, err := NewEvent(TypeToolResult, p, benchTime); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONEventPayloadDecode is the fold reading an agent.message
// payload out of the log.
func BenchmarkJSONEventPayloadDecode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			e := benchEvent(b, TypeAgentMessage, benchAgentMessage(s.n))
			b.ReportAllocs()
			b.SetBytes(int64(len(e.Payload)))
			for b.Loop() {
				var p AgentMessage
				if err := e.Decode(&p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONToolResultDecode is the fold reading a tool.result payload,
// the largest payload a session log holds.
func BenchmarkJSONToolResultDecode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			e := benchEvent(b, TypeToolResult, benchToolResult(s.n))
			b.ReportAllocs()
			b.SetBytes(int64(len(e.Payload)))
			for b.Loop() {
				var p ToolResult
				if err := e.Decode(&p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONEventRedactedCheck is the tombstone probe every redaction
// aware reader runs on each event it folds.
func BenchmarkJSONEventRedactedCheck(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			e := benchEvent(b, TypeToolResult, benchToolResult(s.n))
			b.ReportAllocs()
			b.SetBytes(int64(len(e.Payload)))
			for b.Loop() {
				if e.Redacted() {
					b.Fatal("redacted")
				}
			}
		})
	}
}

// BenchmarkJSONDeltaMarshal is one live delta frame of a streaming
// response, the most frequent encode of a running session.
func BenchmarkJSONDeltaMarshal(b *testing.B) {
	d := Delta{Turn: 12, Step: 3, Block: 0, Kind: DeltaText, Text: benchProse(MaxDeltaText / 4)}
	enc, err := Marshal(d)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		if _, err := Marshal(d); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONSessionMarshal is the session header the stores write and
// the list endpoint returns.
func BenchmarkJSONSessionMarshal(b *testing.B) {
	s := Session{ID: "ses_01JBX5Z8M2Q4R6T8V0W2Y4A6C8", Status: StatusIdle, CreatedAt: benchTime, UpdatedAt: benchTime, ExpiresAt: benchTime.Add(DefaultMaxAge),
		Limits: Limits{TurnTimeout: DefaultTurnTimeout.String(), MaxAge: DefaultMaxAge.String()}}
	enc, err := Marshal(s)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		if _, err := Marshal(s); err != nil {
			b.Fatal(err)
		}
	}
}
