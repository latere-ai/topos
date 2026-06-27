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

// Full fixed event-name set (spec: harness-hook-bus.md §Acceptance Criteria).
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
