// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// Accumulator builds the IR response a stream of IR events describes.
type Accumulator struct {
	resp    ir.Response
	args    map[int]*strings.Builder
	open    map[int]bool
	stopped bool
}

// Stopped reports whether the stream's terminal event arrived.
func (a *Accumulator) Stopped() bool { return a.stopped }

// Response is the response so far.
func (a *Accumulator) Response() *ir.Response { return &a.resp }

// Add folds one event into the response. It refuses an event the stream
// grammar does not allow at that point.
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
		}
		a.resp.Blocks[ev.Index] = b
		if a.open == nil {
			a.open = map[int]bool{}
		}
		a.open[ev.Index] = true
	case ir.EventTextDelta, ir.EventThinkingDelta, ir.EventSignatureDelta, ir.EventArgsDelta:
		if !a.open[ev.Index] {
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
		b := &a.resp.Blocks[ev.Index]
		if b.Type == ir.BlockToolUse && b.ToolUse != nil {
			if sb := a.args[ev.Index]; sb != nil && sb.Len() > 0 {
				b.ToolUse.Args = json.RawMessage(sb.String())
			} else if len(b.ToolUse.Args) == 0 {
				b.ToolUse.Args = json.RawMessage(`{}`)
			}
		}
	case ir.EventMessageDelta:
		if ev.StopReason != "" {
			a.resp.StopReason = ev.StopReason
		}
		if ev.StopSequence != "" {
			a.resp.StopSequence = ev.StopSequence
		}
		mergeUsage(&a.resp.Usage, ev.Usage)
	case ir.EventMessageStop:
		a.stopped = true
	default:
		return fmt.Errorf("models: unknown stream event %q", ev.Type)
	}
	return nil
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
