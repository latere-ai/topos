// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"latere.ai/x/topos/harness/tools"
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
	// A rollout's run reaches no connection, whatever its header holds
	// (spec 077, "A run is isolated").
	var conns []session.Connection
	if !t.s.Isolated() {
		conns = t.s.Connections
	}
	err := cm.ApplyConnections(ctx, HTTPConnections(conns))
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
// offered every tool the connection does not hide.
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

// connectionLine, mcpLine and availableLine are one connection as the
// connections block names it.
type (
	connectionLine struct{ Title, Account, APIURL, APIExample, GitURL, Env, Notes string }
	mcpLine        struct{ Title, Account, Name, Reason string }
	availableLine  struct{ Title, Name string }
)

// reachesHTTP reports whether s's machine m is given the session's
// connections of kind http (spec 069): a Cella machine, unless it says it
// reaches no connection service, as a sandbox that opened without the
// service's Secret does. The model is then told of none of them.
func reachesHTTP(s session.Session, m machine.Machine) bool {
	if s.Machine.Kind != session.MachineCella {
		return false
	}
	cm, ok := m.(machine.Connected)
	return !ok || cm.ReachesConnections()
}

// connectionsBlock is the system block that names s's connections (spec
// 069), "" when it names none: each connected one of kind http when the
// machine is given them, http, with the account it acts as, the API base
// URL its variable stands in for, its example call written whole, its
// notes with {env} written as its variable's name, and how
// to clone and push; each of kind mcp whose listing mcp holds, by its
// account and its tools' prefix or why it is not connected, one whose
// listing failed otherwise offering nothing to name; and, on the
// session's own thread, which holds request_connection, each the person
// may be asked to connect.
func connectionsBlock(s session.Session, own, http bool, mcp map[string]MCPListing) string {
	var lines []connectionLine
	var tools []mcpLine
	var available []availableLine
	if own {
		for _, c := range requestable(s, http) {
			available = append(available, availableLine{Title: c.Title, Name: c.Name})
		}
	}
	for _, c := range s.Connections {
		if !c.Connected() {
			continue
		}
		switch c.Kind {
		case session.ConnectionHTTP:
			if http {
				lines = append(lines, connectionLine{Title: c.Title, Account: c.Account, APIURL: c.APIURL, APIExample: c.APIExample, GitURL: c.GitURL, Env: c.APIEnv(),
					Notes: strings.ReplaceAll(c.APINotes, "{env}", c.APIEnv())})
			}
		case session.ConnectionMCP:
			switch l, listed := mcp[c.Name]; {
			case !listed || l.Err != "":
			case l.NotConnected:
				tools = append(tools, mcpLine{Title: c.Title, Account: c.Account, Name: c.Name, Reason: cmp.Or(l.Reason, "no reason given")})
			default:
				tools = append(tools, mcpLine{Title: c.Title, Account: c.Account, Name: c.Name})
			}
		}
	}
	if len(lines) == 0 && len(tools) == 0 && len(available) == 0 {
		return ""
	}
	return prompts.Render(prompts.ContextConnections, prompts.Data{
		"Branch": session.Branch(s), "Token": session.ConnectionsToken, "HTTP": lines, "MCP": tools, "Available": available,
	})
}

// requestable are s's available connections the person may be asked to
// connect (spec 069): each of kind mcp, and each of kind http when the
// machine is given them, http, as a Cella machine that reaches the
// connection service is, so a session on a host machine, or in a sandbox
// that opened without the service's Secret, is never told of a service
// connecting would not give it.
func requestable(s session.Session, http bool) []session.Connection {
	// A rollout's run asks for no connection: no person attends it, and it
	// reaches none (spec 077, "A run is isolated").
	if s.Isolated() {
		return nil
	}
	var out []session.Connection
	for _, c := range s.Connections {
		if !c.Connected() && (c.Kind == session.ConnectionMCP || http) {
			out = append(out, c)
		}
	}
	return out
}

// ToolRequestConnection is the name of the tool that asks the person to
// connect a service the installation offers (spec 069). It is a harness
// tool, as question is: the harness adds it to the registry of the
// session's own thread while the session has a connection the person may
// be asked to connect, and no thread the agent tool starts holds it.
const ToolRequestConnection = session.ToolRequestConnection

// RequestConnectionSchema is the input schema of request_connection, its
// bound on a reason stated from session's constant.
var RequestConnectionSchema = json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "minLength": 1, "description": "The service's name, as the connections block lists it."},
    "reason": {"type": "string", "minLength": 1, "maxLength": %d, "pattern": "\\S", "description": "One sentence for the person: what you need the service for."}
  },
  "required": ["name", "reason"],
  "additionalProperties": false
}`, session.MaxConnectionReason))

// requestConnectionTool is request_connection on one turn of the session's
// own thread.
type requestConnectionTool struct{ t *turn }

func (r requestConnectionTool) Definition() tools.Definition {
	return tools.Definition{Name: ToolRequestConnection, Description: prompts.Text(prompts.ToolRequestConnection), InputSchema: RequestConnectionSchema}
}

// Properties: the call reaches nothing and changes nothing, so it opens no
// machine, scores 0.0 and is allowed in every mode, plan included, and it
// runs beside the step's other calls.
func (r requestConnectionTool) Properties() tools.Properties {
	return tools.Properties{Parallel: true, Effect: tools.EffectNone}
}

// Run asks the person to connect the service the call names: its result
// tells the model to end the turn, and connection.requested follows the
// result for a client to show the person (spec 069). A name the session
// may not ask for is an error result naming those it may.
func (r requestConnectionTool) Run(_ context.Context, c tools.Call) (tools.Result, error) {
	var in struct {
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, fmt.Errorf("harness: decode a request_connection call: %w", err)
	}
	offered := requestable(r.t.s, reachesHTTP(r.t.s, r.t.h.c.Machine))
	i := slices.IndexFunc(offered, func(c session.Connection) bool { return c.Name == in.Name })
	if i < 0 {
		names := make([]string, len(offered))
		for j, c := range offered {
			names[j] = c.Name
		}
		return tools.Text(tools.OutcomeError, prompts.Render(prompts.RequestConnectionNotAvailable, prompts.Data{"Name": in.Name, "Names": strings.Join(names, ", ")})), nil
	}
	conn := offered[i]
	res := tools.Text(tools.OutcomeOK, prompts.Render(prompts.RequestConnectionAsked, prompts.Data{"Title": conn.Title}))
	res.Requested = &session.ConnectionRequested{Name: conn.Name, Title: conn.Title, Kind: conn.Kind, Reason: oneLineReason(in.Reason)}
	return res, nil
}

// oneLineReason is a call's reason as a client shows the person: on one
// line, its runs of white space as one space and no control character.
func oneLineReason(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)), " ")
}
