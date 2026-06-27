// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package models defines the provider-agnostic chat-completion surface for
// the Topos agentic loop.
//
// A [Model] receives a [Request] and returns a [Stream] of [Event]s. All
// provider-specific wire shapes (Anthropic Messages API, OpenAI Chat
// Completions, Gemini GenerateContent) are absorbed in adapters; the agentic
// loop speaks only the types defined here.
//
// Tool contract: [ToolDef] carries the canonical tool definition
// (name + description + JSON Schema), [ToolCall] carries a call emitted by
// the model, and [ToolResult] carries the caller's response. Adapters
// down-convert to and from each provider's wire encoding; the loop never sees
// provider shapes.
//
// Streaming: [Stream.Recv] delivers [Event]s until it returns [io.EOF].
// Each event has a [EventKind] discriminator; inspect [Event.Kind] before
// reading the payload field. Provider-native events that have no normalized
// counterpart surface as [KindProviderEvent] so nothing is silently dropped,
// but they carry no control semantics.
package models

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrToolsUnsupported is returned (wrapped) by a [Model.Stream] implementation
// when the request carried tools but the provider rejected tool use (e.g. some
// Ollama models that return HTTP 400 "does not support tools"). Callers may
// detect it with [errors.Is] and retry the request without tools to obtain a
// plain-text response.
var ErrToolsUnsupported = errors.New("models: model does not support tool calls")

// Model is the single abstraction boundary between the agentic loop and any
// model backend. Implementations MUST be safe for concurrent use.
type Model interface {
	// Stream sends req to the model and returns a Stream of Events. The
	// caller is responsible for calling Stream.Close when done.
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Request is the provider-agnostic input to a chat-completion call.
type Request struct {
	// System is the system prompt, injected before the message history.
	System string

	// Messages is the ordered conversation history. Roles are "user",
	// "assistant", and "tool".
	Messages []Message

	// Tools is the set of tools the model may call during this turn.
	Tools []ToolDef

	// MaxTokens caps the number of tokens the model may generate.
	MaxTokens int

	// Temperature controls output randomness. 0 means deterministic (or the
	// provider default). Most providers clamp to [0, 1] or [0, 2].
	Temperature float64

	// ThinkingBudget is the extended thinking token budget (Anthropic) or an
	// analogous reasoning budget for other providers. Zero disables it.
	ThinkingBudget int

	// ProviderOptions carries provider-specific request parameters that do
	// not have a normalised counterpart. Keys and values are
	// provider-defined; callers must document which provider they target.
	ProviderOptions map[string]any
}

// Message is one turn in the conversation history.
type Message struct {
	// Role is "user", "assistant", or "tool".
	Role string

	// Content is the text content of the message. For tool-role messages
	// this is the result text; structured data is in ToolResults.
	Content string

	// ToolCalls is populated for assistant-role messages that invoked tools.
	ToolCalls []ToolCall

	// ToolResults is populated for tool-role messages returning results.
	ToolResults []ToolResult
}

// ToolDef is the canonical tool definition passed to the model.
type ToolDef struct {
	// Name is the tool identifier. Must be stable; models encode it in
	// ToolCall.Name.
	Name string

	// Description is the human-readable description used by the model to
	// decide when to call the tool.
	Description string

	// InputSchema is the JSON Schema (object) that describes the tool's
	// input parameters. Adapters pass this verbatim to the provider wire
	// format without re-encoding.
	InputSchema json.RawMessage
}

// ToolCall is a tool invocation emitted by the model.
type ToolCall struct {
	// ID is the call identifier assigned by the model. Must be echoed back
	// in the corresponding ToolResult.CallID.
	ID string

	// Name is the tool name from ToolDef.
	Name string

	// Input is the raw JSON object of the tool's input parameters, as
	// assembled from (possibly streamed) deltas.
	Input json.RawMessage
}

// ToolResult is the caller's response to a ToolCall.
type ToolResult struct {
	// CallID must match the ToolCall.ID this result responds to.
	CallID string

	// Content is the result text returned to the model.
	Content string

	// IsError marks the result as an error; some providers surface this to
	// the model differently.
	IsError bool
}

// EventKind is the discriminator for [Event].
type EventKind string

const (
	// KindTextDelta carries a partial text fragment from the model.
	KindTextDelta EventKind = "text_delta"

	// KindToolCallDelta carries a partial tool-input JSON fragment. Index
	// identifies which tool call is being streamed (0-based, matching the
	// order of content blocks from the provider).
	KindToolCallDelta EventKind = "tool_call_delta"

	// KindToolCallDone signals that a tool call is complete. The fully
	// assembled ToolCall (with intact Input JSON) is in Event.ToolCall.
	KindToolCallDone EventKind = "tool_call_done"

	// KindUsage delivers token accounting for the turn. May be emitted
	// multiple times (e.g. input usage at the start, output usage at the
	// end); consumers should accumulate.
	KindUsage EventKind = "usage"

	// KindDone signals that the stream has ended. Event.StopReason carries
	// the normalized stop reason.
	KindDone EventKind = "done"

	// KindProviderEvent passes through a provider-native event that has no
	// normalized counterpart (e.g. Anthropic thinking blocks, cache
	// events). These are observational only and carry no control semantics.
	KindProviderEvent EventKind = "provider_event"
)

// Event is one item in the normalized stream returned by [Stream.Recv].
// Inspect [Event.Kind] to determine which payload field is populated.
type Event struct {
	// Kind is the discriminator for this event.
	Kind EventKind

	// TextDelta is populated for KindTextDelta events.
	TextDelta string

	// ToolCallIndex is populated for KindToolCallDelta events and identifies
	// the in-progress tool call by its zero-based content-block index.
	ToolCallIndex int

	// ToolCallDelta is the partial input JSON fragment for KindToolCallDelta
	// events.
	ToolCallDelta string

	// ToolCall is the fully assembled tool call for KindToolCallDone events.
	ToolCall *ToolCall

	// Usage is populated for KindUsage events.
	Usage *Usage

	// StopReason is populated for KindDone events.
	StopReason StopReason

	// ProviderEvent is populated for KindProviderEvent events.
	ProviderEvent *ProviderEvent
}

// Usage carries token accounting for a turn. The fields match the shape
// consumed by ops/budget-cost-billing.
type Usage struct {
	// InputTokens is the number of prompt tokens consumed (excluding cache
	// reads, which are billed separately).
	InputTokens int

	// OutputTokens is the number of completion tokens generated.
	OutputTokens int

	// CacheReadTokens is the number of tokens read from the prompt cache
	// (Anthropic: cache_read_input_tokens).
	CacheReadTokens int

	// CacheWriteTokens is the number of tokens written to the prompt cache
	// (Anthropic: cache_creation_input_tokens).
	CacheWriteTokens int
}

// StopReason is the normalized terminal signal from the model.
type StopReason string

const (
	// StopEndTurn indicates the model completed its response naturally.
	StopEndTurn StopReason = "end_turn"

	// StopToolUse indicates the model stopped to invoke one or more tools.
	StopToolUse StopReason = "tool_use"

	// StopMaxTokens indicates the model was cut off at the MaxTokens limit.
	StopMaxTokens StopReason = "max_tokens"

	// StopSequence indicates the model hit a stop sequence.
	StopSequence StopReason = "stop_sequence"
)

// ProviderEvent carries a raw provider-native event that has no normalized
// counterpart. It is observational only; the agentic loop must not branch on
// its contents.
type ProviderEvent struct {
	// Type is the provider-defined event type (e.g. "thinking",
	// "content_block_start" for Anthropic; provider-specific strings for
	// other backends).
	Type string

	// Raw is the full raw JSON of the provider event, suitable for logging
	// or debugging.
	Raw json.RawMessage
}

// Stream is a handle to an in-progress model completion. The caller must
// call Close when done, even if Recv has returned io.EOF.
type Stream interface {
	// Recv returns the next Event in the stream. Returns (Event{}, io.EOF)
	// after the KindDone event has been delivered. Other errors indicate a
	// transport or protocol failure.
	Recv() (Event, error)

	// Close releases resources associated with the stream, including any
	// in-flight HTTP connection. Safe to call multiple times.
	Close() error
}
