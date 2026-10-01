// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// Accumulator builds the IR response a stream of IR events describes.
//
// A tool_use block's arguments are the concatenation of its args
// deltas, settled into ToolUse.Args when the block stops and again when
// the message stops. Settling keeps the response encodable whatever the
// model sent: no arguments are the empty object, and text that is not
// one JSON value becomes the empty object too, with the text kept by
// tool use ID in InvalidArgs. A model can break off inside a string
// argument at its output limit, and a provider can report that stop as
// tool_calls, so such text reaches here as an ordinary tool call.
type Accumulator struct {
	resp    ir.Response
	args    map[int]*strings.Builder
	head    map[int]json.RawMessage
	open    map[int]bool
	invalid map[string]string
	stopped bool
}

// Stopped reports whether the stream's terminal event arrived.
func (a *Accumulator) Stopped() bool { return a.stopped }

// Response is the response so far.
func (a *Accumulator) Response() *ir.Response { return &a.resp }

// InvalidArgs is the argument text of each settled tool call that was
// not one JSON value, by tool use ID; the response holds such a call
// with the input {}. It is nil when every call's arguments were JSON.
func (a *Accumulator) InvalidArgs() map[string]string {
	if len(a.invalid) == 0 {
		return nil
	}
	return maps.Clone(a.invalid)
}

// Add folds one event into the response. It refuses an event the stream
// grammar does not allow at that point, with one exception: arguments
// for a tool_use block that already stopped are appended to it.
func (a *Accumulator) Add(ev ir.Event) error {
	if a.stopped {
		return fmt.Errorf("models: event %s after message_stop", ev.Type)
	}
	switch ev.Type {
	case ir.EventMessageStart:
		a.resp.ID, a.resp.Model = ev.ID, ev.Model
		mergeUsage(&a.resp.Usage, ev.Usage)
	case ir.EventBlockStart:
		if ev.Block == nil {
			return fmt.Errorf("models: block_start %d carries no block", ev.Index)
		}
		for len(a.resp.Blocks) <= ev.Index {
			a.resp.Blocks = append(a.resp.Blocks, ir.Block{})
		}
		b := *ev.Block
		if b.ToolUse != nil {
			tu := *b.ToolUse
			b.ToolUse = &tu
			if len(tu.Args) > 0 {
				if a.head == nil {
					a.head = map[int]json.RawMessage{}
				}
				a.head[ev.Index] = tu.Args
			}
		}
		a.resp.Blocks[ev.Index] = b
		if a.open == nil {
			a.open = map[int]bool{}
		}
		a.open[ev.Index] = true
	case ir.EventTextDelta, ir.EventThinkingDelta, ir.EventSignatureDelta, ir.EventArgsDelta:
		// Arguments for a tool_use block that already stopped are taken:
		// a decoder that closes a call's block when the next call begins
		// sends the rest of interleaved parallel calls that way, and the
		// message's stop settles them.
		if !a.open[ev.Index] && (ev.Type != ir.EventArgsDelta || !a.toolUse(ev.Index)) {
			return fmt.Errorf("models: %s for block %d, which is not open", ev.Type, ev.Index)
		}
		b := &a.resp.Blocks[ev.Index]
		switch ev.Type {
		case ir.EventTextDelta, ir.EventThinkingDelta:
			b.Text += ev.Delta
		case ir.EventSignatureDelta:
			b.Signature += ev.Delta
		case ir.EventArgsDelta:
			if a.args == nil {
				a.args = map[int]*strings.Builder{}
			}
			if a.args[ev.Index] == nil {
				a.args[ev.Index] = &strings.Builder{}
			}
			a.args[ev.Index].WriteString(ev.Delta)
		}
	case ir.EventBlockStop:
		if !a.open[ev.Index] {
			return fmt.Errorf("models: block_stop for block %d, which is not open", ev.Index)
		}
		delete(a.open, ev.Index)
		a.settle(ev.Index)
	case ir.EventMessageDelta:
		if ev.StopReason != "" {
			a.resp.StopReason = ev.StopReason
		}
		if ev.StopSequence != "" {
			a.resp.StopSequence = ev.StopSequence
		}
		mergeUsage(&a.resp.Usage, ev.Usage)
	case ir.EventMessageStop:
		// The message's end closes every block still open, and settles
		// every call again with the arguments that arrived after its
		// block stopped.
		clear(a.open)
		for i := range a.resp.Blocks {
			a.settle(i)
		}
		a.stopped = true
	default:
		return fmt.Errorf("models: unknown stream event %q", ev.Type)
	}
	return nil
}

// toolUse reports whether block i is a tool_use block.
func (a *Accumulator) toolUse(i int) bool {
	return i >= 0 && i < len(a.resp.Blocks) && a.resp.Blocks[i].Type == ir.BlockToolUse && a.resp.Blocks[i].ToolUse != nil
}

// settle sets a tool_use block's Args from the deltas it received, or
// from its start when none arrived: no arguments are {}, one JSON value
// is kept as it is, and any other text is {} with the text recorded in
// invalid.
func (a *Accumulator) settle(i int) {
	if !a.toolUse(i) {
		return
	}
	tu := a.resp.Blocks[i].ToolUse
	raw := []byte(a.head[i])
	if sb := a.args[i]; sb != nil && sb.Len() > 0 {
		raw = []byte(sb.String())
	}
	delete(a.invalid, tu.ID)
	switch {
	case len(bytes.TrimSpace(raw)) == 0:
		tu.Args = json.RawMessage(`{}`)
	case json.Valid(raw):
		tu.Args = json.RawMessage(raw)
	default:
		tu.Args = json.RawMessage(`{}`)
		if a.invalid == nil {
			a.invalid = map[string]string{}
		}
		a.invalid[tu.ID] = string(raw)
	}
}

// mergeUsage keeps the latest figure of each count a stream reports.
func mergeUsage(dst *ir.Usage, src *ir.Usage) {
	if src == nil {
		return
	}
	if src.InputTokens > 0 {
		dst.InputTokens = src.InputTokens
	}
	if src.OutputTokens > 0 {
		dst.OutputTokens = src.OutputTokens
	}
	if src.ReasoningTokens > 0 {
		dst.ReasoningTokens = src.ReasoningTokens
	}
	if src.CacheReadInputTokens != nil {
		dst.CacheReadInputTokens = src.CacheReadInputTokens
	}
	if src.CacheWriteInputTokens != nil {
		dst.CacheWriteInputTokens = src.CacheWriteInputTokens
	}
	if src.CostUSDMicro != nil {
		dst.CostUSDMicro = src.CostUSDMicro
	}
}

// LuxMessage encodes an IR response as the Lux wire assistant message and
// usage through the lux codec, so the log holds exactly what the Lux
// dialect writes.
func LuxMessage(r *ir.Response) (lux.Message, lux.Usage, error) {
	b, err := (&lux.Frontend{}).EncodeResponse(r)
	if err != nil {
		return lux.Message{}, lux.Usage{}, fmt.Errorf("models: encode the response as Lux wire: %w", err)
	}
	var w lux.Response
	if err := json.Unmarshal(b, &w); err != nil {
		return lux.Message{}, lux.Usage{}, fmt.Errorf("models: read the Lux wire response: %w", err)
	}
	return lux.Message{Role: ir.RoleAssistant, Blocks: w.Blocks}, w.Usage, nil
}
