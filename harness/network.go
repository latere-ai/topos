// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"latere.ai/x/pkg/hostmatch"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// CodeNetworkUnavailable is the code of a session.error when the session's
// network could not be given to its machine: a widening for a person's
// allow, or the network a send named, which Cella or its admission
// refused (spec 052).
const CodeNetworkUnavailable = "network_unavailable"

// MaxAsksPerCall is how many approval.requested one command's refused
// connections append; the hosts past them are counted in the last one's
// more (spec 052).
const MaxAsksPerCall = 3

// Network is a session's network as the harness decides by it (spec 052):
// its mode, the hosts it reaches under allowlist, whether a first contact
// with a host outside it asks the person attending the session, and the
// hosts a person denied in it, which are not asked about again. The hosts
// of the session's named secrets are the machine's to join, so they are
// not among Hosts.
type Network struct {
	Mode   string
	Hosts  []string
	Ask    bool
	Denied []string
}

// EffectiveNetwork is the network of s from its header and its log, for an
// agent whose machine names mode and hosts: the base network the header
// records, or the agent's mode with ask false for a session that records
// none. Under allowlist its hosts are the base network's, the ones a
// person allowed, the agent's and the git hosts of the session's
// repositories; open and none name none and never ask.
func EffectiveNetwork(s session.Session, evs []session.Event, agentMode string, agentHosts []string) Network {
	r := session.NetworkOf(s, evs)
	n := Network{Mode: cmp.Or(agentMode, session.NetworkAllowlist), Denied: r.Denied}
	if r.Base != nil {
		n.Mode, n.Ask = cmp.Or(r.Base.Mode, session.NetworkAllowlist), r.Base.Ask
	}
	if n.Mode != session.NetworkAllowlist {
		n.Ask = false
		return n
	}
	n.Hosts = session.JoinHosts(r.Hosts(), agentHosts, RepositoryHosts(s))
	return n
}

// RepositoryHosts are the git hosts of the session's repositories, which
// an allowlist joins so the session reaches its repositories (spec 009).
func RepositoryHosts(s session.Session) []string {
	var out []string
	for _, r := range session.Repositories(s) {
		if u, err := url.Parse(r.URL); err == nil && u.Hostname() != "" {
			out = append(out, u.Hostname())
		}
	}
	return out
}

// Machine is the egress the network gives the session's machine.
func (n Network) Machine() machine.Network {
	return machine.Network{Mode: n.Mode, Hosts: slices.Clone(n.Hosts)}
}

// Inside reports whether the network reaches host: any host under open,
// none under none, and under allowlist a host one of its patterns names.
func (n Network) Inside(host string) bool {
	switch n.Mode {
	case session.NetworkOpen:
		return true
	case session.NetworkNone:
		return false
	}
	return hostmatch.New(n.Hosts, strings.ToLower).Matches(strings.ToLower(host))
}

// asks reports whether a first contact with host outside the network asks
// the person: an allowlist that asks, and a host no person denied.
func (n Network) asks(host string) bool {
	return n.Mode == session.NetworkAllowlist && n.Ask && !slices.Contains(n.Denied, host)
}

// with is the network with host joined, as a person's allow widens it.
func (n Network) with(host string) Network {
	n.Hosts = session.JoinHosts(n.Hosts, []string{host})
	return n
}

// fetchDecision is the network's decision on a web_fetch (spec 052): a
// host inside the network is allowed, one outside asks when the network
// asks and is blocked with the result the model reads otherwise. It
// decides nothing for another tool, a policy with no network, or a fetch
// whose URL names no host.
func (p Policy) fetchDecision(name string, input json.RawMessage) (Decision, bool) {
	n := p.Network
	if n == nil || name != tools.NameWebFetch {
		return Decision{}, false
	}
	host := fetchHost(input)
	switch {
	case host == "":
		return Decision{}, false
	case n.Inside(host):
		return Decision{Verdict: VerdictAllow, Reason: session.ReasonInsideNetwork}, true
	case n.asks(host):
		return Decision{Verdict: VerdictAsk, Reason: session.ReasonOutsideNetwork}, true
	}
	return Decision{Verdict: VerdictBlock, Reason: prompts.Render(prompts.CallOutsideNetwork, prompts.Data{"Host": host})}, true
}

// network is the session's network as the turn decides by it now, nil on
// a machine whose egress no session network governs: the host machine
// applies what its driver can. A first contact asks only in a session a
// person attends (spec 039), since nothing times out an ask.
func (t *turn) network() *Network {
	if t.h.c.Machine.Info().Kind != machine.KindCella {
		return nil
	}
	n := EffectiveNetwork(t.s, t.events(), t.h.c.Policy.EgressMode, t.h.c.Policy.Egress)
	n.Ask = n.Ask && t.s.Attended
	return &n
}

// applyNetwork gives the session's machine the network it has now before
// the turn's first request, which a send's change may have moved since
// the machine last took it (spec 052). A machine not opened yet takes it
// when it opens. A network the machine cannot take ends the turn: a
// session never runs on a boundary its authorizer did not name.
func (t *turn) applyNetwork(ctx context.Context) error {
	n := t.network()
	nm, ok := t.h.c.Machine.(machine.Networked)
	if n == nil || !ok {
		return nil
	}
	err := nm.ApplyNetwork(ctx, n.Machine())
	if err == nil {
		return nil
	}
	se, eerr := t.sessionError(CodeNetworkUnavailable, "The session's network could not be given to its machine.", true, err.Error())
	if eerr != nil {
		return eerr
	}
	return t.finish(ctx, session.StopError, CodeNetworkUnavailable, se)
}

// widen gives the session's machine its network with host joined, as a
// person's allow does (spec 052), and records the allow in a
// session.network_changed naming the call or the approval it answered. The
// machine is opened first, so the widening reaches a running sandbox and
// a refusal is known before anything is recorded: a widening the machine
// refuses, or a machine that cannot be had, records nothing and is
// returned.
func (t *turn) widen(ctx context.Context, n Network, host, toolUseID, approvalID string) error {
	if err := machine.Open(ctx, t.h.c.Machine); err != nil {
		return err
	}
	if nm, ok := t.h.c.Machine.(machine.Networked); ok {
		if err := nm.ApplyNetwork(ctx, n.with(host).Machine()); err != nil {
			return err
		}
	}
	e, err := t.event(session.TypeNetworkChanged, session.NetworkChanged{
		Added: []string{host}, Source: session.NetworkFromPerson, ToolUseID: toolUseID, ApprovalID: approvalID,
	})
	if err != nil {
		return err
	}
	return t.commit(ctx, e)
}

// admitFetch readies an allowed web_fetch to run (spec 052): a host
// outside the session's network is joined to it first, as the person's
// allow asked. It answers the result of a fetch that must not run: one
// whose host the network no longer asks about, whose machine cannot be
// had, or whose widening the machine refused, which also appends a
// session.error network_unavailable. A core's refusal for spend comes
// back beside the result, to stop the turn once it is recorded.
func (t *turn) admitFetch(ctx context.Context, use session.AgentToolUse) (*tools.Result, error) {
	n := t.network()
	if n == nil || use.Name != tools.NameWebFetch {
		return nil, nil
	}
	host := fetchHost(use.Input)
	switch {
	case host == "" || n.Inside(host):
		return nil, nil
	case !n.asks(host):
		res := tools.Text(tools.OutcomeBlocked, prompts.Render(prompts.CallOutsideNetwork, prompts.Data{"Host": host}))
		return &res, nil
	}
	// The fetch opens the machine anyway; one that cannot be had answers
	// the call as any tool's open does.
	if err := machine.Open(ctx, t.h.c.Machine); err != nil {
		res, uerr := t.unopened(ctx, err)
		if uerr != nil && !isSpent(uerr) {
			return nil, uerr
		}
		return &res, uerr
	}
	err := t.widen(ctx, *n, host, use.ToolUseID, "")
	if err == nil {
		return nil, nil
	}
	if _, lost := errors.AsType[*appendError](err); lost {
		return nil, err
	}
	se, eerr := t.sessionError(CodeNetworkUnavailable, "The session's network could not be widened to "+host+".", true, err.Error())
	if eerr != nil {
		return nil, eerr
	}
	if cerr := t.commit(ctx, se); cerr != nil {
		return nil, cerr
	}
	res := tools.Text(tools.OutcomeError, prompts.Render(prompts.CallNetworkUnavailable, prompts.Data{"Host": host, "Error": err.Error()}))
	return &res, nil
}

// refusals reads the connections the gateway refused while a bash call
// ran, from since, and appends an approval.requested for each of the first
// MaxAsksPerCall hosts outside the session's network that no person denied
// and no open request names, the further ones counted in the last one's
// more (spec 052). The verdict is ask in the session's own thread of a
// session whose network asks and that a person attends, which leaves the
// step waiting for a confirmation; block otherwise, recorded with nothing
// waiting. A read that fails is a session.error, and the call stands.
func (t *turn) refusals(ctx context.Context, toolUseID string, since time.Time, w *waits) error {
	n := t.network()
	nm, ok := t.h.c.Machine.(machine.Networked)
	if n == nil || !ok || n.Mode != session.NetworkAllowlist {
		return nil
	}
	conns, err := nm.Refused(ctx, since)
	if err != nil {
		se, eerr := t.sessionError(CodeNetworkUnavailable, "The connections the command made could not be read.", true, err.Error())
		if eerr != nil {
			return eerr
		}
		return t.commit(ctx, se)
	}
	pending := map[string]bool{}
	for _, a := range session.Approvals(t.events()) {
		if a.Open() {
			pending[a.Request.Destination.Host] = true
		}
	}
	var hosts []machine.Connection
	for _, c := range conns {
		if !n.Inside(c.Host) && !slices.Contains(n.Denied, c.Host) && !pending[c.Host] {
			hosts = append(hosts, c)
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	verdict := VerdictBlock
	if t.thread == "" && n.Ask {
		verdict = VerdictAsk
	}
	shown := hosts[:min(len(hosts), MaxAsksPerCall)]
	batch := make([]session.Event, 0, len(shown))
	for i, c := range shown {
		p := session.ApprovalRequested{
			ApprovalID: session.NewID(session.PrefixApproval), ToolUseID: toolUseID, Source: session.ApprovalFromEgress,
			Destination: session.Destination{Host: c.Host, Port: c.Port}, Reason: session.ReasonConnectionOutside, Verdict: string(verdict),
		}
		if i == len(shown)-1 {
			p.More = len(hosts) - len(shown)
		}
		e, err := t.event(session.TypeApprovalRequested, p)
		if err != nil {
			return err
		}
		batch = append(batch, e)
	}
	if err := t.commit(ctx, batch...); err != nil {
		return err
	}
	if verdict == VerdictAsk {
		w.confirmation = true
	}
	return nil
}

// settleApprovals settles the session's open approvals a person answered
// (spec 052): an allow widens the session's network to the host and
// appends approval.decided, a deny or a person's message in place of an
// answer appends approval.decided with deny. An allow whose widening the
// machine refused appends a session.error network_unavailable before its
// approval.decided, and the model reads that the host stays out of reach.
// An approval nothing answered leaves the step waiting for a
// confirmation.
func (t *turn) settleApprovals(ctx context.Context, w *waits) error {
	for _, a := range session.Approvals(t.events()) {
		if !a.Open() || a.Thread != t.thread {
			continue
		}
		d := session.ApprovalDecided{ApprovalID: a.Request.ApprovalID, Decision: session.DecisionDeny}
		switch {
		case a.Confirmation != nil:
			d.Decision, d.By, d.Note = a.Confirmation.Decision, a.Confirmation.Sender, a.Confirmation.Note
		case a.Message != nil:
			d.By = a.Message.Sender
		default:
			w.confirmation = true
			continue
		}
		if d.Decision == session.DecisionAllow {
			if err := t.allowApproval(ctx, a.Request); err != nil {
				return err
			}
		}
		e, err := t.event(session.TypeApprovalDecided, d)
		if err != nil {
			return err
		}
		if err := t.commit(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// allowApproval widens the session's network to an allowed request's
// host, or records why it could not.
func (t *turn) allowApproval(ctx context.Context, r session.ApprovalRequested) error {
	n := t.network()
	if n == nil {
		n = &Network{Mode: session.NetworkAllowlist}
	}
	err := t.widen(ctx, *n, r.Destination.Host, "", r.ApprovalID)
	if err == nil {
		return nil
	}
	if _, lost := errors.AsType[*appendError](err); lost {
		return err
	}
	se, eerr := t.sessionError(CodeNetworkUnavailable, "The session's network could not be widened to "+r.Destination.Host+".", true, err.Error())
	if eerr != nil {
		return eerr
	}
	return t.commit(ctx, se)
}
