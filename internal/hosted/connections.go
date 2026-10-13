// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/internal/mcp"
	"latere.ai/x/topos/internal/version"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// SecretConnections ends the name of a sandbox's Secret that holds the
// session's token for the installation's connection service (spec 069).
const SecretConnections = "-connections"

// ConnectionsGitConfig is the file of git configuration, under the
// sandbox's HOME, that points git at the session's http connections: the
// runner writes it whole and names it once in git's global configuration
// as an include.path, so a connection a send removed leaves no entry.
const ConnectionsGitConfig = ".config/topos/connections.gitconfig"

// connectionsScript is the shell script that writes the git configuration
// of cs: for each connection with a git address, git's URLs under its git
// URL rewritten to the git address, and the placeholder of the connection
// service's Secret sent there as a bearer. The placeholder is read from
// the sandbox's own environment, so neither the script nor the log
// carries it. A sandbox without git has nothing to configure.
func connectionsScript(cs []machine.HTTPConnection) string {
	var b strings.Builder
	b.WriteString("command -v git >/dev/null 2>&1 || exit 0\nset -e\n")
	b.WriteString(`f="$HOME/` + ConnectionsGitConfig + `"` + "\n")
	b.WriteString(`mkdir -p "$(dirname "$f")"` + "\n")
	b.WriteString(`t="$f.$$"` + "\n")
	b.WriteString(`trap 'rm -f "$t"' EXIT` + "\n")
	b.WriteString(`: >"$t"` + "\n")
	for _, c := range cs {
		if c.GitAddress == "" {
			continue
		}
		base := strings.TrimSuffix(c.GitAddress, "/") + "/"
		fmt.Fprintf(&b, "git config -f \"$t\" %s %s\n", quote("url."+base+".insteadOf"), quote(strings.TrimSuffix(c.GitURL, "/")+"/"))
		fmt.Fprintf(&b, "git config -f \"$t\" %s \"Authorization: Bearer $%s\"\n", quote("http."+base+".extraHeader"), session.ConnectionsToken)
	}
	b.WriteString(`mv -f "$t" "$f"` + "\n")
	b.WriteString(`git config --global --get-all include.path | grep -qxF "$f" || git config --global --add include.path "$f"` + "\n")
	return b.String()
}

// quote is s as one word of the sandbox's shell. The host sandbox's quoting
// is not imported here, since its package builds on Unix alone and this
// file builds wherever the runner does.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ApplyConnections gives the sandbox the session's http connections (spec
// 069): it writes the git configuration of their git addresses, and
// every command run after carries each connection's forwarding address
// under its variable. A sandbox that mounts no Secret of the connection
// service reaches none of them, and takes nothing. The same list as the
// last one writes nothing.
func (m *sessionMachine) ApplyConnections(ctx context.Context, cs []machine.HTTPConnection) error {
	if !m.reaches {
		return nil
	}
	script := connectionsScript(cs)
	env := map[string]string{}
	for _, c := range cs {
		env[c.Env] = c.Address
	}
	m.mu.Lock()
	same := script == m.script && maps.Equal(env, m.env)
	m.mu.Unlock()
	if same {
		return nil
	}
	res, err := m.Machine.Exec(ctx, machine.ExecRequest{Command: script, Timeout: time.Minute})
	switch {
	case err != nil:
		return fmt.Errorf("configure git for the session's connections: %w", err)
	case res.ExitCode != 0:
		return fmt.Errorf("configure git for the session's connections: exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Output)))
	}
	m.mu.Lock()
	m.script, m.env = script, env
	m.mu.Unlock()
	return nil
}

// withEnv is r with the variables of the session's http connections, the
// request's own winning over one of the same name.
func (m *sessionMachine) withEnv(r machine.ExecRequest) machine.ExecRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.env) == 0 {
		return r
	}
	env := maps.Clone(m.env)
	maps.Copy(env, r.Env)
	r.Env = env
	return r
}

// Exec runs a command with the variables of the session's http
// connections in its environment.
func (m *sessionMachine) Exec(ctx context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	return m.Machine.Exec(ctx, m.withEnv(r))
}

// ExecStream runs a command as Exec does, streaming its output.
func (m *sessionMachine) ExecStream(ctx context.Context, r machine.ExecRequest) (machine.ExecStream, error) {
	return m.Machine.ExecStream(ctx, m.withEnv(r))
}

var _ machine.Connected = (*sessionMachine)(nil)

// MCPListTimeout bounds a drive's listing of one connection's tools.
const MCPListTimeout = 30 * time.Second

// mcp lists the tools of each of s's connected connections of kind mcp,
// at once, with the session's token for the connection's audience and
// the workload session, asked of the drive's token source for every
// request, and adds them to reg (spec 069). It answers how each listing
// ended, which the harness records and names in the connections block; a
// listing that fails offers none of the connection's tools, and the
// session goes on. An available connection has no server to list.
func (b builder) mcp(ctx context.Context, s session.Session, reg *tools.Registry) map[string]harness.MCPListing {
	var conns []session.Connection
	for _, c := range s.Connections {
		if c.Kind == session.ConnectionMCP && c.Connected() {
			conns = append(conns, c)
		}
	}
	if len(conns) == 0 {
		return nil
	}
	src := runner.TokensFrom(ctx)
	type listing struct {
		tools []tools.Tool
		l     harness.MCPListing
	}
	results := make([]listing, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Go(func() {
			token := func(ctx context.Context) (string, error) {
				if src == nil {
					return "", errors.New("this server mints no session tokens")
				}
				cred, err := src.Token(ctx, c.Audience, runner.WorkloadSession)
				return cred.Value, err
			}
			server := mcp.Server{Connection: c, Client: mcp.NewClient(c.Address, token, otel.HTTPClient(), version.Version)}
			lctx, cancel := context.WithTimeout(ctx, MCPListTimeout)
			defer cancel()
			ts, reduced, err := server.Tools(lctx)
			var nc *mcp.NotConnected
			switch {
			case errors.As(err, &nc):
				results[i].l = harness.MCPListing{NotConnected: true, Reason: nc.Reason, Scopes: nc.Scopes}
			case err != nil:
				results[i].l = harness.MCPListing{Err: err.Error()}
			default:
				results[i] = listing{tools: ts, l: harness.MCPListing{Reduced: reduced}}
			}
		})
	}
	wg.Wait()
	out := make(map[string]harness.MCPListing, len(conns))
	for i, c := range conns {
		for _, t := range results[i].tools {
			// A server that lists two tools of one name offers the first.
			if err := reg.Add(t); err != nil {
				b.log().WarnContext(ctx, "a server's tool is not offered", "session", s.ID, "connection", c.Name, "err", err)
			}
		}
		out[c.Name] = results[i].l
	}
	return out
}

// log is the harness's logger, one that discards when none is set.
func (b builder) log() *slog.Logger {
	if b.o.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.o.Log
}
