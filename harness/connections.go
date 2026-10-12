// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"

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

// connectionLine and mcpLine are one connection as the connections block
// names it.
type (
	connectionLine struct{ Title, GitURL, Env string }
	mcpLine        struct{ Title, Name, Reason string }
)

// connectionsBlock is the system block that names s's connections (spec
// 069), "" when it names none: each of kind http on a Cella machine, with
// how to clone, push and call its API, and each of kind mcp that mcp
// names, the drive's listing of its tools, by its tools' prefix or why it
// is not connected.
func connectionsBlock(s session.Session, mcp map[string]string) string {
	var http []connectionLine
	var tools []mcpLine
	for _, c := range s.Connections {
		switch c.Kind {
		case session.ConnectionHTTP:
			if s.Machine.Kind == session.MachineCella {
				http = append(http, connectionLine{Title: c.Title, GitURL: c.GitURL, Env: c.APIEnv()})
			}
		case session.ConnectionMCP:
			if reason, listed := mcp[c.Name]; listed {
				tools = append(tools, mcpLine{Title: c.Title, Name: c.Name, Reason: reason})
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
