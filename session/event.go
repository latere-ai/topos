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
	TypeUserAnswer           Type = "user.answer"
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
	TypeModelChanged         Type = "session.model_changed"
	TypePolicyChanged        Type = "session.policy_changed"
	TypeTitleChanged         Type = "session.title_changed"
	TypeSessionResumed       Type = "session.resumed"
	TypeSessionError         Type = "session.error"
	TypeMemoryAttached       Type = "memory.attached"
	TypeMemorySynced         Type = "memory.synced"
	TypeEventRedacted        Type = "event.redacted"
	TypeSessionRewound       Type = "session.rewound"
	TypeAttachmentsDelivered Type = "attachments.delivered"
	TypeNetworkChanged       Type = "session.network_changed"
	TypeApprovalRequested    Type = "approval.requested"
	TypeApprovalDecided      Type = "approval.decided"
	TypeFilesKept            Type = "files.kept"
)

// Known is every type schema v1 defines. A type outside it is kept by
// every store and reported by the fold.
var Known = map[Type]bool{
	TypeUserMessage: true, TypeUserInterrupt: true, TypeUserToolConfirmation: true,
	TypeUserToolResult: true, TypeUserAnswer: true, TypeAgentMessage: true, TypeAgentToolUse: true,
	TypeToolResult: true, TypeThreadStarted: true, TypeThreadEnded: true,
	TypeThreadMessage: true, TypeContextCompacted: true, TypeModelRequest: true,
	TypeSessionStatus: true, TypeSessionMachine: true, TypeScopeChanged: true, TypeModelChanged: true, TypePolicyChanged: true, TypeTitleChanged: true, TypeSessionResumed: true,
	TypeSessionError: true, TypeMemoryAttached: true, TypeMemorySynced: true,
	TypeEventRedacted: true, TypeSessionRewound: true, TypeAttachmentsDelivered: true,
	TypeNetworkChanged: true, TypeApprovalRequested: true, TypeApprovalDecided: true,
	TypeFilesKept: true,
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

// UserMessage is the payload of user.message. Attachments are the files
// it carries, whose bytes are blobs of the session (spec 015). FiringID
// names the trigger's firing that sent it (spec 022).
type UserMessage struct {
	Sender      Sender       `json:"sender"`
	Content     []lux.Block  `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
	FiringID    string       `json:"firing_id,omitempty"`
}

// UserInterrupt is the payload of user.interrupt.
type UserInterrupt struct {
	Sender Sender `json:"sender"`
}

// UserToolConfirmation is the payload of user.tool_confirmation. It
// answers exactly one of a call that waits, by ToolUseID, and an
// approval.requested that asks, by ApprovalID (spec 052).
type UserToolConfirmation struct {
	Sender     Sender `json:"sender"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	Decision   string `json:"decision"`
	Note       string `json:"note,omitempty"`
	Remember   string `json:"remember,omitempty"`
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

// Suggestion is a decision service's suggestion for a call (spec 037):
// its verdict, the probability that the person approves and its
// uncertainty, the thresholds and the audit rate of the costs it applied,
// its source and its reason.
type Suggestion struct {
	Source      string               `json:"source"`
	Verdict     string               `json:"verdict"`
	Approve     float64              `json:"approve"`
	Uncertainty float64              `json:"uncertainty"`
	Thresholds  SuggestionThresholds `json:"thresholds"`
	AuditRate   float64              `json:"audit_rate"`
	Reason      string               `json:"reason,omitempty"`
}

// SuggestionThresholds place an approval probability on the verdict scale:
// above AllowAbove an allow, below BlockBelow a block, an ask between.
type SuggestionThresholds struct {
	AllowAbove float64 `json:"allow_above"`
	BlockBelow float64 `json:"block_below"`
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
	// ReviewProbability is the probability, fixed before the call ran,
	// that a person sees it (spec 037); nil on an event written before it.
	ReviewProbability *float64 `json:"review_probability,omitempty"`
	// Draw is the uniform draw the call's review used, made once.
	Draw *float64 `json:"draw,omitempty"`
	// Suggestion is a decision service's suggestion, when one was asked.
	Suggestion *Suggestion `json:"suggestion,omitempty"`
}

// Spill names where the rest of a capped tool output went.
type Spill struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// ToolResult is the payload of tool.result. Meta is the tool's own
// record for later calls of the thread: the hash of a file a file tool
// read or wrote, the directory bash ended in, the todo list.
// CostUSDMicro is what a service the call reached charged for it, a
// web search's (spec 047), which the session's spend counts beside its
// model requests; nil for a call that cost nothing.
type ToolResult struct {
	ToolUseID    string          `json:"tool_use_id"`
	Content      []lux.Block     `json:"content"`
	IsError      bool            `json:"is_error,omitempty"`
	Outcome      string          `json:"outcome,omitempty"`
	DurationMS   int64           `json:"duration_ms,omitempty"`
	Spill        *Spill          `json:"spill,omitempty"`
	Meta         json.RawMessage `json:"meta,omitempty"`
	CostUSDMicro *int64          `json:"cost_usd_micro,omitempty"`
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
	Kind       string   `json:"kind"`
	FromSeq    uint64   `json:"from_seq,omitempty"`
	ToSeq      uint64   `json:"to_seq,omitempty"`
	ToolUseIDs []string `json:"tool_use_ids,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	Cause      string   `json:"cause,omitempty"`
	Request    string   `json:"request,omitempty"`
	// Prompt names the compaction prompt a summary's request asked with,
	// such as compact/compact-v2, so a replay builds the request again
	// with the same text. It is absent on a summary asked with the first
	// version, the only one before the field existed.
	Prompt       string `json:"prompt,omitempty"`
	TokensBefore int64  `json:"tokens_before,omitempty"`
	TokensAfter  int64  `json:"tokens_after,omitempty"`
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
	// Outcome is ok, error, canceled, or escalated: a response that
	// stopped at a max_tokens below the model's output limit, whose
	// request was sent again at the limit (spec 005).
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
	// MaxTokens is the max_tokens the request asked.
	MaxTokens int64    `json:"max_tokens,omitempty"`
	Loss      []string `json:"loss,omitempty"`
	// FoldSeq is the log's last sequence when the request was built:
	// the request is the fold of the events through it, which a replay
	// folds again (spec 007).
	FoldSeq uint64 `json:"fold_seq,omitempty"`
}

// CheckpointRef names a checkpoint (spec 034). Remote is the URL of the
// repository that keeps it past the machine, as the session names the
// repository, when the runner pushed it there (spec 035); empty, the
// checkpoint lives on the machine alone.
type CheckpointRef struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
	Remote string `json:"remote,omitempty"`
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
	// Egress is the egress mode the machine reported when it attached,
	// open, allowlist or none (spec 052).
	Egress string `json:"egress,omitempty"`
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

// DeliveredRepository is one repository the runner delivered into the
// session's first machine (spec 019): its URL, the session's branch in
// it, and the commit that branch's HEAD was at once the delivery
// finished, absent for a repository with no commit yet. A repository
// that is an app's source names the app's slug and the base its branch
// started from (spec 058): BaseLive, BasePreview or BaseDefault, with
// BaseError saying why an app's served commit could not be had when it
// starts from BaseDefault for that reason; a ref the allow named leaves
// the base empty.
type DeliveredRepository struct {
	URL       string `json:"url"`
	Branch    string `json:"branch"`
	Commit    string `json:"commit,omitempty"`
	App       string `json:"app,omitempty"`
	Base      string `json:"base,omitempty"`
	BaseError string `json:"base_error,omitempty"`
}

// The bases an app's checkout starts from (spec 058): the commit the app
// serves at its address, the commit of its newest ready preview, or its
// repository's default branch.
const (
	BaseLive    = "live"
	BasePreview = "preview"
	BaseDefault = "default"
)

// SessionMachine is the payload of session.machine. Repositories are set
// on the attachment of the session's first machine, the one its
// repositories are delivered into, in the session's order.
type SessionMachine struct {
	Machine      AttachedMachine       `json:"machine"`
	Reason       string                `json:"reason"`
	Context      string                `json:"context,omitempty"`
	Instructions []Instructions        `json:"instructions,omitempty"`
	Skills       []Skill               `json:"skills,omitempty"`
	Repositories []DeliveredRepository `json:"repositories,omitempty"`
	Checkpoint   *CheckpointRef        `json:"checkpoint,omitempty"`
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

// ModelChanged is the payload of session.model_changed: the model the
// session's next turn runs, its reasoning level, or both changed (spec
// 015). By is the person who changed them, or the service when a send's
// allow named another model or another level (specs 038 and 049). Old is
// the model and the level the session ran, its agent's until a first
// change, and each names in via the name that was asked when the
// authorizer answered another. The payload is stored with each level
// under effort and answered with it under reasoning.
//
// Reason is why the service changed the model, a code a client maps to
// one sentence, and empty for every change but the one a turn makes when
// its model could not serve (spec 051): ReasonModelBusy, with the
// gateway's answer in Detail for a developer, followed by each model the
// authorizer named before that could not be connected, and why.
type ModelChanged struct {
	By     Sender   `json:"by"`
	Old    ModelRef `json:"old"`
	New    ModelRef `json:"new"`
	Reason string   `json:"reason,omitempty"`
	Detail string   `json:"detail,omitempty"`
}

// ReasonModelBusy is the reason of a session.model_changed a turn made
// when the model it ran could not serve and the authorizer named another
// to answer in its place (spec 051). A client renders it as
// MessageModelBusyChange.
const ReasonModelBusy = "model_busy"

// MessageModelBusyChange is the one sentence of ReasonModelBusy, for a
// person.
const MessageModelBusyChange = "The model was busy, so another one answered."

// PolicyChanged is the payload of session.policy_changed: a person
// changed the approval mode the session decides its calls under (spec
// 041). Old is the mode the session ran, New the one it runs from the
// next step; the lists and the thresholds of its policy are unchanged.
type PolicyChanged struct {
	By  Sender    `json:"by"`
	Old PolicyRef `json:"old"`
	New PolicyRef `json:"new"`
}

// TitleChanged is the payload of session.title_changed: the title the
// session had, empty for one created without a title, and the one it has
// (spec 054).
type TitleChanged struct {
	By  Sender `json:"by"`
	Old string `json:"old"`
	New string `json:"new"`
}

// PolicyRef is what a session.policy_changed names of a session's
// policy: its approval mode, one of manifest/v1's Mode values.
type PolicyRef struct {
	Mode string `json:"mode"`
}

// Mode is the approval mode of the latest session.policy_changed among
// events that s wrote itself, and false when it holds none: a fork reads
// its copied changes as its parent's history and runs the mode it was
// created with (spec 041).
func Mode(s Session, events []Event) (string, bool) {
	mode, found := "", false
	for _, e := range events {
		if e.Type != TypePolicyChanged || e.Redacted() || s.Copied(e) {
			continue
		}
		var p PolicyChanged
		if e.Decode(&p) == nil && p.New.Mode != "" {
			mode, found = p.New.Mode, true
		}
	}
	return mode, found
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
