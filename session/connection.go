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
	// MaxReadOnlyTools is the most read_only globs of one connection.
	MaxReadOnlyTools = 64
	// MaxReadOnlyGlob is the most bytes of one read_only glob.
	MaxReadOnlyGlob = 128
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
	// Audience is the core the session's token for the connection is
	// addressed to.
	Audience string `json:"audience"`
	// Address is the forwarding address of an http connection and the MCP
	// address of an mcp one.
	Address string `json:"address"`
	// GitAddress is an http connection's git address, and GitURL the git
	// host the person's repositories live at, which git in the sandbox
	// reaches through GitAddress; both or neither.
	GitAddress string `json:"git_address,omitempty"`
	GitURL     string `json:"git_url,omitempty"`
	// ReadOnly are an mcp connection's globs over its tool names; a tool a
	// glob matches reads, and every other tool reaches outside.
	ReadOnly []string `json:"read_only,omitempty"`
}

// connectionName is a lowercase DNS label that starts with a letter, so
// it names an environment variable too.
var connectionName = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

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

// CheckConnections reports why cs cannot be a session's connections: at
// most MaxConnections, each with a name, a kind, a title, an audience and
// an address of the rules spec 069 gives, its git address and git URL on
// an http connection alone and both or neither, its read_only globs on an
// mcp connection alone, and no name or git URL named twice. A git URL is
// written without a trailing "/" in the list it answers, which is a copy.
func CheckConnections(cs []Connection) ([]Connection, error) {
	if len(cs) > MaxConnections {
		return nil, fmt.Errorf("connections names %d connections, more than %d", len(cs), MaxConnections)
	}
	out := slices.Clone(cs)
	names, gits := map[string]bool{}, map[string]bool{}
	for i := range out {
		c := &out[i]
		c.ReadOnly = slices.Clone(c.ReadOnly)
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
	if c.Audience == "" || len(c.Audience) > MaxConnectionAudience || strings.ContainsFunc(c.Audience, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("the audience of %s is not 1 to %d bytes with no space or control character", c.Name, MaxConnectionAudience)
	}
	if err := checkAddress(c.Address); err != nil {
		return fmt.Errorf("the address of %s %w", c.Name, err)
	}
	switch c.Kind {
	case ConnectionHTTP:
		if len(c.ReadOnly) > 0 {
			return fmt.Errorf("%s is of kind http and names read_only, which only an mcp connection takes", c.Name)
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
		if c.GitAddress != "" || c.GitURL != "" {
			return fmt.Errorf("%s is of kind mcp and names git_address or git_url, which only an http connection takes", c.Name)
		}
		if len(c.ReadOnly) > MaxReadOnlyTools {
			return fmt.Errorf("%s names %d read_only globs, more than %d", c.Name, len(c.ReadOnly), MaxReadOnlyTools)
		}
		for i, g := range c.ReadOnly {
			if _, err := path.Match(g, ""); err != nil || g == "" || len(g) > MaxReadOnlyGlob {
				return fmt.Errorf("the read_only[%d] of %s is %q, not a glob of 1 to %d bytes", i, c.Name, g, MaxReadOnlyGlob)
			}
		}
	}
	return nil
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

// HTTPConnections are the connections of cs of kind http, in order.
func HTTPConnections(cs []Connection) []Connection {
	var out []Connection
	for _, c := range cs {
		if c.Kind == ConnectionHTTP {
			out = append(out, c)
		}
	}
	return out
}

// SameConnections reports whether a and b are the same list, in order.
func SameConnections(a, b []Connection) bool {
	return slices.EqualFunc(a, b, func(x, y Connection) bool {
		return x.Name == y.Name && x.Kind == y.Kind && x.Title == y.Title && x.Audience == y.Audience &&
			x.Address == y.Address && x.GitAddress == y.GitAddress && x.GitURL == y.GitURL && slices.Equal(x.ReadOnly, y.ReadOnly)
	})
}

// ConnectionsChanged is the payload of session.connections_changed (spec
// 069): the session's connections from here on, as a send's allow named
// them, empty for none.
type ConnectionsChanged struct {
	Connections []Connection `json:"connections"`
}
