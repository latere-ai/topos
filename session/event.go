// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"fmt"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"
)

// Type is an event type of spec 004.
type Type string

// Every event type of schema v1.
const (
	TypeUserMessage          Type = "user.message"
	TypeUserInterrupt        Type = "user.interrupt"
	TypeUserToolConfirmation Type = "user.tool_confirmation"
	TypeUserToolResult       Type = "user.tool_result"
	TypeAgentMessage         Type = "agent.message"
	TypeAgentToolUse         Type = "agent.tool_use"
	TypeToolResult           Type = "tool.result"
	TypeThreadStarted        Type = "thread.started"
	TypeThreadEnded          Type = "thread.ended"
	TypeThreadMessage        Type = "thread.message"
	TypeContextCompacted     Type = "context.compacted"
	TypeModelRequest         Type = "model.request"
	TypeSessionStatus        Type = "session.status"
	TypeSessionMachine       Type = "session.machine"
	TypeScopeChanged         Type = "session.scope_changed"
	TypeSessionResumed       Type = "session.resumed"
	TypeSessionError         Type = "session.error"
	TypeMemoryAttached       Type = "memory.attached"
	TypeMemorySynced         Type = "memory.synced"
	TypeEventRedacted        Type = "event.redacted"
	TypeSessionRewound       Type = "session.rewound"
)

// Known is every type schema v1 defines. A type outside it is kept by
// every store and reported by the fold.
var Known = map[Type]bool{
	TypeUserMessage: true, TypeUserInterrupt: true, TypeUserToolConfirmation: true,
	TypeUserToolResult: true, TypeAgentMessage: true, TypeAgentToolUse: true,
	TypeToolResult: true, TypeThreadStarted: true, TypeThreadEnded: true,
	TypeThreadMessage: true, TypeContextCompacted: true, TypeModelRequest: true,
	TypeSessionStatus: true, TypeSessionMachine: true, TypeScopeChanged: true, TypeSessionResumed: true,
	TypeSessionError: true, TypeMemoryAttached: true, TypeMemorySynced: true,
	TypeEventRedacted: true, TypeSessionRewound: true,
}

// Event is one entry of a session's log.
type Event struct {
	ID        string          `json:"id"`
	Seq       uint64          `json:"seq"`
	SessionID string          `json:"session_id"`
	Type      Type            `json:"type"`
	Time      time.Time       `json:"time"`
	Turn      int             `json:"turn,omitempty"`
	Step      int             `json:"step,omitempty"`
	Thread    string          `json:"thread,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}

// tombstone is the payload a redacted event keeps.
var tombstone = json.RawMessage(`{"tombstone":true}`)

// Redacted reports whether the event's payload was replaced by redaction.
func (e Event) Redacted() bool {
	var p struct {
		Tombstone bool `json:"tombstone"`
	}
	return json.Unmarshal(e.Payload, &p) == nil && p.Tombstone
}

// NewEvent returns an event of the given type with a fresh id and the
// payload encoded in the canonical form. Seq and SessionID are the
// appender's to set.
func NewEvent(t Type, payload any, now time.Time) (Event, error) {
	b, err := Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("session: encode %s payload: %w", t, err)
	}
	return Event{ID: NewID(PrefixEvent), Type: t, Time: now.UTC(), Payload: b}, nil
}

// Decode unmarshals the event's payload into v.
func (e Event) Decode(v any) error {
	if err := json.Unmarshal(e.Payload, v); err != nil {
		return fmt.Errorf("session: decode %s payload of %s: %w", e.Type, e.ID, err)
	}
	return nil
}

// The payloads of the event types the fold and the harness read. The
// fields are spec 004's table; a payload may carry fields a later schema
// adds, which a reader keeps.

// UserMessage is the payload of user.message.
type UserMessage struct {
	Sender  Sender      `json:"sender"`
	Content []lux.Block `json:"content"`
}

// UserInterrupt is the payload of user.interrupt.
type UserInterrupt struct {
	Sender Sender `json:"sender"`
}

// UserToolConfirmation is the payload of user.tool_confirmation.
type UserToolConfirmation struct {
	Sender    Sender `json:"sender"`
	ToolUseID string `json:"tool_use_id"`
	Decision  string `json:"decision"`
	Note      string `json:"note,omitempty"`
	Remember  string `json:"remember,omitempty"`
}

// Decisions of a tool confirmation.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

// UserToolResult is the payload of user.tool_result.
type UserToolResult struct {
	Sender    Sender      `json:"sender"`
	ToolUseID string      `json:"tool_use_id"`
	Content   []lux.Block `json:"content"`
	IsError   bool        `json:"is_error,omitempty"`
}

// AgentMessage is the payload of agent.message.
type AgentMessage struct {
	Message        lux.Message   `json:"message"`
	StopReason     ir.StopReason `json:"stop_reason"`
	Request        string        `json:"request,omitempty"`
	Truncated      bool          `json:"truncated,omitempty"`
	ContinuationOf string        `json:"continuation_of,omitempty"`
}

// Risk is the risk score recorded on a tool call (spec 012).
type Risk struct {
	Score    float64  `json:"score"`
	Source   string   `json:"source"`
	Features []string `json:"features,omitempty"`
}

// AgentToolUse is the payload of agent.tool_use.
type AgentToolUse struct {
	ToolUseID  string          `json:"tool_use_id"`
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input"`
	Risk       *Risk           `json:"risk,omitempty"`
	Verdict    string          `json:"verdict"`
	Reason     string          `json:"reason,omitempty"`
	Mode       string          `json:"mode,omitempty"`
	Client     bool            `json:"client,omitempty"`
	Repeatable bool            `json:"repeatable,omitempty"`
}

// Spill names where the rest of a capped tool output went.
type Spill struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ToolResult is the payload of tool.result. Meta is the tool's own
// record for later calls of the thread: the hash of a file a file tool
// read or wrote, the directory bash ended in, the todo list.
type ToolResult struct {
	ToolUseID  string          `json:"tool_use_id"`
	Content    []lux.Block     `json:"content"`
	IsError    bool            `json:"is_error,omitempty"`
	Outcome    string          `json:"outcome,omitempty"`
	DurationMS int64           `json:"duration_ms,omitempty"`
	Spill      *Spill          `json:"spill,omitempty"`
	Meta       json.RawMessage `json:"meta,omitempty"`
}

// ThreadStarted is the payload of thread.started. The event's id is the
// new thread's id, and so is its thread field; Parent names the thread
// that started it.
type ThreadStarted struct {
	Agent     AgentRef `json:"agent"`
	Parent    string   `json:"parent,omitempty"`
	ToolUseID string   `json:"tool_use_id,omitempty"`
	Task      string   `json:"task"`
	Isolation string   `json:"isolation,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
	Depth     int      `json:"depth"`
	Model     string   `json:"model,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Budget    *Budget  `json:"budget,omitempty"`
	// Ignored are the fields of the agent the thread names that it does
	// not use, since a thread acts with the session's credentials and
	// machine (spec 013): identity, permissions, model.credential,
	// connections, memoryStores, machine.
	Ignored []string `json:"ignored,omitempty"`
}

// ThreadEnded is the payload of thread.ended.
type ThreadEnded struct {
	Reason       string     `json:"reason"`
	FinalText    string     `json:"final_text,omitempty"`
	Usage        *lux.Usage `json:"usage,omitempty"`
	CostUSDMicro *int64     `json:"cost_usd_micro,omitempty"`
}

// ThreadMessage is the payload of thread.message; the event's thread is
// the receiving thread, the same as To. FromName is the
// sending thread's agent name, recorded by the runner so the fold renders
// the sender without looking up another thread.
type ThreadMessage struct {
	From      string      `json:"from,omitempty"`
	To        string      `json:"to,omitempty"`
	FromName  string      `json:"from_name,omitempty"`
	Content   []lux.Block `json:"content"`
	ToolUseID string      `json:"tool_use_id,omitempty"`
}

// Kinds and causes of a compaction.
const (
	CompactClearToolResults = "clear_tool_results"
	CompactSummary          = "summary"
	CauseThreshold          = "threshold"
	CauseRedaction          = "redaction"
)

// ContextCompacted is the payload of context.compacted.
type ContextCompacted struct {
	Kind         string   `json:"kind"`
	FromSeq      uint64   `json:"from_seq,omitempty"`
	ToSeq        uint64   `json:"to_seq,omitempty"`
	ToolUseIDs   []string `json:"tool_use_ids,omitempty"`
	Summary      string   `json:"summary,omitempty"`
	Cause        string   `json:"cause,omitempty"`
	Request      string   `json:"request,omitempty"`
	TokensBefore int64    `json:"tokens_before,omitempty"`
	TokensAfter  int64    `json:"tokens_after,omitempty"`
}

// ModelRequest is the payload of model.request.
type ModelRequest struct {
	Model         string        `json:"model"`
	Family        string        `json:"family,omitempty"`
	Dialect       string        `json:"dialect,omitempty"`
	Codec         string        `json:"codec,omitempty"`
	PromptVersion string        `json:"prompt_version,omitempty"`
	ToolsSHA256   string        `json:"tools_sha256,omitempty"`
	RequestSHA256 string        `json:"request_sha256,omitempty"`
	RequestBytes  int64         `json:"request_bytes,omitempty"`
	RequestBlob   Digest        `json:"request_blob,omitempty"`
	ResponseBlob  Digest        `json:"response_blob,omitempty"`
	Usage         *lux.Usage    `json:"usage,omitempty"`
	CostUSDMicro  *int64        `json:"cost_usd_micro,omitempty"`
	CostSource    string        `json:"cost_source,omitempty"`
	LatencyMS     int64         `json:"latency_ms,omitempty"`
	FirstTokenMS  int64         `json:"first_token_ms,omitempty"`
	StopReason    ir.StopReason `json:"stop_reason,omitempty"`
	Attempts      int           `json:"attempts,omitempty"`
	Outcome       string        `json:"outcome"`
	Error         string        `json:"error,omitempty"`
	Loss          []string      `json:"loss,omitempty"`
	// FoldSeq is the log's last sequence when the request was built:
	// the request is the fold of the events through it, which a replay
	// folds again (spec 007).
	FoldSeq uint64 `json:"fold_seq,omitempty"`
}

// CheckpointRef names a checkpoint (spec 034).
type CheckpointRef struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// RunnerRef names the runner that claimed a session.
type RunnerRef struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// SessionStatus is the payload of session.status.
type SessionStatus struct {
	Status     Status         `json:"status"`
	StopReason StopReason     `json:"stop_reason,omitempty"`
	Detail     string         `json:"detail,omitempty"`
	Runner     *RunnerRef     `json:"runner,omitempty"`
	Checkpoint *CheckpointRef `json:"checkpoint,omitempty"`
}

// AttachedMachine is the machine a runner attached (spec 009).
type AttachedMachine struct {
	Kind        string `json:"kind"`
	ID          string `json:"id,omitempty"`
	Workdir     string `json:"workdir,omitempty"`
	OS          string `json:"os,omitempty"`
	Arch        string `json:"arch,omitempty"`
	Environment string `json:"environment,omitempty"`
	// Sandbox is the host sandbox driver the machine's commands run
	// under, set on a server's host (spec 009).
	Sandbox string `json:"sandbox,omitempty"`
}

// Instructions names one project instruction file the harness loaded.
type Instructions struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Blob   Digest `json:"blob"`
}

// Skill is one skill in the index the model sees (spec 011).
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

// SessionMachine is the payload of session.machine.
type SessionMachine struct {
	Machine      AttachedMachine `json:"machine"`
	Reason       string          `json:"reason"`
	Context      string          `json:"context,omitempty"`
	Instructions []Instructions  `json:"instructions,omitempty"`
	Skills       []Skill         `json:"skills,omitempty"`
	Checkpoint   *CheckpointRef  `json:"checkpoint,omitempty"`
}

// SessionError is the payload of session.error.
type SessionError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// MemoryAttached is the payload of memory.attached.
type MemoryAttached struct {
	MemoryStoreID string `json:"memory_store_id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Access        string `json:"access"`
	Path          string `json:"path"`
	Version       string `json:"version,omitempty"`
}

// ScopeChanged is the payload of session.scope_changed. Old and New are
// the scope's permission entries; Until is a time, "end_of_turn", or
// empty for a standing change.
type ScopeChanged struct {
	By     Sender            `json:"by"`
	Old    []json.RawMessage `json:"old"`
	New    []json.RawMessage `json:"new"`
	Reason string            `json:"reason,omitempty"`
	Until  string            `json:"until,omitempty"`
}

// SessionResumed is the payload of session.resumed: a session idle on its
// budget resumes once its cap is raised or its payer's credit restored
// (spec 007). MaxCostUSDMicro is the session's budget from here on,
// absent when it has none.
type SessionResumed struct {
	By              Sender `json:"by"`
	Reason          string `json:"reason,omitempty"`
	MaxCostUSDMicro *int64 `json:"max_cost_usd_micro,omitempty"`
}

// MemorySynced is the payload of memory.synced.
type MemorySynced struct {
	MemoryStoreID string   `json:"memory_store_id"`
	Pushed        int      `json:"pushed"`
	Pulled        int      `json:"pulled"`
	Deleted       int      `json:"deleted"`
	Conflicts     []string `json:"conflicts,omitempty"`
	Version       string   `json:"version,omitempty"`
}

// EventRedacted is the payload of event.redacted.
type EventRedacted struct {
	EventID string `json:"event_id"`
	By      Sender `json:"by"`
	Reason  string `json:"reason,omitempty"`
}

// SessionRewound is the payload of session.rewound.
type SessionRewound struct {
	ToTurn     int            `json:"to_turn"`
	Checkpoint CheckpointRef  `json:"checkpoint"`
	Saved      *CheckpointRef `json:"saved,omitempty"`
	By         Sender         `json:"by"`
}
