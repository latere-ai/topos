// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"cmp"
	"context"
	"strings"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// CodeConnectionsUnavailable is a session error: the session's
// connections of kind http could not be given to its machine at a turn's
// start (spec 069). The turn goes on, and the next one gives them again.
const CodeConnectionsUnavailable = "connections_unavailable"

// HTTPConnections are the connections of cs of kind http as a Cella
// machine reaches them (spec 069).
func HTTPConnections(cs []session.Connection) []machine.HTTPConnection {
	var out []machine.HTTPConnection
	for _, c := range session.HTTPConnections(cs) {
		out = append(out, machine.HTTPConnection{Env: c.APIEnv(), Address: c.Address, GitAddress: c.GitAddress, GitURL: c.GitURL})
	}
	return out
}

// applyConnections gives the session's machine its connections of kind
// http as the session holds them at the turn's start, which a send may
// have replaced since the machine last took them (spec 069). A machine
// not opened yet takes them when it opens, and one that is not a Cella
// sandbox takes none: a host machine's git and environment are the
// person's own. A machine that cannot take them appends a session.error
// and the turn goes on: a connection is one service the session reaches,
// not the boundary it runs within.
func (t *turn) applyConnections(ctx context.Context) error {
	cm, ok := t.h.c.Machine.(machine.Connected)
	if !ok || t.h.c.Machine.Info().Kind != machine.KindCella {
		return nil
	}
	err := cm.ApplyConnections(ctx, HTTPConnections(t.s.Connections))
	if err == nil {
		return nil
	}
	se, eerr := t.sessionError(CodeConnectionsUnavailable, "The session's connections could not be given to its machine.", true, err.Error())
	if eerr != nil {
		return eerr
	}
	return t.commit(ctx, se)
}

// The session errors of a drive's listing of its mcp connections (specs
// 021 and 069): a server whose tools could not be listed, and a server
// some of whose tools' schemas use a keyword the validator ignores.
const (
	CodeMCPUnavailable   = "mcp_unavailable"
	CodeMCPSchemaReduced = "mcp_schema_reduced"
)

// MCPListing is how a drive's listing of a connection of kind mcp ended
// (spec 069): NotConnected with the connection service's Reason and the
// Scopes it named, Err when it failed otherwise, and Reduced the tools
// offered with a schema keyword the validator ignores. The zero listing
// offered every tool.
type MCPListing struct {
	NotConnected bool
	Reason       string
	Scopes       []string
	Err          string
	Reduced      []string
}

// recordListings appends, at the drive's first turn, how the listing of
// each mcp connection that did not offer every tool ended: a
// connection.requested for one not connected, which a client shows the
// person as one to connect again, a session.error mcp_unavailable for one
// that could not be listed, and a session.error mcp_schema_reduced naming
// the tools of one whose schemas were reduced. The session goes on.
func (t *turn) recordListings(ctx context.Context) error {
	if t.h.listed.Load() || len(t.h.c.MCP) == 0 {
		return nil
	}
	var batch []session.Event
	for _, c := range t.s.Connections {
		l, ok := t.h.c.MCP[c.Name]
		if !ok || c.Kind != session.ConnectionMCP {
			continue
		}
		var e session.Event
		var err error
		switch {
		case l.NotConnected:
			e, err = t.event(session.TypeConnectionRequested, session.ConnectionRequested{Name: c.Name, Title: c.Title, Kind: c.Kind, Reason: l.Reason, Scopes: l.Scopes})
		case l.Err != "":
			e, err = t.sessionError(CodeMCPUnavailable, "The tools of a server the session reaches could not be listed.", true, c.Name+": "+l.Err)
		case len(l.Reduced) > 0:
			e, err = t.sessionError(CodeMCPSchemaReduced, "Some tools of a server the session reaches are checked against part of their schema.", false, c.Name+": "+strings.Join(l.Reduced, ", "))
		default:
			continue
		}
		if err != nil {
			return err
		}
		batch = append(batch, e)
	}
	if len(batch) > 0 {
		if err := t.commit(ctx, batch...); err != nil {
			return err
		}
	}
	t.h.listed.Store(true)
	return nil
}

// connectionLine and mcpLine are one connection as the connections block
// names it.
type (
	connectionLine struct{ Title, GitURL, Env string }
	mcpLine        struct{ Title, Name, Reason string }
)

// connectionsBlock is the system block that names s's connections (spec
// 069), "" when it names none: each of kind http on a Cella machine, with
// how to clone, push and call its API, and each of kind mcp whose listing
// mcp holds, by its tools' prefix or why it is not connected; one whose
// listing failed otherwise offers nothing to name.
func connectionsBlock(s session.Session, mcp map[string]MCPListing) string {
	var http []connectionLine
	var tools []mcpLine
	for _, c := range s.Connections {
		switch c.Kind {
		case session.ConnectionHTTP:
			if s.Machine.Kind == session.MachineCella {
				http = append(http, connectionLine{Title: c.Title, GitURL: c.GitURL, Env: c.APIEnv()})
			}
		case session.ConnectionMCP:
			switch l, listed := mcp[c.Name]; {
			case !listed || l.Err != "":
			case l.NotConnected:
				tools = append(tools, mcpLine{Title: c.Title, Name: c.Name, Reason: cmp.Or(l.Reason, "no reason given")})
			default:
				tools = append(tools, mcpLine{Title: c.Title, Name: c.Name})
			}
		}
	}
	if len(http) == 0 && len(tools) == 0 {
		return ""
	}
	return prompts.Render(prompts.ContextConnections, prompts.Data{
		"Branch": session.Branch(s), "Token": session.ConnectionsToken, "HTTP": http, "MCP": tools,
	})
}
