// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package lux adapts the Lux gateway's native dialect to the
// [models.Model] seam via luxsdk (lux spec 33). One adapter reaches
// every provider Lux routes — the gateway owns the per-provider
// translation, so this package replaces the per-provider adapter
// roadmap (OpenAI, Gemini) that models/model.go once deferred to
// follow-up specs.
//
// Event mapping: the lux stream grammar is the gateway IR's, so the
// translation to [models.Event] is mechanical. text_delta →
// KindTextDelta; args_delta → KindToolCallDelta with per-index
// assembly and KindToolCallDone at the closing block_stop; usage on
// message_start / message_delta → KindUsage (consumers accumulate);
// message_stop → KindDone with the stop reason carried by
// message_delta. Frames with no normalized counterpart (thinking and
// signature deltas, structural block frames) surface as
// [models.KindProviderEvent] — observable, never decision-bearing.
package lux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"latere.ai/x/pkg/luxsdk"

	"latere.ai/x/topos/models"
)

const (
	defaultBaseURL = "https://lux.latere.ai"

	// defaultModel matches the anthropic adapter's default so swapping
	// ModelKind alone keeps behavior.
	defaultModel = "claude-opus-4-8"
)

// Adapter implements [models.Model] over any [luxsdk.Caller]: the
// gateway client (default) or a provider-direct caller.
type Adapter struct {
	client luxsdk.Caller
	model  string
}

// Option configures the Adapter.
type Option func(*Adapter, *[]luxsdk.Option)

// WithModel overrides the default model id.
func WithModel(model string) Option {
	return func(a *Adapter, _ *[]luxsdk.Option) { a.model = model }
}

// WithBearerSource supplies a per-call bearer (a rotating JWT) instead
// of a static key.
func WithBearerSource(fn func(ctx context.Context) (string, error)) Option {
	return func(_ *Adapter, sdkOpts *[]luxsdk.Option) {
		*sdkOpts = append(*sdkOpts, luxsdk.WithTokenSource(tokenFunc(fn)))
	}
}

type tokenFunc func(ctx context.Context) (string, error)

func (f tokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// New builds an Adapter for the Lux deployment at baseURL (the
// gateway root, e.g. "https://lux.latere.ai"). apiKey is a Lux
// virtual key; leave it empty when a [WithBearerSource] option
// supplies the credential.
func New(apiKey, baseURL string, opts ...Option) *Adapter {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	a := &Adapter{model: defaultModel}
	sdkOpts := []luxsdk.Option{}
	if apiKey != "" {
		sdkOpts = append(sdkOpts, luxsdk.WithAPIKey(apiKey))
	}
	for _, o := range opts {
		o(a, &sdkOpts)
	}
	a.client = luxsdk.New(baseURL, sdkOpts...)
	return a
}

// NewFromCaller wraps an already-built [luxsdk.Caller] — e.g. a
// [luxsdk.Direct] for BYO-key provider access. SDK-level options
// passed here are ignored; configure them on the caller.
func NewFromCaller(c luxsdk.Caller, opts ...Option) *Adapter {
	a := &Adapter{model: defaultModel, client: c}
	var discard []luxsdk.Option
	for _, o := range opts {
		o(a, &discard)
	}
	return a
}

// Model returns the model id the adapter will request. Exposed for
// wiring/observability.
func (a *Adapter) Model() string { return a.model }

// Stream implements [models.Model].
func (a *Adapter) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	wire, err := a.buildRequest(req)
	if err != nil {
		return nil, fmt.Errorf("lux: build request: %w", err)
	}
	st, err := a.client.Stream(ctx, wire)
	if err != nil {
		return nil, fmt.Errorf("lux: %w", err)
	}
	return newStream(st), nil
}

func (a *Adapter) buildRequest(req models.Request) (*luxsdk.Request, error) {
	maxTokens := int64(req.MaxTokens)
	if maxTokens == 0 {
		maxTokens = 8192 // safe default, matching the anthropic adapter
	}
	wire := &luxsdk.Request{
		Model:     a.model,
		MaxTokens: &maxTokens,
	}
	if req.System != "" {
		// CacheHint marks the prompt-cache breakpoint after the system
		// block; backends without caching report it as loss, not error.
		wire.System = []luxsdk.Block{{Type: luxsdk.BlockText, Text: req.System, CacheHint: true}}
	}
	if req.Temperature != 0 {
		t := req.Temperature
		wire.Temperature = &t
	}
	if req.ThinkingBudget > 0 {
		wire.Reasoning = &luxsdk.Reasoning{BudgetTokens: int64(req.ThinkingBudget)}
	}
	for _, td := range req.Tools {
		schema := td.InputSchema
		if schema == nil {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		wire.Tools = append(wire.Tools, luxsdk.Tool{
			Name:        td.Name,
			Description: td.Description,
			InputSchema: schema,
		})
	}
	for _, m := range req.Messages {
		wm, err := messageToWire(m)
		if err != nil {
			return nil, err
		}
		wire.Messages = append(wire.Messages, wm)
	}
	return wire, nil
}

// messageToWire converts a canonical [models.Message] to the lux wire
// shape. Same role mapping as the anthropic adapter: "tool" becomes a
// user turn carrying tool_result blocks (the IR keeps the two-role
// model).
func messageToWire(m models.Message) (luxsdk.Message, error) {
	switch m.Role {
	case models.RoleUser:
		return luxsdk.UserText(m.Content), nil

	case models.RoleAssistant:
		var blocks []luxsdk.Block
		if m.Content != "" {
			blocks = append(blocks, luxsdk.Block{Type: luxsdk.BlockText, Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			input := tc.Input
			if input == nil {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, luxsdk.Block{
				Type:    luxsdk.BlockToolUse,
				ToolUse: &luxsdk.ToolUse{ID: tc.ID, Name: tc.Name, Args: input},
			})
		}
		if len(blocks) == 0 {
			blocks = []luxsdk.Block{{Type: luxsdk.BlockText, Text: ""}}
		}
		return luxsdk.Message{Role: luxsdk.RoleAssistant, Blocks: blocks}, nil

	case models.RoleTool:
		var blocks []luxsdk.Block
		for _, tr := range m.ToolResults {
			blocks = append(blocks, luxsdk.Block{
				Type: luxsdk.BlockToolResult,
				ToolResult: &luxsdk.ToolResult{
					ToolUseID: tr.CallID,
					Blocks:    []luxsdk.Block{{Type: luxsdk.BlockText, Text: tr.Content}},
					IsError:   tr.IsError,
				},
			})
		}
		if len(blocks) == 0 {
			return luxsdk.Message{}, fmt.Errorf("lux: tool message has no ToolResults")
		}
		return luxsdk.Message{Role: luxsdk.RoleUser, Blocks: blocks}, nil

	default:
		return luxsdk.Message{}, fmt.Errorf("lux: unknown message role %q", m.Role)
	}
}

// stream adapts a luxsdk stream to [models.Stream].
type stream struct {
	src  *luxsdk.Stream
	mu   sync.Mutex
	done bool

	// Per content-block state: index → in-progress tool call.
	toolBlocks map[int]*toolBlock

	// pendingStopReason is set by message_delta and consumed by
	// message_stop.
	pendingStopReason models.StopReason
}

type toolBlock struct {
	id    string
	name  string
	input strings.Builder
}

func newStream(src *luxsdk.Stream) *stream {
	return &stream{src: src, toolBlocks: make(map[int]*toolBlock)}
}

// Recv implements [models.Stream]. After the KindDone event, the next
// call returns io.EOF.
func (s *stream) Recv() (models.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return models.Event{}, io.EOF
	}
	for {
		ev, err := s.src.Next()
		if err != nil {
			// io.EOF included: a stream that ends without message_stop
			// surfaces as-is; the loop treats it as truncation.
			return models.Event{}, err
		}
		out, ok := s.mapEvent(ev)
		if !ok {
			continue
		}
		if out.Kind == models.KindDone {
			s.done = true
		}
		return out, nil
	}
}

// mapEvent translates one lux wire event. ok=false means the frame
// was consumed as state (nothing to surface this iteration).
func (s *stream) mapEvent(ev luxsdk.Event) (models.Event, bool) {
	switch ev.Type {
	case luxsdk.EventMessageStart:
		if ev.Usage == nil {
			return providerEvent(ev)
		}
		// Input-side fields only; consumers accumulate KindUsage.
		return models.Event{Kind: models.KindUsage, Usage: usageFromWire(*ev.Usage)}, true

	case luxsdk.EventBlockStart:
		if ev.Block != nil && ev.Block.Type == luxsdk.BlockToolUse && ev.Block.ToolUse != nil {
			s.toolBlocks[ev.Index] = &toolBlock{id: ev.Block.ToolUse.ID, name: ev.Block.ToolUse.Name}
		}
		return providerEvent(ev)

	case luxsdk.EventTextDelta:
		return models.Event{Kind: models.KindTextDelta, TextDelta: ev.Delta}, true

	case luxsdk.EventArgsDelta:
		if tb := s.toolBlocks[ev.Index]; tb != nil {
			tb.input.WriteString(ev.Delta)
		}
		return models.Event{Kind: models.KindToolCallDelta, ToolCallIndex: ev.Index, ToolCallDelta: ev.Delta}, true

	case luxsdk.EventBlockStop:
		tb, ok := s.toolBlocks[ev.Index]
		if !ok {
			return providerEvent(ev)
		}
		delete(s.toolBlocks, ev.Index)
		input := tb.input.String()
		if input == "" {
			input = "{}"
		}
		return models.Event{
			Kind:     models.KindToolCallDone,
			ToolCall: &models.ToolCall{ID: tb.id, Name: tb.name, Input: json.RawMessage(input)},
		}, true

	case luxsdk.EventMessageDelta:
		s.pendingStopReason = models.StopReason(ev.StopReason)
		if ev.Usage == nil {
			return models.Event{}, false
		}
		return models.Event{Kind: models.KindUsage, Usage: usageFromWire(*ev.Usage)}, true

	case luxsdk.EventMessageStop:
		return models.Event{Kind: models.KindDone, StopReason: s.pendingStopReason}, true

	default:
		// thinking_delta, signature_delta, and anything the wire grows
		// later: observable, never decision-bearing.
		return providerEvent(ev)
	}
}

func providerEvent(ev luxsdk.Event) (models.Event, bool) {
	raw, err := json.Marshal(ev)
	if err != nil {
		raw = nil
	}
	return models.Event{
		Kind:          models.KindProviderEvent,
		ProviderEvent: &models.ProviderEvent{Type: string(ev.Type), Raw: raw},
	}, true
}

func usageFromWire(u luxsdk.Usage) *models.Usage {
	return &models.Usage{
		InputTokens:      int(u.InputTokens),
		OutputTokens:     int(u.OutputTokens),
		CacheReadTokens:  int(u.CacheReadInputTokens),
		CacheWriteTokens: int(u.CacheWriteInputTokens),
	}
}

// Close implements [models.Stream].
func (s *stream) Close() error { return s.src.Close() }
