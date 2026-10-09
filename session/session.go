// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"
	"unicode"
	"unicode/utf8"
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
	// StopQuestion is a question call that waits for the person's answer
	// (spec 039).
	StopQuestion    StopReason = "question"
	StopBudget      StopReason = "budget"
	StopTurnLimit   StopReason = "turn_limit"
	StopOutputLimit StopReason = "output_limit"
	StopInterrupted StopReason = "interrupted"
	StopError       StopReason = "error"
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

// TriggerSubjectPrefix begins the subject of a trigger's messages,
// trigger:<trg_id> (spec 004).
const TriggerSubjectPrefix = "trigger:"

// AuthorizerSubject is the subject of a change the installation's
// authorizer made, a Sender of kind service: the model a send's allow
// moved the session to (spec 038), which nobody who wrote to the session
// asked for.
const AuthorizerSubject = "service:authorizer"

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
	// App is the app at the installation's app host a repository is the
	// source of, nil for any other; only the allow of the session's create
	// attaches one (spec 058).
	App *ResourceApp `json:"app,omitempty"`
	// Attached marks a resource the allow of the session's create
	// attached rather than the request or the agent named. A fork does
	// not carry it: its own allow attaches what it reaches (spec 058).
	Attached bool `json:"attached,omitempty"`
}

// Policy is a session's approval policy (spec 012): the agent's
// spec.approvals merged at create with the authorizer's limits, so every
// runner that drives the session applies the same mode, lists and
// thresholds.
type Policy struct {
	Mode          string     `json:"mode"`
	AlwaysConfirm []string   `json:"always_confirm,omitempty"`
	AlwaysAllow   []string   `json:"always_allow,omitempty"`
	Thresholds    Thresholds `json:"thresholds"`
}

// Thresholds are the progressive mode's risk cut-offs, each in [0, 1].
type Thresholds struct {
	FlagAt  float64 `json:"flag_at"`
	AskAt   float64 `json:"ask_at"`
	BlockAt float64 `json:"block_at"`
}

// Budget is a session's spend ceiling and its spend so far, in millionths
// of a USD. A nil MaxCostUSDMicro means no ceiling.
type Budget struct {
	MaxCostUSDMicro   *int64 `json:"max_cost_usd_micro,omitempty"`
	SpentCostUSDMicro int64  `json:"spent_cost_usd_micro"`
	// CarriedCostUSDMicro is the part of the spend a fork copied from its
	// parent's log, fixed at the fork, zero on a session no fork made
	// (spec 056). The session's own spend, which its ceiling holds, is
	// SpentCostUSDMicro less it.
	CarriedCostUSDMicro int64 `json:"carried_cost_usd_micro,omitempty"`
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

// ModelRef names a model a session runs and the reasoning level it runs
// at: one of manifest/v1's Efforts, or empty for the model's own default
// (spec 015). Via is the name that was asked, of the agent, of a person's
// change or of the session before, when the authorizer answered another,
// and empty when the session runs the name asked (spec 038). Route is the
// routed name the authorizer resolved Via to before it named the model,
// empty when it named none (spec 061): the core keeps it and never reads
// what it means. MessageText is the authorizer's ask for the opening of
// each later message on the session, kept beside the route by the same
// rules (spec 063); a question carries the opening only while the
// operator's ceiling allows it.
//
// The level is stored under effort, the spelling every header and every
// session.model_changed stored before the rename carries, so an earlier
// release reads what this one stores (spec 049). The API answers it under
// reasoning alone: Answered moves it there, and nothing stored sets
// Reasoning.
type ModelRef struct {
	Name        string `json:"name"`
	Via         string `json:"via,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Reasoning   string `json:"reasoning,omitempty"`
	Route       string `json:"route,omitempty"`
	MessageText bool   `json:"message_text,omitempty"`
}

// Level is the reasoning level under either name: reasoning, and effort
// where reasoning is empty.
func (m ModelRef) Level() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	return m.Effort
}

// Answered is m as the API answers it: the level under reasoning and none
// under effort.
func (m ModelRef) Answered() ModelRef {
	m.Reasoning, m.Effort = m.Level(), ""
	return m
}

// Capture says which raw wire data a session keeps beyond the default.
type Capture struct {
	Requests bool `json:"requests,omitempty"`
}

// Parent names the session and sequence a forked session started from:
// the session forked and how many of its events the fork copied, 0 for
// a fork before its opening message (spec 056).
type Parent struct {
	SessionID string `json:"session_id"`
	Seq       uint64 `json:"seq"`
}

// Tree is a fork tree as a list grouped by tree answers it (spec 056):
// the id of the session at its top, and how many of its sessions the
// list keeps under the same filters.
type Tree struct {
	Root     string `json:"root"`
	Sessions int    `json:"sessions"`
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
	// Policy is the merged approval policy; nil, as on a local session,
	// leaves the agent's own to the runner.
	Policy *Policy `json:"policy,omitempty"`
	Budget Budget  `json:"budget"`
	// Model is the model the session's turns run: the one its create's
	// allow named, then the latest session.model_changed's; nil runs the
	// agent's.
	Model     *ModelRef `json:"model,omitempty"`
	Limits    Limits    `json:"limits"`
	Capture   Capture   `json:"capture"`
	EndOnIdle bool      `json:"end_on_idle,omitempty"`
	// Attended is the creator's declaration that a person answers the
	// session's questions, through a client that shows them (spec 039). A
	// question call of a session that is not attended is answered at
	// once, and the session never goes idle on it. It is set at create
	// and no event changes it, so every runner reads the same value.
	Attended bool `json:"attended,omitempty"`
	// Network is the session's base network (spec 052): the one its
	// create's allow named, or its agent's, with each change an allow of a
	// send made since. Nil on a session that recorded none, which runs on
	// its agent's mode and hosts.
	Network *Network `json:"network,omitempty"`
	// Instructions are the initiator's standing instructions an allow of
	// the session's create carried (spec 053), which the model reads after
	// its agent's own; empty for none. No event changes them.
	Instructions string `json:"instructions,omitempty"`
	// Context is the text the allow of the session's create attached, in
	// its order, which the model reads after the initiator's instructions
	// (spec 058); no event changes it.
	Context []ContextPart `json:"context,omitempty"`
	Parent  *Parent       `json:"parent,omitempty"`
	// Root is the id of the session at the top of this session's fork
	// tree: its parent's root, or its parent's id where the parent has
	// none; its own id on a fork that started a tree of its own; empty on
	// a session no fork made, whose tree's root is itself (spec 056). It
	// is fixed at the fork, and names a deleted session as readily as a
	// live one.
	Root      string    `json:"root,omitempty"`
	TriggerID string    `json:"trigger_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// ArchivedAt is when the session was filed away from the lists, nil
	// while it is not. Only a store's SetArchived writes it; no event
	// does, so an ended session's log stays closed (spec 015).
	ArchivedAt *time.Time        `json:"archived_at,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	// Tree is set only on the sessions a List grouped by tree answers,
	// each the one that stands for its tree (spec 056).
	Tree *Tree `json:"tree,omitempty"`
}

// TreeRoot is the id that keys s's fork tree: its root, or its own id
// where it has none.
func (s Session) TreeRoot() string {
	if s.Root != "" {
		return s.Root
	}
	return s.ID
}

// Copied reports whether e is one of the events a fork copied from the
// session's parent, which the session reads as history: its machines,
// its checkpoints and its budget are the parent's, not this session's
// (spec 017).
func (s Session) Copied(e Event) bool {
	return s.Parent != nil && e.Seq <= s.Parent.Seq
}

// MaxMetadata is the most entries a session's metadata holds.
const MaxMetadata = 32

// MaxTitleLength is the most characters a title a person changes a
// session's title to holds (spec 054).
const MaxTitleLength = 200

// CheckTitle reports why a title a person changes a session's title to
// cannot be one, "" when it can: empty, longer than MaxTitleLength
// characters, or holding a control character or a line or paragraph
// separator, which would break the line a list shows it on. The title is
// checked as given; trimming it is the caller's.
func CheckTitle(title string) string {
	switch n := utf8.RuneCountInString(title); {
	case n == 0:
		return "the title is empty"
	case n > MaxTitleLength:
		return fmt.Sprintf("the title is %d characters, more than %d", n, MaxTitleLength)
	}
	for _, r := range title {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return fmt.Sprintf("the title holds the control character %U", r)
		}
	}
	return ""
}

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

// Spent is the budget meter of spec 007: the cost of every model.request
// of every thread, and of every tool.result a service charged for (spec
// 047), a redacted one included, since its tombstone keeps the cost.
func Spent(log []Event) int64 {
	var total int64
	for _, e := range log {
		total += Cost(e)
	}
	return total
}

// Cost is what one event adds to the session's spend: a model request's
// cost, a tool result's, and nothing for any other event. A redacted
// model request is not counted, and none is ever redacted (Redactable);
// a redacted tool result is read from its tombstone.
func Cost(e Event) int64 {
	switch {
	case e.Type == TypeModelRequest && !e.Redacted():
		var p ModelRequest
		if e.Decode(&p) == nil && p.CostUSDMicro != nil {
			return *p.CostUSDMicro
		}
	case e.Type == TypeToolResult:
		var p struct {
			CostUSDMicro *int64 `json:"cost_usd_micro"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.CostUSDMicro != nil {
			return *p.CostUSDMicro
		}
	}
	return 0
}

// HasPendingInput reports whether a session's log holds input a runner
// should act on: a resuming user event, a message, a confirmation, a
// client tool's result, an answer to a question, or a session.resumed
// after a budget stop, after its last session.status (the stop reason
// table of spec 004). A session that has never run has pending input once
// its first message is in the log. A user.interrupt is input in one case
// alone (spec 039): it closed a question whose result no runner appended
// yet, so a runner claims the session to close the call. An interrupt on
// any other idle session starts nothing.
func HasPendingInput(evs []Event) bool {
	for _, ev := range slices.Backward(evs) {
		switch ev.Type {
		case TypeSessionStatus:
			return false
		case TypeUserMessage, TypeUserToolConfirmation, TypeUserToolResult, TypeSessionResumed, TypeUserAnswer:
			return true
		case TypeUserInterrupt:
			if c, owed := owedQuestion(evs); owed && c.EventID == ev.ID {
				return true
			}
		}
	}
	return false
}

// Answer is what a call waiting for a person needs: a confirmation of a
// call whose verdict was ask, the result of a call a client runs, or the
// answer to a question.
type Answer int

// The answers a waiting call takes.
const (
	AnswerConfirmation Answer = iota + 1
	AnswerResult
	AnswerQuestion
)

// Awaiting lists the calls of a log, in every thread, that wait for a
// person's answer, by tool_use id: an ask not yet confirmed, a client's
// call without its result, and the question nothing closed. A call
// answered once is not awaiting a second answer, so a repeated or stray
// confirmation or result names no call here. A person's message appended
// after an ask denies it (spec 012), so the ask awaits no confirmation
// after it. What closed a question is Questions' to say. An
// approval.requested that asks and that nothing answered awaits a
// confirmation by its approval_id (spec 052).
func Awaiting(evs []Event) map[string]Answer {
	out := map[string]Answer{}
	if q, open := OpenQuestion(evs); open {
		out[q.ToolUseID] = AnswerQuestion
	}
	for _, e := range evs {
		// A redacted result still answers its call.
		if id := e.Answers(); id != "" {
			delete(out, id)
			continue
		}
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeAgentToolUse:
			var p AgentToolUse
			if e.Decode(&p) != nil {
				continue
			}
			switch {
			case p.Client:
				out[p.ToolUseID] = AnswerResult
			case p.Verdict == "ask":
				out[p.ToolUseID] = AnswerConfirmation
			}
		case TypeUserToolConfirmation:
			var p UserToolConfirmation
			if e.Decode(&p) == nil && out[p.ToolUseID] == AnswerConfirmation {
				delete(out, p.ToolUseID)
			}
		case TypeUserMessage:
			var p UserMessage
			if e.Decode(&p) != nil || p.Sender.Kind != SenderPerson {
				continue
			}
			for id, want := range out {
				if want == AnswerConfirmation {
					delete(out, id)
				}
			}
		}
	}
	// An approval reads what answered it in its own order: a message
	// appended before it answers nothing.
	for _, a := range Approvals(evs) {
		if a.Waiting() {
			out[a.Request.ApprovalID] = AnswerConfirmation
		}
	}
	return out
}

// Redactable reports whether an event of typ holds content a person or
// a tool put in the log, which a redaction removes (spec 018). The
// record of what was decided and spent, a verdict, a confirmation, a
// model request, a status, a scope change, is not redactable, so no
// redaction rewrites the audit or the budget. An answer to a question
// holds a person's own words and is redactable, and so are the images an
// answer showed, files.kept (spec 055).
func Redactable(typ Type) bool {
	switch typ {
	case TypeUserMessage, TypeUserToolResult, TypeUserAnswer, TypeAgentMessage, TypeAgentToolUse, TypeToolResult, TypeContextCompacted, TypeFilesKept:
		return true
	}
	return false
}
