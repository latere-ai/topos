// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package dir

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

var benchTime = time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)

// benchProse returns n bytes of deterministic text with characters JSON
// escapes, as a tool's output has.
func benchProse(n int) string {
	const unit = "The runner reads \"main.go\", finds func main() and wires the store.\n\tIt then appends the batch & returns <ok> to the caller. "
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

// benchBatch is one turn step as the harness appends it: a tool call, its
// result of n bytes and the model's message, with sequences from first.
func benchBatch(b *testing.B, first uint64, n int) []session.Event {
	b.Helper()
	payloads := []struct {
		typ session.Type
		p   any
	}{
		{session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: "toolu_01A9XkQ2", Name: "read", Input: json.RawMessage(`{"path":"session/event.go","offset":120,"limit":80}`), Verdict: "allow"}},
		{session.TypeToolResult, session.ToolResult{ToolUseID: "toolu_01A9XkQ2", Content: []lux.Block{{Type: ir.BlockText, Text: benchProse(n)}}, Outcome: "ok", DurationMS: 42}},
		{session.TypeAgentMessage, session.AgentMessage{Message: lux.Message{Role: ir.RoleAssistant, Blocks: []lux.Block{{Type: ir.BlockText, Text: benchProse(300)}}}, StopReason: ir.StopEndTurn}},
	}
	out := make([]session.Event, len(payloads))
	for i, c := range payloads {
		e, err := session.NewEvent(c.typ, c.p, benchTime)
		if err != nil {
			b.Fatal(err)
		}
		e.ID = "evt_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
		e.SessionID = "ses_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
		e.Seq = first + uint64(i)
		e.Turn, e.Step = 12, 3
		out[i] = e
	}
	return out
}

var benchSizes = []struct {
	name string
	n    int
}{{"small", 100}, {"medium", 9 << 10}, {"large", 200 << 10}}

// BenchmarkJSONLogBatchEncode is the directory store encoding one append:
// the events of a batch as the JSON lines of one write.
func BenchmarkJSONLogBatchEncode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			evs := benchBatch(b, 1, s.n)
			enc, err := encodeBatch(evs)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(enc)))
			for b.Loop() {
				if _, err := encodeBatch(evs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONLogScanDecode is the directory store reading an events file
// back, on every recovery, tail read and watch: the committed lines of a
// session's transcript, here 8 batches.
func BenchmarkJSONLogScanDecode(b *testing.B) {
	for _, s := range benchSizes {
		n := s.n
		if s.name == "large" {
			n = 25 << 10 // eight results of 25 KB make a 200 KB transcript
		}
		b.Run(s.name, func(b *testing.B) {
			var file bytes.Buffer
			for i := range 8 {
				enc, err := encodeBatch(benchBatch(b, uint64(3*i+1), n))
				if err != nil {
					b.Fatal(err)
				}
				file.Write(enc)
			}
			data := file.Bytes()
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				sc, err := scanLog(bytes.NewReader(data))
				if err != nil {
					b.Fatal(err)
				}
				if len(sc.lines) != 24 {
					b.Fatalf("scanned %d lines", len(sc.lines))
				}
			}
		})
	}
}

// BenchmarkJSONLogBatchRoundTrip is one append followed by its read, the
// cost of a durable event in the transcript file.
func BenchmarkJSONLogBatchRoundTrip(b *testing.B) {
	evs := benchBatch(b, 1, 9<<10)
	enc, err := encodeBatch(evs)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	for b.Loop() {
		out, err := encodeBatch(evs)
		if err != nil {
			b.Fatal(err)
		}
		sc, err := scanLog(bytes.NewReader(out))
		if err != nil {
			b.Fatal(err)
		}
		if len(sc.lines) != len(evs) {
			b.Fatalf("scanned %d lines", len(sc.lines))
		}
	}
}
