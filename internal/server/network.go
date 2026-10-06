// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"slices"

	"latere.ai/x/topos/authorizer"
	"latere.ai/x/topos/harness"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// NetworkRule is what the create route states of a session's network and
// of the initiator's instructions (specs 052 and 053).
var NetworkRule = fmt.Sprintf("The session's network is what its machine may reach, network {mode, hosts, ask, source}: mode open, allowlist or none, "+
	"the one the authorizer's allow names with source authorizer, or the agent's spec.machine egressMode with ask false and source agent. "+
	"Under allowlist the agent's spec.machine.egress, the hosts of the session's named secrets and the git hosts of its repositories are always joined. "+
	"A web_fetch of a host inside the network runs without asking, its reason %q; one outside it asks, its reason %q, when ask is true and the session is attended, and is blocked otherwise. "+
	"The allow may carry the initiator's standing instructions, at most %d bytes of UTF-8, which the session records as instructions and its model reads after its agent's own; no send changes them.",
	session.ReasonInsideNetwork, session.ReasonOutsideNetwork, authorizer.MaxInitiatorInstructions)

// SendNetworkRule is what the send route states of a session's network and
// of the approvals a person answers (spec 052).
var SendNetworkRule = fmt.Sprintf("The allow may also name the session's network {mode, hosts, ask}: session.network_changed {added, removed, mode, ask, source: authorizer} is then appended straight before the event, "+
	"and the session's machine runs the new network before a call of the turn runs on it; the hosts a person allowed in the session stay. "+
	"A command's connection the gateway refused outside the session's network appends approval.requested {approval_id, tool_use_id, source: egress, destination {host, port}, reason %q, verdict, more} after the call's result, "+
	"at most %d per call, more counting the further hosts on the last; verdict ask, in an attended session whose network asks, idles the session with tool_confirmation, and block records it with nothing waiting. "+
	"A user.tool_confirmation names exactly one of tool_use_id, a call that waits, and approval_id, an approval.requested that asks and that nothing answered, and remember is invalid_request with approval_id. "+
	"An allow widens the session's network to the host for the rest of the session, appending session.network_changed {added, source: person, approval_id} and approval.decided {approval_id, decision, by, note}; "+
	"a deny, or a person's user.message in place of an answer, appends approval.decided with deny, and the host is not asked again in the session. "+
	"An allowed web_fetch outside the network widens it the same way before it runs, with tool_use_id; a widening Cella refuses closes the call with session.error %s and changes nothing. ",
	session.ReasonConnectionOutside, harness.MaxAsksPerCall, harness.CodeNetworkUnavailable)

// createdNetwork is the base network a create records: the allow's, or
// the agent's spec.machine egressMode with ask false. The agent's hosts are
// not the base network's: they join every allowlist the session runs.
func createdNetwork(n *authorizer.Network, m v1.Machine) *session.Network {
	if n == nil {
		return &session.Network{Mode: m.Mode(), Source: session.NetworkFromAgent}
	}
	return &session.Network{Mode: n.Mode, Hosts: slices.Clone(n.Hosts), Ask: n.Ask, Source: session.NetworkFromAuthorizer}
}

// sentNetwork is the change an allow of a send makes to the session's
// base network (spec 052), nil when it names none or the one the session
// runs. A session that recorded no network takes the whole of the one
// named, its mode included.
func sentNetwork(sess session.Session, n *authorizer.Network) *session.NetworkChanged {
	if n == nil {
		return nil
	}
	next := session.Network{Mode: n.Mode, Hosts: n.Hosts, Ask: n.Ask}
	c, changed := session.Change(sess.Network, next)
	if sess.Network == nil {
		c.Mode, changed = next.Mode, true
	}
	if !changed {
		return nil
	}
	return &c
}
