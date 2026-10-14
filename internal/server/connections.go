// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/url"
	"strings"

	"latere.ai/x/topos/internal/auth"
	"latere.ai/x/topos/session"
)

// ConnectionsRule is what the create and the send routes state of a
// session's connections (spec 069).
var ConnectionsRule = fmt.Sprintf("The authorizer's allow of session.create, session.fork and session.send may name the session's connections, at most %d, "+
	"each {name, kind, title, state, audience, address, git_address, git_url, read_only, hidden, account, api_url, api_example, api_notes}: kind http, an API at address and a git host at git_url reached through git_address from the session's sandbox, "+
	"or mcp, a remote MCP server at address whose tools the model is offered as mcp__<name>__<tool>. "+
	"read_only and hidden, of mcp alone, are each at most %d globs over the server's tool names in path.Match syntax of 1 to %d bytes: a tool a read_only glob matches reads and every other reaches outside, "+
	"and a tool a hidden glob matches is never offered to the model, a call of it answered unknown_tool without reaching the server. "+
	"account, optional, is the account at the other service the connection acts as, one line of at most %d characters, api_url, optional and of http alone, the service's own API base URL that address stands in for, an https URL, and api_example, optional and of http alone, the whole path of one documented call after api_url, version prefix included, at most %d bytes with no space, control character, ?, #, $, quote, backtick or backslash, and api_notes, optional and of http alone, one line of at most %d characters the model is told of the API beyond the example, in which {env} stands for the name of the connection's variable; the model is told each. "+
	"state is absent for a connected connection, or available for a service the installation offers and the initiator has not connected, which names name, kind and title alone and which the session reaches nothing of; the bound counts both. "+
	"A create's and a fork's are the session's, a fork's never its parent's, and the session answers them as connections; "+
	"a send's allow replaces them, appending session.connections_changed {connections} straight before the event when the list differs, and an allow that names none leaves the session with none. "+
	"An http connection names the audience of TOPOS_CONNECTIONS_AUDIENCE and addresses on the host of TOPOS_CONNECTIONS_URL, and a connection that breaks a rule is authorizer_unavailable naming it. "+
	"A call of an mcp connection's tool that the connection service answers 409 connection_not_connected, and a drive's listing of its tools that meets it, append connection.requested {name, title, kind, reason, scopes, tool_use_id}, tool_use_id absent for a listing, which a client shows the person as a service to connect again. "+
	"While the session has an available connection, of kind mcp or, on a Cella machine, http, the model of its own thread is offered request_connection {name, reason}, whose call appends connection.requested {name, title, kind, reason, tool_use_id}, reason the model's sentence on what it needs the service for, which a client shows the person as a service to connect.",
	session.MaxConnections, session.MaxReadOnlyTools, session.MaxReadOnlyGlob, session.MaxConnectionAccount, session.MaxConnectionAPIExample, session.MaxConnectionAPINotes)

// connections are an allow's connections as a session holds them (spec
// 069): a connected http connection, which a sandbox reaches through the
// one Secret mounted for the installation's connection service, names
// that service's audience and addresses on its host, and an installation
// with none refuses it. A connection the server cannot apply refuses the
// request authorizer_unavailable, as a member that does not decode does.
func (s *Server) connections(cs []session.Connection) ([]session.Connection, error) {
	host := ""
	if u, err := url.Parse(s.o.ConnectionsURL); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	for i, c := range cs {
		// An available connection names no audience and no address: the
		// session reaches nothing of it until the person connects it.
		if c.Kind != session.ConnectionHTTP || !c.Connected() {
			continue
		}
		var why string
		switch {
		case host == "":
			why = "is of kind http, and this server names no connection service (TOPOS_CONNECTIONS_URL)"
		case c.Audience != s.o.ConnectionsAudience:
			why = fmt.Sprintf("names the audience %q, not the connection service's, %q", c.Audience, s.o.ConnectionsAudience)
		case !onHost(c.Address, host) || (c.GitAddress != "" && !onHost(c.GitAddress, host)):
			why = "names an address on another host than the connection service's, " + host
		}
		if why != "" {
			err := fmt.Errorf("limits.connections[%d] %s %s", i, c.Name, why)
			return nil, &apiError{code: auth.CodeAuthorizerUnavailable, detail: err.Error(), err: err}
		}
	}
	return cs, nil
}

// onHost reports whether raw, an address CheckConnections accepted, is on
// host.
func onHost(raw, host string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.ToLower(u.Hostname()) == host
}
