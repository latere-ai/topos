// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Package hooks defines the Topos hook bus — a typed, ordered, registerable
// event bus that implements the Claude Code hook lifecycle as the extensibility
// contract for the Topos agentic loop.
//
// Every tool call goes through the three-phase path defined in decision.go:
// validate → permission resolution → execute + post-hooks.
//
// Event names are fixed constants (see below). Payload types are versioned
// structs for MVP-used events; others use the generic Payload alias.
package hooks

import (
	"encoding/json"
	"time"

	"latere.ai/x/topos/models"
)

// EventName is a fixed, versioned hook lifecycle event identifier.
// Names are kept verbatim from the Claude Code lifecycle so Topos is a
// drop-in execution space for that mental model.
type EventName string

// Full fixed event-name set.
const (
	// Lifecycle — synchronous / decision-bearing.
	EventSetup               EventName = "Setup"
	EventSessionStart        EventName = "SessionStart"
	EventUserPromptSubmit    EventName = "UserPromptSubmit"
	EventUserPromptExpansion EventName = "UserPromptExpansion"
	EventPreToolUse          EventName = "PreToolUse"
	EventPermissionRequest   EventName = "PermissionRequest"
	EventPostToolUse         EventName = "PostToolUse"
	EventPostToolUseFailure  EventName = "PostToolUseFailure"
	EventPostToolBatch       EventName = "PostToolBatch"
	EventSubagentStart       EventName = "SubagentStart"
	EventSubagentStop        EventName = "SubagentStop"
	EventTaskCreated         EventName = "TaskCreated"
	EventTaskCompleted       EventName = "TaskCompleted"
	EventStop                EventName = "Stop"
	EventStopFailure         EventName = "StopFailure"
	EventTeammateIdle        EventName = "TeammateIdle"
	EventPreCompact          EventName = "PreCompact"
	EventPostCompact         EventName = "PostCompact"
	EventSessionEnd          EventName = "SessionEnd"

	// Async / observational.
	EventNotification       EventName = "Notification"
	EventConfigChange       EventName = "ConfigChange"
	EventInstructionsLoaded EventName = "InstructionsLoaded"
	// EventAssistantMessage carries one turn's assistant text. Observational
	// only (the loop emits it as it completes each turn) so embedders can render
	// a live transcript; it bears no Decision.
	EventAssistantMessage EventName = "AssistantMessage"
	// EventTextDelta carries a single incremental fragment of assistant text as
	// the model streams it, so an embedder can render token-by-token. It is
	// dispatched ephemerally (see Bus.DispatchEphemeral) — delivered to consumers
	// but never recorded in the session event log, because its durable
	// counterpart is the assembled EventAssistantMessage emitted once the turn
	// completes. It is observational only and bears no Decision.
	EventTextDelta EventName = "TextDelta"

	// Environment-reactive.
	EventWorktreeCreate EventName = "WorktreeCreate"
	EventWorktreeRemove EventName = "WorktreeRemove"
	EventCwdChanged     EventName = "CwdChanged"
	EventFileChanged    EventName = "FileChanged"
)

// Payload is the base payload type for events that do not yet have a
// typed schema. Typed events below embed or extend this.
type Payload = map[string]any

// SessionStartPayload is the versioned payload for EventSessionStart.
type SessionStartPayload struct {
	// Version is the payload schema version.
	Version string `json:"version"`
	// SessionID is the opaque session identifier.
	SessionID string `json:"session_id"`
	// AgentID is the agent being run.
	AgentID string `json:"agent_id"`
	// StartedAt is the RFC3339 start timestamp.
	StartedAt time.Time `json:"started_at"`
}

// UserPromptSubmitPayload is the versioned payload for EventUserPromptSubmit.
type UserPromptSubmitPayload struct {
	Version     string    `json:"version"`
	SessionID   string    `json:"session_id"`
	Prompt      string    `json:"prompt"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// PreToolUsePayload is the versioned payload for EventPreToolUse.
// Consumers may mutate NormalisedInput (via Decision.Modify) or deny the call.
type PreToolUsePayload struct {
	Version   string          `json:"version"`
	SessionID string          `json:"session_id"`
	ToolCall  models.ToolCall `json:"tool_call"`
	// NormalisedInput is the backfilled, validated JSON input for the tool.
	// Consumers see this (not the raw model output) to prevent injection attacks.
	NormalisedInput json.RawMessage `json:"normalised_input"`
}

// PostToolUsePayload is the versioned payload for EventPostToolUse.
type PostToolUsePayload struct {
	Version   string            `json:"version"`
	SessionID string            `json:"session_id"`
	ToolCall  models.ToolCall   `json:"tool_call"`
	Result    models.ToolResult `json:"result"`
}

// PostToolUseFailurePayload is the versioned payload for EventPostToolUseFailure.
type PostToolUseFailurePayload struct {
	Version   string          `json:"version"`
	SessionID string          `json:"session_id"`
	ToolCall  models.ToolCall `json:"tool_call"`
	Error     string          `json:"error"`
}

// AssistantMessagePayload is the versioned payload for EventAssistantMessage:
// one completed turn's assistant text, emitted as the loop finishes the turn.
type AssistantMessagePayload struct {
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Text      string `json:"text"`
	Turn      int    `json:"turn"`
}

// EventUsage carries the running token usage after a turn completes, so an
// embedder can render a live cost/usage HUD. Durable (one per turn).
const EventUsage EventName = "Usage"

// UsagePayload is the versioned payload for EventUsage: the just-completed
// turn's usage and the session's running total.
type UsagePayload struct {
	Version   string       `json:"version"`
	SessionID string       `json:"session_id"`
	AgentID   string       `json:"agent_id"`
	Turn      int          `json:"turn"`
	TurnUsage models.Usage `json:"turn_usage"`
	Total     models.Usage `json:"total"`
}

// TextDeltaPayload is the versioned payload for EventTextDelta: one streamed
// fragment of assistant text within a turn. SessionID and AgentID let a consumer
// route the fragment to the right transcript and lineage node; Turn is the
// 1-based turn index the fragment belongs to, so a consumer can group fragments
// into the turn whose assembled AssistantMessage follows.
type TextDeltaPayload struct {
	Version   string `json:"version"`
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Text      string `json:"text"`
	Turn      int    `json:"turn"`
}

// StopPayload is the versioned payload for EventStop.
type StopPayload struct {
	Version    string            `json:"version"`
	SessionID  string            `json:"session_id"`
	StopReason models.StopReason `json:"stop_reason"`
	// ToolCallCount is the total number of tool calls executed this session.
	ToolCallCount int `json:"tool_call_count"`
}

// SessionEndPayload is the versioned payload for EventSessionEnd.
type SessionEndPayload struct {
	Version   string    `json:"version"`
	SessionID string    `json:"session_id"`
	EndedAt   time.Time `json:"ended_at"`
	// FinalReason is the normalised stop reason that ended the session.
	FinalReason models.StopReason `json:"final_reason"`
}
