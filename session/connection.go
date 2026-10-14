// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The kinds of a connection (spec 069): http reaches a service's API and
// its git host through the connection service's addresses from the
// session's sandbox, and mcp is a remote Model Context Protocol server
// the runner reaches.
const (
	ConnectionHTTP = "http"
	ConnectionMCP  = "mcp"
)

// ConnectionKinds are the kinds a connection takes.
var ConnectionKinds = []string{ConnectionHTTP, ConnectionMCP}

// ConnectionAvailable is the state of a connection the installation
// offers and the initiator has not connected (spec 069): the session
// cannot reach it, and the model may ask the person to connect it. A
// connection with no state is connected.
const ConnectionAvailable = "available"

// ToolRequestConnection is the name of the tool that asks the person to
// connect a service the installation offers (spec 069). The harness
// offers it to the session's own thread while the session has a
// connection it may ask for, and no client tool takes the name.
const ToolRequestConnection = "request_connection"

// The bounds of a session's connections (spec 069).
const (
	// MaxConnections is the most connections an allow names.
	MaxConnections = 32
	// MaxConnectionName is the most characters of a connection's name,
	// which leaves room for the name in a tool name of mcp__<name>__<tool>.
	MaxConnectionName = 32
	// MaxConnectionTitle is the most characters of a connection's title.
	MaxConnectionTitle = 100
	// MaxConnectionAudience is the most bytes of a connection's audience.
	MaxConnectionAudience = 256
	// MaxReadOnlyTools is the most globs of one connection's read_only, and
	// of its hidden.
	MaxReadOnlyTools = 64
	// MaxReadOnlyGlob is the most bytes of one glob of read_only or hidden.
	MaxReadOnlyGlob = 128
	// MaxConnectionReason is the most characters of the reason a
	// request_connection call gives the person.
	MaxConnectionReason = 200
	// MaxConnectionAccount is the most characters of the account a
	// connection acts as.
	MaxConnectionAccount = 256
	// MaxConnectionAPIExample is the most bytes of the path of a
	// connection's example call.
	MaxConnectionAPIExample = 256
	// MaxConnectionAPINotes is the most characters of what an http
	// connection's notes tell the model of its API.
	MaxConnectionAPINotes = 800
)

// ConnectionsToken is the variable a sandbox holds the placeholder of its
// session's token for the installation's connection service under, which
// the egress gateway swaps for the token on requests to that service.
const ConnectionsToken = "CONNECTIONS_TOKEN"

// Connection is one connection an allow names for a session (spec 069): a
// person's connection to another service, which the installation's
// connection service serves at addresses of its own and reaches with the
// person's credential. The session reaches the addresses with its own
// token for Audience and holds no credential of the other service.
type Connection struct {
	// Name is a lowercase DNS label that starts with a letter, unique in
	// the session's list; it names the connection's variable and tools.
	Name string `json:"name"`
	// Kind is ConnectionHTTP or ConnectionMCP.
	Kind string `json:"kind"`
	// Title is what the model and a person call the connection.
	Title string `json:"title"`
	// State is ConnectionAvailable for a service the installation offers
	// and the initiator has not connected, which names no audience and no
	// address, and empty for a connected one.
	State string `json:"state,omitempty"`
	// Audience is the core the session's token for the connection is
	// addressed to; empty on an available connection.
	Audience string `json:"audience,omitempty"`
	// Address is the forwarding address of an http connection and the MCP
	// address of an mcp one; empty on an available connection.
	Address string `json:"address,omitempty"`
	// GitAddress is an http connection's git address, and GitURL the git
	// host the person's repositories live at, which git in the sandbox
	// reaches through GitAddress; both or neither.
	GitAddress string `json:"git_address,omitempty"`
	GitURL     string `json:"git_url,omitempty"`
	// ReadOnly are an mcp connection's globs over its tool names; a tool a
	// glob matches reads, and every other tool reaches outside.
	ReadOnly []string `json:"read_only,omitempty"`
	// Hidden are an mcp connection's globs over its tool names; a tool a
	// glob matches is never offered to the model, so a call of it is
	// answered as a call of no tool and never reaches the server. It wins
	// over ReadOnly.
	Hidden []string `json:"hidden,omitempty"`
	// Account is the account at the other service the connection acts as,
	// such as a login or an email address, which the model is told; empty
	// when the allow names none, and on an available connection.
	Account string `json:"account,omitempty"`
	// APIURL is an http connection's service's own API base URL, which
	// Address stands in for, so the model can map the service's documented
	// calls onto the connection's variable; empty when the allow names
	// none, and on an available connection.
	APIURL string `json:"api_url,omitempty"`
	// APIExample is the whole path of one documented call of an http
	// connection's API after APIURL, version prefix included, such as
	// /v1/users/me/messages, which the block writes out as a call through
	// the connection's variable; empty when the allow names none, and on
	// an available connection.
	APIExample string `json:"api_example,omitempty"`
	// APINotes is what the model is told of an http connection's API
	// beyond its example, one line of plain text, such as how a document's
	// text is read or where an upload goes; {env} in it stands for the name
	// of the connection's variable. Empty when the allow names none, and on
	// an available connection.
	APINotes string `json:"api_notes,omitempty"`
}

// connectionName is a lowercase DNS label that starts with a letter, so
// it names an environment variable too.
var connectionName = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// Connected reports whether c is connected, so the session reaches it.
func (c Connection) Connected() bool { return c.State == "" }

// APIEnv is the variable that names an http connection's forwarding
// address in the sandbox's commands: its name upper-cased with each "-"
// as "_", and _API_URL.
func (c Connection) APIEnv() string {
	return strings.ReplaceAll(strings.ToUpper(c.Name), "-", "_") + "_API_URL"
}

// ReadsOnly reports whether a glob of c's ReadOnly matches tool.
func (c Connection) ReadsOnly(tool string) bool {
	for _, g := range c.ReadOnly {
		if ok, err := path.Match(g, tool); err == nil && ok {
			return true
		}
	}
	return false
}

// Hides reports whether a glob of c's Hidden matches tool, path.Match over
// the whole name as ReadsOnly reads.
func (c Connection) Hides(tool string) bool {
	for _, g := range c.Hidden {
		if ok, err := path.Match(g, tool); err == nil && ok {
			return true
		}
	}
	return false
}

// CheckConnections reports why cs cannot be a session's connections: at
// most MaxConnections, connected and available together, each with a
// name, a kind and a title of the rules spec 069 gives; a connected one
// with an audience and an address, its git address and git URL on an
// http connection alone and both or neither, its API URL and the path of
// its example call on an http connection alone, its read_only and hidden
// globs on an mcp connection alone, and an account on one line; an
// available one with none of those; and no name or git URL named twice. A
// git URL and an API URL are written without a trailing "/" in the list it
// answers, which is a copy.
func CheckConnections(cs []Connection) ([]Connection, error) {
	if len(cs) > MaxConnections {
		return nil, fmt.Errorf("connections names %d connections, more than %d", len(cs), MaxConnections)
	}
	out := slices.Clone(cs)
	names, gits := map[string]bool{}, map[string]bool{}
	for i := range out {
		c := &out[i]
		c.ReadOnly, c.Hidden = slices.Clone(c.ReadOnly), slices.Clone(c.Hidden)
		if err := checkConnection(c); err != nil {
			return nil, fmt.Errorf("connections[%d]: %w", i, err)
		}
		if names[c.Name] {
			return nil, fmt.Errorf("connections[%d] names %s a second time", i, c.Name)
		}
		names[c.Name] = true
		if c.GitURL != "" {
			if gits[c.GitURL] {
				return nil, fmt.Errorf("connections[%d] names the git URL %s a second time", i, c.GitURL)
			}
			gits[c.GitURL] = true
		}
	}
	return out, nil
}

func checkConnection(c *Connection) error {
	if len(c.Name) > MaxConnectionName || !connectionName.MatchString(c.Name) {
		return fmt.Errorf("the name %q is not a lowercase DNS label of at most %d characters that starts with a letter", c.Name, MaxConnectionName)
	}
	if !slices.Contains(ConnectionKinds, c.Kind) {
		return fmt.Errorf("the kind of %s is %q, not one of %s", c.Name, c.Kind, strings.Join(ConnectionKinds, ", "))
	}
	if why := oneLine(c.Title, MaxConnectionTitle); why != "" {
		return fmt.Errorf("the title of %s: %s", c.Name, why)
	}
	switch c.State {
	case "":
	case ConnectionAvailable:
		// An available connection is one the person has not connected: the
		// connection service serves no address for it yet, and a session
		// reaches nothing of it.
		if c.Audience != "" || c.Address != "" || c.GitAddress != "" || c.GitURL != "" || len(c.ReadOnly) > 0 || len(c.Hidden) > 0 || c.Account != "" || c.APIURL != "" || c.APIExample != "" || c.APINotes != "" {
			return fmt.Errorf("%s is available and names audience, address, git_address, git_url, read_only, hidden, account, api_url, api_example or api_notes, which only a connected connection takes", c.Name)
		}
		return nil
	default:
		return fmt.Errorf("the state of %s is %q, not %s or absent for a connected one", c.Name, c.State, ConnectionAvailable)
	}
	if c.Audience == "" || len(c.Audience) > MaxConnectionAudience || strings.ContainsFunc(c.Audience, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("the audience of %s is not 1 to %d bytes with no space or control character", c.Name, MaxConnectionAudience)
	}
	if err := checkAddress(c.Address); err != nil {
		return fmt.Errorf("the address of %s %w", c.Name, err)
	}
	if c.Account != "" {
		if why := oneLine(c.Account, MaxConnectionAccount); why != "" {
			return fmt.Errorf("the account of %s: %s", c.Name, why)
		}
	}
	switch c.Kind {
	case ConnectionHTTP:
		if len(c.ReadOnly) > 0 || len(c.Hidden) > 0 {
			return fmt.Errorf("%s is of kind http and names read_only or hidden, which only an mcp connection takes", c.Name)
		}
		if c.APIURL != "" {
			c.APIURL = strings.TrimSuffix(c.APIURL, "/")
			if err := checkAddress(c.APIURL); err != nil {
				return fmt.Errorf("the api_url of %s %w", c.Name, err)
			}
		}
		if c.APIExample != "" && !examplePath(c.APIExample) {
			return fmt.Errorf("the api_example of %s is %q, not a path that starts with / of at most %d bytes with no space, control character, ?, #, $, \", `, or \\", c.Name, c.APIExample, MaxConnectionAPIExample)
		}
		if c.APINotes != "" {
			if why := oneLine(c.APINotes, MaxConnectionAPINotes); why != "" {
				return fmt.Errorf("the api_notes of %s: %s", c.Name, why)
			}
		}
		if (c.GitAddress == "") != (c.GitURL == "") {
			return fmt.Errorf("%s names one of git_address and git_url; it names both or neither", c.Name)
		}
		if c.GitAddress == "" {
			return nil
		}
		if err := checkAddress(c.GitAddress); err != nil {
			return fmt.Errorf("the git_address of %s %w", c.Name, err)
		}
		c.GitURL = strings.TrimSuffix(c.GitURL, "/")
		if err := checkAddress(c.GitURL); err != nil {
			return fmt.Errorf("the git_url of %s %w", c.Name, err)
		}
	case ConnectionMCP:
		if c.GitAddress != "" || c.GitURL != "" || c.APIURL != "" || c.APIExample != "" || c.APINotes != "" {
			return fmt.Errorf("%s is of kind mcp and names git_address, git_url, api_url, api_example or api_notes, which only an http connection takes", c.Name)
		}
		if err := checkToolGlobs("read_only", c.Name, c.ReadOnly); err != nil {
			return err
		}
		if err := checkToolGlobs("hidden", c.Name, c.Hidden); err != nil {
			return err
		}
	}
	return nil
}

// checkToolGlobs reports why globs cannot be a connection's list member of
// globs over its tool names: more than MaxReadOnlyTools, or one that is
// empty, longer than MaxReadOnlyGlob bytes or not path.Match syntax.
func checkToolGlobs(member, name string, globs []string) error {
	if len(globs) > MaxReadOnlyTools {
		return fmt.Errorf("%s names %d %s globs, more than %d", name, len(globs), member, MaxReadOnlyTools)
	}
	for i, g := range globs {
		if _, err := path.Match(g, ""); err != nil || g == "" || len(g) > MaxReadOnlyGlob {
			return fmt.Errorf("the %s[%d] of %s is %q, not a glob of 1 to %d bytes", member, i, name, g, MaxReadOnlyGlob)
		}
	}
	return nil
}

// examplePath reports whether p can be the path of a connection's example
// call: it starts with "/", is at most MaxConnectionAPIExample bytes of
// UTF-8, and holds no white space, no control character, no query or
// fragment, and nothing a shell reads inside double quotes, since the
// connections block writes it into a curl command.
func examplePath(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) <= MaxConnectionAPIExample && utf8.ValidString(p) &&
		!strings.ContainsFunc(p, func(r rune) bool {
			return unicode.IsSpace(r) || unicode.IsControl(r) || strings.ContainsRune("?#$\"`\\", r)
		})
}

// checkAddress reports why raw is not an absolute https URL with a host
// and no user information, query or fragment.
func checkAddress(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("is not a URL: %w", err)
	case u.Scheme != "https" || u.Host == "" || u.Hostname() == "":
		return fmt.Errorf("%q is not an absolute https URL with a host", raw)
	case u.User != nil, u.RawQuery != "", u.ForceQuery, u.Fragment != "", strings.Contains(raw, "#"):
		return fmt.Errorf("%q names user information, a query or a fragment", raw)
	}
	return nil
}

// HTTPConnections are the connected connections of cs of kind http, in
// order.
func HTTPConnections(cs []Connection) []Connection {
	var out []Connection
	for _, c := range cs {
		if c.Kind == ConnectionHTTP && c.Connected() {
			out = append(out, c)
		}
	}
	return out
}

// SameConnections reports whether a and b are the same list, in order.
func SameConnections(a, b []Connection) bool {
	return slices.EqualFunc(a, b, func(x, y Connection) bool {
		return x.Name == y.Name && x.Kind == y.Kind && x.Title == y.Title && x.State == y.State && x.Audience == y.Audience &&
			x.Address == y.Address && x.GitAddress == y.GitAddress && x.GitURL == y.GitURL && slices.Equal(x.ReadOnly, y.ReadOnly) &&
			slices.Equal(x.Hidden, y.Hidden) && x.Account == y.Account && x.APIURL == y.APIURL && x.APIExample == y.APIExample && x.APINotes == y.APINotes
	})
}

// ConnectionsChanged is the payload of session.connections_changed (spec
// 069): the session's connections from here on, as a send's allow named
// them, empty for none.
type ConnectionsChanged struct {
	Connections []Connection `json:"connections"`
}

// ConnectionRequested is the payload of connection.requested (spec 069):
// a connection the person is asked to connect, which a client shows as a
// service to connect. It is one the connection service answered is not
// connected, Reason the service's reason, or one the model asked for with
// request_connection, Reason the model's words on why the task needs it.
// ToolUseID names the call that met it or asked for it, and is absent
// when the listing of a server's tools met it; Scopes are the ones the
// other service asked for, when it named them.
type ConnectionRequested struct {
	Name      string   `json:"name"`
	Title     string   `json:"title"`
	Kind      string   `json:"kind"`
	Reason    string   `json:"reason,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
	ToolUseID string   `json:"tool_use_id,omitempty"`
}
