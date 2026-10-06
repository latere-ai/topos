// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"slices"
	"strings"
)

// The modes of a session's network (spec 052), which are Cella's egress
// modes: open reaches every host but a denied one, allowlist reaches the
// network's hosts and the ones joined to it, and none reaches nothing.
const (
	NetworkOpen      = "open"
	NetworkAllowlist = "allowlist"
	NetworkNone      = "none"
)

// NetworkModes are the modes a network takes, widest first.
var NetworkModes = []string{NetworkOpen, NetworkAllowlist, NetworkNone}

// The sources of a session's network and of a change to it: the
// installation's authorizer, the agent's spec.machine when the
// authorizer named none, and a person's allow.
const (
	NetworkFromAuthorizer = "authorizer"
	NetworkFromAgent      = "agent"
	NetworkFromPerson     = "person"
)

// The reasons a client renders for what the network decided, kept
// byte-identical: a fetch inside the network, a fetch outside it that
// asks, and a command's connection the gateway refused.
const (
	ReasonInsideNetwork     = "inside the session's network"
	ReasonOutsideNetwork    = "outside the session's network"
	ReasonConnectionOutside = "connection outside the session's network"
)

// Network is a session's base network (spec 052): the one its create's
// allow named, or its agent's, as each authorizer change at a send has
// replaced it since. Hosts are host patterns under Cella's host rule and
// are set only with allowlist; Ask makes a first contact with a host
// outside the network ask the person attending the session.
type Network struct {
	Mode   string   `json:"mode"`
	Hosts  []string `json:"hosts,omitempty"`
	Ask    bool     `json:"ask,omitempty"`
	Source string   `json:"source,omitempty"`
}

// NetworkChanged is the payload of session.network_changed: the hosts a
// change added and removed, the mode and ask when they changed, and who
// made it. A person's allow adds a host to the session's network for the
// rest of the session and names the call or the approval it answered; an
// authorizer's change at a send replaces the base network.
type NetworkChanged struct {
	Added      []string `json:"added,omitempty"`
	Removed    []string `json:"removed,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Ask        *bool    `json:"ask,omitempty"`
	Source     string   `json:"source"`
	ToolUseID  string   `json:"tool_use_id,omitempty"`
	ApprovalID string   `json:"approval_id,omitempty"`
}

// Apply is base with an authorizer's change applied: the mode and ask it
// names, the hosts it removed taken out and the hosts it added joined, in
// sorted order. A nil base is the network of a session that recorded
// none, an allowlist of no hosts. base is not changed.
func (c NetworkChanged) Apply(base *Network) *Network {
	out := Network{Mode: NetworkAllowlist}
	if base != nil {
		out = *base
		out.Hosts = slices.Clone(base.Hosts)
	}
	if c.Mode != "" {
		out.Mode = c.Mode
	}
	if c.Ask != nil {
		out.Ask = *c.Ask
	}
	out.Hosts = slices.DeleteFunc(out.Hosts, func(h string) bool { return slices.Contains(c.Removed, h) })
	out.Hosts = JoinHosts(out.Hosts, c.Added)
	out.Source = NetworkFromAuthorizer
	return &out
}

// Change is the change that moves the base network from old to next, and
// false when they are the same network. A nil old is the network of a
// session that recorded none.
func Change(old *Network, next Network) (NetworkChanged, bool) {
	from := Network{Mode: NetworkAllowlist}
	if old != nil {
		from = *old
	}
	c := NetworkChanged{Source: NetworkFromAuthorizer}
	for _, h := range next.Hosts {
		if !slices.Contains(from.Hosts, h) {
			c.Added = append(c.Added, h)
		}
	}
	for _, h := range from.Hosts {
		if !slices.Contains(next.Hosts, h) {
			c.Removed = append(c.Removed, h)
		}
	}
	if next.Mode != from.Mode {
		c.Mode = next.Mode
	}
	if next.Ask != from.Ask {
		c.Ask = new(next.Ask)
	}
	changed := len(c.Added) > 0 || len(c.Removed) > 0 || c.Mode != "" || c.Ask != nil
	return c, changed
}

// JoinHosts is the hosts of every list, lowercased, without a trailing
// dot or a repeat, sorted. An empty entry is dropped.
func JoinHosts(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		for _, h := range l {
			if h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), "."); h != "" {
				out = append(out, h)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// The source of an approval.requested: a connection the egress gateway
// refused.
const ApprovalFromEgress = "egress"

// Destination is where a refused connection was going.
type Destination struct {
	Host string `json:"host"`
	Port int    `json:"port,omitempty"`
}

// ApprovalRequested is the payload of approval.requested (spec 052): a
// connection a command made that the egress gateway refused, the call that
// made it, and the verdict, ask when the person attending the session
// decides, block when nothing waits. More counts the further hosts the
// same call reached that no request of their own names.
type ApprovalRequested struct {
	ApprovalID  string      `json:"approval_id"`
	ToolUseID   string      `json:"tool_use_id"`
	Source      string      `json:"source"`
	Destination Destination `json:"destination"`
	Reason      string      `json:"reason"`
	Verdict     string      `json:"verdict"`
	More        int         `json:"more,omitempty"`
}

// ApprovalDecided is the payload of approval.decided: the person's answer
// to an approval.requested.
type ApprovalDecided struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
	By         Sender `json:"by"`
	Note       string `json:"note,omitempty"`
}

// Approval is one approval.requested of a log and what answered it: its
// approval.decided once the runner settled it, the first confirmation
// that names it, or a person's message appended while it waited, which
// denies it as a message denies an asked call (spec 012).
type Approval struct {
	Request      ApprovalRequested
	Seq          uint64
	Thread       string
	Decided      *ApprovalDecided
	Confirmation *UserToolConfirmation
	Message      *UserMessage
}

// Open reports whether the approval asks and nothing settled it yet.
func (a Approval) Open() bool { return a.Request.Verdict == "ask" && a.Decided == nil }

// Waiting reports whether the approval still waits for a person: open,
// with no confirmation and no message that answers it.
func (a Approval) Waiting() bool { return a.Open() && a.Confirmation == nil && a.Message == nil }

// Approvals are the approval.requested events of a log in order, each
// with what answered it.
func Approvals(evs []Event) []Approval {
	var out []Approval
	index := map[string]int{}
	for _, e := range evs {
		if e.Redacted() {
			continue
		}
		switch e.Type {
		case TypeApprovalRequested:
			var p ApprovalRequested
			if e.Decode(&p) != nil {
				continue
			}
			index[p.ApprovalID] = len(out)
			out = append(out, Approval{Request: p, Seq: e.Seq, Thread: e.Thread})
		case TypeApprovalDecided:
			var p ApprovalDecided
			if e.Decode(&p) != nil {
				continue
			}
			if i, ok := index[p.ApprovalID]; ok && out[i].Decided == nil {
				out[i].Decided = &p
			}
		case TypeUserToolConfirmation:
			var p UserToolConfirmation
			if e.Decode(&p) != nil || p.ApprovalID == "" {
				continue
			}
			if i, ok := index[p.ApprovalID]; ok && out[i].Confirmation == nil && out[i].Message == nil {
				out[i].Confirmation = &p
			}
		case TypeUserMessage:
			var p UserMessage
			if e.Decode(&p) != nil || p.Sender.Kind != SenderPerson {
				continue
			}
			for i := range out {
				if out[i].Waiting() {
					out[i].Message = &p
				}
			}
		}
	}
	return out
}

// Reach is what a session's log says it may reach (spec 052): its base
// network, nil for a session that recorded none, the hosts a person
// allowed in it, and the hosts a person denied in it, which are not asked
// about again. A fork's copied events are its parent's: each allow and
// each deny was for that session alone.
type Reach struct {
	Base    *Network
	Allowed []string
	Denied  []string
}

// NetworkOf is the reach of s from its header and its log.
func NetworkOf(s Session, evs []Event) Reach {
	r := Reach{Base: s.Network}
	hosts := map[string]string{}
	for _, e := range evs {
		if e.Redacted() || s.Copied(e) {
			continue
		}
		switch e.Type {
		case TypeNetworkChanged:
			var p NetworkChanged
			if e.Decode(&p) == nil && p.Source == NetworkFromPerson {
				r.Allowed = JoinHosts(r.Allowed, p.Added)
			}
		case TypeApprovalRequested:
			var p ApprovalRequested
			if e.Decode(&p) == nil {
				hosts[p.ApprovalID] = p.Destination.Host
			}
		case TypeApprovalDecided:
			var p ApprovalDecided
			if e.Decode(&p) == nil && p.Decision == DecisionDeny && hosts[p.ApprovalID] != "" {
				r.Denied = JoinHosts(r.Denied, []string{hosts[p.ApprovalID]})
			}
		}
	}
	return r
}

// Hosts are the hosts of the session's network beside the ones its
// machine joins: the base network's and the ones a person allowed.
func (r Reach) Hosts() []string {
	if r.Base == nil {
		return slices.Clone(r.Allowed)
	}
	return JoinHosts(r.Base.Hosts, r.Allowed)
}
