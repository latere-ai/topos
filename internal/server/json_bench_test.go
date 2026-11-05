// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

var benchTime = time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC)

// benchProse returns n bytes of deterministic text with characters JSON
// escapes.
func benchProse(n int) string {
	const unit = "The runner reads \"main.go\", finds func main() and wires the store.\n\tIt then appends the batch & returns <ok> to the caller. "
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}

var benchSizes = []struct {
	name string
	n    int
}{{"small", 256}, {"medium", 10 << 10}, {"large", 200 << 10}}

// benchWriter is a response writer that keeps the body and nothing else.
type benchWriter struct {
	h    http.Header
	body bytes.Buffer
}

func (w *benchWriter) Header() http.Header         { return w.h }
func (w *benchWriter) WriteHeader(int)             {}
func (w *benchWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

// benchToolResultEvent is a tool.result event whose output is n bytes.
func benchToolResultEvent(b *testing.B, seq uint64, n int) session.Event {
	b.Helper()
	e, err := session.NewEvent(session.TypeToolResult, session.ToolResult{
		ToolUseID:  "toolu_01A9XkQ2",
		Content:    []lux.Block{{Type: ir.BlockText, Text: benchProse(n)}},
		Outcome:    "ok",
		DurationMS: 42,
	}, benchTime)
	if err != nil {
		b.Fatal(err)
	}
	e.ID = "evt_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
	e.SessionID = "ses_01JBX5Z8M2Q4R6T8V0W2Y4A6C8"
	e.Seq = seq
	e.Turn, e.Step = 12, 3
	return e
}

// BenchmarkJSONSSEFrameEncode is the event stream writing one event as a
// Server-Sent Events frame, once per event per subscriber.
func BenchmarkJSONSSEFrameEncode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			ev := benchToolResultEvent(b, 4217, s.n)
			var buf bytes.Buffer
			if err := frame(&buf, ev); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(buf.Len()))
			for b.Loop() {
				buf.Reset()
				if err := frame(&buf, ev); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONSSEDeltaFrameEncode is the event stream writing one live
// delta of a streaming model response, the most frequent frame.
func BenchmarkJSONSSEDeltaFrameEncode(b *testing.B) {
	d := session.Delta{Turn: 12, Step: 3, Block: 0, Kind: session.DeltaText, Text: benchProse(300)}
	var buf bytes.Buffer
	if err := deltaFrame(&buf, d); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(buf.Len()))
	for b.Loop() {
		buf.Reset()
		if err := deltaFrame(&buf, d); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkJSONEventPageEncode is GET /sessions/{id}/events: the handler's
// page of events rendered through the API answer, 1, 10 and 50 events.
func BenchmarkJSONEventPageEncode(b *testing.B) {
	for _, s := range []struct {
		name         string
		events, size int
	}{{"small", 1, 256}, {"medium", 10, 1 << 10}, {"large", 50, 4 << 10}} {
		b.Run(s.name, func(b *testing.B) {
			evs := make([]session.Event, s.events)
			for i := range evs {
				evs[i] = benchToolResultEvent(b, uint64(i+1), s.size)
			}
			w := &benchWriter{h: http.Header{}}
			c := &call{w: w, rt: &route{status: http.StatusOK}}
			if err := c.replyPage(evs, ""); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(w.body.Len()))
			for b.Loop() {
				w.body.Reset()
				if err := c.replyPage(evs, ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONSendEventDecode is POST /sessions/{id}/events carrying a
// user.message: the request body decoded and its payload decoded strictly.
func BenchmarkJSONSendEventDecode(b *testing.B) {
	for _, s := range benchSizes {
		b.Run(s.name, func(b *testing.B) {
			payload, err := json.Marshal(messageBody{Content: []lux.Block{{Type: ir.BlockText, Text: benchProse(s.n)}}})
			if err != nil {
				b.Fatal(err)
			}
			body, err := json.Marshal(sendBody{Type: session.TypeUserMessage, Payload: payload})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				var sb sendBody
				if err := decodeBody(body, &sb); err != nil {
					b.Fatal(err)
				}
				var m messageBody
				if err := strict(sb.Payload, &m); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkJSONSessionAnswerEncode is a session answered as the API does,
// the response of create, get and each row of a list.
func BenchmarkJSONSessionAnswerEncode(b *testing.B) {
	sess := session.Session{ID: "ses_01JBX5Z8M2Q4R6T8V0W2Y4A6C8", Status: session.StatusIdle, CreatedAt: benchTime, UpdatedAt: benchTime,
		ExpiresAt: benchTime.Add(session.DefaultMaxAge),
		Limits:    session.Limits{TurnTimeout: session.DefaultTurnTimeout.String(), MaxAge: session.DefaultMaxAge.String()}}
	w := &benchWriter{h: http.Header{}}
	c := &call{w: w, rt: &route{status: http.StatusOK}}
	if err := c.reply(http.StatusOK, sess); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(w.body.Len()))
	for b.Loop() {
		w.body.Reset()
		if err := c.reply(http.StatusOK, sess); err != nil {
			b.Fatal(err)
		}
	}
}
