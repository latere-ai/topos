// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"slices"
)

// The values of a call's undo (spec 101), from the least reach to the
// most: the call changes nothing; it can be undone in the working
// directory with no loss; it can be undone from a copy or by another
// call; it cannot be undone, or others already saw it.
const (
	UndoNone         = "none"
	UndoReversible   = "reversible"
	UndoRecoverable  = "recoverable"
	UndoIrreversible = "irreversible"
)

// Undos are the values of undo, from the least reach to the most.
var Undos = []string{UndoNone, UndoReversible, UndoRecoverable, UndoIrreversible}

// The classes of what a call changes (spec 101), from the least reach to
// the most: something disposable; something this session made, its
// branch, its preview or its checkout; something other people or the
// person's own files use; the default or a protected branch, production,
// a live release.
const (
	TargetScratch = "scratch"
	TargetOwn     = "own"
	TargetShared  = "shared"
	TargetPrimary = "primary"
)

// TargetClasses are the classes of a target, from the least reach to the
// most.
var TargetClasses = []string{TargetScratch, TargetOwn, TargetShared, TargetPrimary}

// TargetKinds are the kinds of a target a recorded scope names: a word
// for the type of the target and never its name.
var TargetKinds = []string{"path", "ref", "app", "host", "repository", "package", "deploy", "service"}

// The contacts of a call: the session's first with its target, or a
// later one. A first contact is the wider reach.
const (
	ContactRepeat = "repeat"
	ContactFirst  = "first"
)

// Contacts are the values of contact, from the least reach to the most.
var Contacts = []string{ContactRepeat, ContactFirst}

// The sources of a recorded scope: the decision service's reading joined
// with Topos's own claims, or Topos's claims alone.
const (
	ScopeFromService = "service"
	ScopeFromTopos   = "topos"
)

// Crosses are the boundaries a call crosses, as far as the one who says
// it knows (spec 101): the machine outside the working directory, another
// system, a host outside the session's network, a party the organization
// does not own, and anyone at all. A boundary nobody knows is nil, never
// false.
type Crosses struct {
	Machine      *bool `json:"machine,omitzero"`
	Remote       *bool `json:"remote,omitzero"`
	Network      *bool `json:"network,omitzero"`
	Organization *bool `json:"organization,omitzero"`
	Public       *bool `json:"public,omitzero"`
}

// Empty reports whether no boundary is known.
func (c Crosses) Empty() bool {
	return c.Machine == nil && c.Remote == nil && c.Network == nil && c.Organization == nil && c.Public == nil
}

// Wider is the wider of c and o in each boundary: true before false, and
// a known value before an unknown one.
func (c Crosses) Wider(o Crosses) Crosses {
	or := func(a, b *bool) *bool {
		switch {
		case a == nil:
			return b
		case b == nil:
			return a
		}
		v := *a || *b
		return &v
	}
	return Crosses{
		Machine: or(c.Machine, o.Machine), Remote: or(c.Remote, o.Remote), Network: or(c.Network, o.Network),
		Organization: or(c.Organization, o.Organization), Public: or(c.Public, o.Public),
	}
}

// CallScope is the reach a call was decided on, as its agent.tool_use
// records it before the call waits for a person (spec 101): whether it
// can be undone, the boundaries it crosses, the class and kind of its
// target, and whether it is a first contact. Every member but Source is
// optional, and one left out is not known. No member holds a name: never
// a path, a host, a ref, an app's slug or other text of the call's input.
type CallScope struct {
	Undo        string   `json:"undo,omitempty"`
	Crosses     *Crosses `json:"crosses,omitzero"`
	TargetClass string   `json:"target_class,omitempty"`
	TargetKind  string   `json:"target_kind,omitempty"`
	Contact     string   `json:"contact,omitempty"`
	Source      string   `json:"source"`
}

// Widest is the wider of two values of an ordered list, a known one
// before an empty one; a value outside the list is empty.
func Widest(order []string, a, b string) string {
	i, j := slices.Index(order, a), slices.Index(order, b)
	switch {
	case i < 0 && j < 0:
		return ""
	case i < j:
		return b
	}
	return a
}

// Join is the scope of a call from the decision service's reading s and
// Topos's own claims t: in each member the service's value, or Topos's
// where the service gives none or a narrower one, so the wider of the
// two. Its Source is service.
func (s CallScope) Join(t CallScope) CallScope {
	out := CallScope{
		Undo:        Widest(Undos, s.Undo, t.Undo),
		TargetClass: Widest(TargetClasses, s.TargetClass, t.TargetClass),
		Contact:     Widest(Contacts, s.Contact, t.Contact),
		TargetKind:  s.TargetKind,
		Source:      ScopeFromService,
	}
	if !slices.Contains(TargetKinds, out.TargetKind) {
		out.TargetKind = ""
	}
	if out.TargetKind == "" && slices.Contains(TargetKinds, t.TargetKind) {
		out.TargetKind = t.TargetKind
	}
	var c Crosses
	if s.Crosses != nil {
		c = *s.Crosses
	}
	if t.Crosses != nil {
		c = c.Wider(*t.Crosses)
	}
	if !c.Empty() {
		out.Crosses = &c
	}
	return out
}

// The surfaces a person answers a confirmation on (spec 101): the
// conversation, with the call waiting in it; a system notification,
// without opening the session; a list of waiting questions or reviews;
// one gesture that answered more than one call.
const (
	SurfacePrompt       = "prompt"
	SurfaceNotification = "notification"
	SurfaceQueue        = "queue"
	SurfaceBulk         = "bulk"
)

// Surfaces are the surfaces a confirmation names.
var Surfaces = []string{SurfacePrompt, SurfaceNotification, SurfaceQueue, SurfaceBulk}

// MaxAnswerMS is the longest answer_ms a confirmation carries: a day.
const MaxAnswerMS = 86_400_000

// CheckAnswer reports why the fields of c that say how the person
// answered cannot be read, nil when they can: an answer_ms outside 0 to
// MaxAnswerMS, or a surface outside Surfaces.
func (c UserToolConfirmation) CheckAnswer() error {
	if c.AnswerMS != nil && (*c.AnswerMS < 0 || *c.AnswerMS > MaxAnswerMS) {
		return fmt.Errorf("answer_ms is %d, not 0 to %d", *c.AnswerMS, MaxAnswerMS)
	}
	if c.Surface != "" && !slices.Contains(Surfaces, c.Surface) {
		return fmt.Errorf("surface is %q, not one of %v", c.Surface, Surfaces)
	}
	return nil
}

// Boundary is the organization's boundary an allow of session.create,
// session.fork or session.send named (spec 101): the hosts the
// organization owns, each a host name or "*." and a domain, lowercased.
type Boundary struct {
	Hosts []string `json:"hosts"`
}

// MaxBoundaryHosts is the most hosts a boundary names.
const MaxBoundaryHosts = 256

// DenialLimits are the refusals that start a session's fallback (spec
// 101): in a row, and in all. Zero turns a limit off.
type DenialLimits struct {
	Consecutive int `json:"consecutive"`
	Total       int `json:"total"`
}

// Lower is l with each limit of o in its place where o's is lower and on:
// an authorizer's limits make the installation's stricter and never
// looser. A limit l turns off is turned on by o's.
func (l DenialLimits) Lower(o *DenialLimits) DenialLimits {
	if o == nil {
		return l
	}
	lower := func(a, b int) int {
		switch {
		case b <= 0:
			return a
		case a <= 0:
			return b
		}
		return min(a, b)
	}
	return DenialLimits{Consecutive: lower(l.Consecutive, o.Consecutive), Total: lower(l.Total, o.Total)}
}

// The defaults of the denial limits, TOPOS_DENIALS_CONSECUTIVE and
// TOPOS_DENIALS_TOTAL.
const (
	DefaultDenialsConsecutive = 3
	DefaultDenialsTotal       = 20
)

// The limits a session.denials_reached names: the one the refusal reached
// first.
const (
	LimitConsecutive = "consecutive"
	LimitTotal       = "total"
)

// The effects of a session.denials_reached: an attended turn asks the
// person for every call that changes something, and an unattended one
// stops.
const (
	DenialsAsk  = "ask"
	DenialsStop = "stop"
)

// DenialsReached is the payload of session.denials_reached (spec 101): a
// refusal reached a limit, with the counts at that refusal, the limit it
// reached first, and what the session does now.
type DenialsReached struct {
	Consecutive int    `json:"consecutive"`
	Total       int    `json:"total"`
	Limit       string `json:"limit"`
	Effect      string `json:"effect"`
}

// Refusals are the refusals of a session's log (spec 101), counted in the
// order of their events across the session's own conversation and every
// subagent: Consecutive since the last call that changed something and
// ran, and Total since the session started or the fallback last ended.
// Fallback is set while the session asks the person for every call that
// changes something: from a session.denials_reached until the person's
// next message, or, after one that stopped an unattended turn, until the
// message after the one that began the person's next turn. Stopped is the
// sequence of a session.denials_reached that stopped a turn and that no
// person's message followed, zero for none.
type Refusals struct {
	Consecutive int
	Total       int
	Fallback    bool
	Stopped     uint64
}

// Reached is the limit the counts reach under limits, outside the
// fallback, and false when they reach none: consecutive before total.
func (r Refusals) Reached(limits DenialLimits) (string, bool) {
	switch {
	case r.Fallback:
		return "", false
	case limits.Consecutive > 0 && r.Consecutive >= limits.Consecutive:
		return LimitConsecutive, true
	case limits.Total > 0 && r.Total >= limits.Total:
		return LimitTotal, true
	}
	return "", false
}

// refusedBy are the layers whose block is a refusal: the risk score's
// block_at, the session's network, and a decision service's suggestion. A
// block in plan mode is not one: plan mode blocks every change by design.
func refusedBy(u AgentToolUse) bool {
	switch u.DecidedBy {
	case DecidedScore:
		return u.DecidedByRule == "block_at"
	case DecidedNetwork, DecidedLearned:
		return true
	}
	return false
}

// changes reports whether a recorded call changes something: a call no
// read-only, memory or question layer decided, whose score was not read
// for a tool that reads or changes nothing.
func changes(u AgentToolUse) bool {
	switch u.DecidedBy {
	case DecidedReadOnly, DecidedMemory, DecidedQuestion, DecidedPlan:
		return false
	}
	if u.Risk != nil {
		for _, f := range u.Risk.Features {
			if f == "effect:none" || f == "effect:read" || f == "memory" {
				return false
			}
		}
	}
	return true
}

// RefusalsOf reads the refusals of a log (spec 101). A refusal is a call
// that did not run because the risk score, the session's network or a
// decision service blocked it, a confirmation that denied a call, or the
// approval.decided of a network approval a person's confirmation denied.
// A call that changes something and runs, an allow, a flag or an ask a
// person allowed, ends a run of refusals; a read-only call neither counts
// nor ends one. The fold is pure, so every runner reads the same counts
// after a handoff.
func RefusalsOf(evs []Event) Refusals {
	var r Refusals
	// state is none, the fallback of an attended turn, or the one an
	// unattended turn's stop left for the person's next turn.
	const (
		none = iota
		asking
		stopped
	)
	state := none
	asked := map[string]bool{}
	denied := map[string]bool{}
	refuse := func() { r.Consecutive++; r.Total++ }
	for _, e := range evs {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeAgentToolUse:
			var u AgentToolUse
			if e.Decode(&u) != nil {
				continue
			}
			switch u.Verdict {
			case "block":
				if refusedBy(u) {
					refuse()
				}
			case "ask":
				asked[u.ToolUseID] = changes(u)
			case "allow", "flag":
				if changes(u) {
					r.Consecutive = 0
				}
			}
		case TypeUserToolConfirmation:
			var c UserToolConfirmation
			if e.Decode(&c) != nil {
				continue
			}
			if c.ApprovalID != "" {
				denied[c.ApprovalID] = c.Decision == DecisionDeny
				continue
			}
			ch, ok := asked[c.ToolUseID]
			if !ok {
				continue
			}
			delete(asked, c.ToolUseID)
			switch {
			case c.Decision == DecisionDeny:
				refuse()
			case ch:
				r.Consecutive = 0
			}
		case TypeApprovalDecided:
			var d ApprovalDecided
			if e.Decode(&d) == nil && d.Decision == DecisionDeny && denied[d.ApprovalID] {
				refuse()
			}
			delete(denied, d.ApprovalID)
		case TypeDenialsReached:
			var d DenialsReached
			if e.Decode(&d) != nil {
				continue
			}
			state = asking
			if d.Effect == DenialsStop {
				state, r.Stopped = stopped, e.Seq
			}
		case TypeUserMessage:
			var m UserMessage
			if e.Decode(&m) != nil || m.Sender.Kind != SenderPerson {
				continue
			}
			switch state {
			case asking:
				state, r.Consecutive, r.Total = none, 0, 0
			case stopped:
				state, r.Stopped = asking, 0
			}
		}
	}
	r.Fallback = state != none
	return r
}

// RefusedIn counts the refusals among the calls of thread whose events
// follow the sequence after (spec 101): the calls the risk score, the
// session's network or a decision service blocked, and the ones a
// person's confirmation denied. A subagent's run is the events after the
// one that drove it, so the count is what its caller is warned of.
func RefusedIn(evs []Event, thread string, after uint64) int {
	n := 0
	asked := map[string]bool{}
	approvals := map[string]bool{}
	denied := map[string]bool{}
	for _, e := range evs {
		if e.Seq <= after || e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeAgentToolUse:
			var u AgentToolUse
			if e.Subagent != thread || e.Decode(&u) != nil {
				continue
			}
			switch {
			case u.Verdict == "block" && refusedBy(u):
				n++
			case u.Verdict == "ask":
				asked[u.ToolUseID] = true
			}
		case TypeApprovalRequested:
			var p ApprovalRequested
			if e.Subagent == thread && e.Decode(&p) == nil {
				approvals[p.ApprovalID] = true
			}
		case TypeUserToolConfirmation:
			var c UserToolConfirmation
			if e.Decode(&c) != nil || c.Decision != DecisionDeny {
				continue
			}
			if asked[c.ToolUseID] {
				n++
				delete(asked, c.ToolUseID)
			}
			if approvals[c.ApprovalID] {
				denied[c.ApprovalID] = true
			}
		case TypeApprovalDecided:
			var d ApprovalDecided
			if e.Decode(&d) == nil && d.Decision == DecisionDeny && denied[d.ApprovalID] {
				n++
				delete(denied, d.ApprovalID)
			}
		}
	}
	return n
}
