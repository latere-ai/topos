// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"encoding/json"
	"time"
)

// SchemaVersion is the schema of every Session and Event this package
// writes.
const SchemaVersion = 1

// Status is a session's state (spec 004).
type Status string

// The three statuses. A waiting session is StatusIdle with a stop reason.
const (
	StatusIdle    Status = "idle"
	StatusRunning Status = "running"
	StatusEnded   Status = "ended"
)

// StopReason says why a session is idle or ended.
type StopReason string

// Stop reasons of an idle session.
const (
	StopEndTurn          StopReason = "end_turn"
	StopToolConfirmation StopReason = "tool_confirmation"
	StopToolResult       StopReason = "tool_result"
	StopBudget           StopReason = "budget"
	StopTurnLimit        StopReason = "turn_limit"
	StopOutputLimit      StopReason = "output_limit"
	StopInterrupted      StopReason = "interrupted"
	StopError            StopReason = "error"
)

// Stop reasons of an ended session.
const (
	StopCompleted StopReason = "completed"
	StopFailed    StopReason = "failed"
	StopCanceled  StopReason = "canceled"
	StopExpired   StopReason = "expired"
)

// Runner kinds: where a session's writer lives.
const (
	RunnerHosted   = "hosted"
	RunnerExternal = "external"
)

// Sender is whoever wrote a user event or created a session.
type Sender struct {
	Subject string `json:"subject"`
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind"`
}

// The kinds of Sender.
const (
	SenderPerson  = "person"
	SenderTrigger = "trigger"
	SenderService = "service"
)

// AgentRef names the agent version a session runs.
type AgentRef struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Version int    `json:"version"`
	Digest  Digest `json:"digest,omitempty"`
	// Bundle is the digest of the blob holding the agent and the agents
	// it pins, which a runner rebuilds the agent from.
	Bundle Digest `json:"bundle,omitempty"`
}

// Writer is who may append to a session now.
type Writer struct {
	Kind    string    `json:"kind"`
	Subject string    `json:"subject,omitempty"`
	Since   time.Time `json:"since"`
}

// Machine is the machine a session asks for (spec 009).
type Machine struct {
	Kind        string `json:"kind"`
	Environment string `json:"environment,omitempty"`
	Image       string `json:"image,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
}

// Machine kinds.
const (
	MachineHost  = "host"
	MachineCella = "cella"
)

// Resource is one attachment of a session: a memory store or a
// repository.
type Resource struct {
	Type          string `json:"type"`
	MemoryStoreID string `json:"memory_store_id,omitempty"`
	Access        string `json:"access,omitempty"`
	URL           string `json:"url,omitempty"`
	Ref           string `json:"ref,omitempty"`
}

// Budget is a session's spend ceiling and its spend so far, in millionths
// of a USD. A nil MaxCostUSDMicro means no ceiling.
type Budget struct {
	MaxCostUSDMicro   *int64 `json:"max_cost_usd_micro,omitempty"`
	SpentCostUSDMicro int64  `json:"spent_cost_usd_micro"`
}

// Limits are a session's wall-clock bounds, as Go durations.
type Limits struct {
	TurnTimeout string `json:"turn_timeout,omitempty"`
	MaxAge      string `json:"max_age,omitempty"`
	// Retention is how long the session is kept after it ends; empty
	// keeps it until it is deleted (spec 014).
	Retention string `json:"retention,omitempty"`
}

// The default limits of spec 004.
const (
	DefaultTurnTimeout = 2 * time.Hour
	DefaultMaxAge      = 168 * time.Hour
)

// Capture says which raw wire data a session keeps beyond the default.
type Capture struct {
	Requests bool `json:"requests,omitempty"`
}

// Parent names the session and sequence a forked session started from.
type Parent struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
}

// Session is the header of a session. Its status mirrors the last
// session.status event of its log.
type Session struct {
	Schema     int               `json:"schema"`
	ID         string            `json:"id"`
	Agent      AgentRef          `json:"agent"`
	Title      string            `json:"title,omitempty"`
	Initiator  Sender            `json:"initiator"`
	Runner     string            `json:"runner"`
	Writer     *Writer           `json:"writer,omitempty"`
	Status     Status            `json:"status"`
	StopReason StopReason        `json:"stop_reason,omitempty"`
	Turn       int               `json:"turn"`
	LastSeq    uint64            `json:"last_seq"`
	Machine    Machine           `json:"machine"`
	Resources  []Resource        `json:"resources,omitempty"`
	Scope      []json.RawMessage `json:"scope,omitempty"`
	Budget     Budget            `json:"budget"`
	Limits     Limits            `json:"limits"`
	Capture    Capture           `json:"capture"`
	EndOnIdle  bool              `json:"end_on_idle,omitempty"`
	Parent     *Parent           `json:"parent,omitempty"`
	TriggerID  string            `json:"trigger_id,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedAt  time.Time         `json:"updated_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// MaxMetadata is the most entries a session's metadata holds.
const MaxMetadata = 32

// New returns a Session with the schema, a fresh id, the given agent and
// initiator, status idle, and the default limits applied from now.
func New(agent AgentRef, initiator Sender, runner string, m Machine, now time.Time) Session {
	now = now.UTC()
	return Session{
		Schema:    SchemaVersion,
		ID:        NewID(PrefixSession),
		Agent:     agent,
		Initiator: initiator,
		Runner:    runner,
		Status:    StatusIdle,
		Machine:   m,
		Limits:    Limits{TurnTimeout: DefaultTurnTimeout.String(), MaxAge: DefaultMaxAge.String()},
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(DefaultMaxAge),
	}
}

// Marshal renders v in the one byte form spec 004 fixes: encoding/json
// with HTML escaping off, fields in declaration order, no trailing
// newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
