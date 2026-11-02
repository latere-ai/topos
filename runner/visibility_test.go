// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// probed is a machine whose every command answers res and err, and which
// records the commands.
type probed struct {
	machine.Machine
	res  machine.ExecResult
	err  error
	seen *[]machine.ExecRequest
}

func (p *probed) Exec(_ context.Context, r machine.ExecRequest) (machine.ExecResult, error) {
	*p.seen = append(*p.seen, r)
	return p.res, p.err
}

// TestARepositorysVisibilityIsWhatAReadWithNoCredentialFinds (spec 101):
// a repository whose git host answers a read with no credential is
// public, one that asks for a credential private, and one that answers
// anything else, or is not reached over http or https, is not known; the
// read sends no credential git could send.
func TestARepositorysVisibilityIsWhatAReadWithNoCredentialFinds(t *testing.T) {
	ctx := t.Context()
	for _, c := range []struct {
		name string
		url  string
		res  machine.ExecResult
		err  error
		want string
	}{
		{"answered", "https://git.example.com/acme/api.git", machine.ExecResult{}, nil, session.VisibilityPublic},
		{"asked for one", "https://git.example.com/acme/api.git", machine.ExecResult{ExitCode: 128, Output: []byte("fatal: could not read Username for 'https://git.example.com': terminal prompts disabled")}, nil, session.VisibilityPrivate},
		{"refused otherwise", "http://git.example.com/acme/api.git", machine.ExecResult{ExitCode: 128, Output: []byte("fatal: repository not found")}, nil, ""},
		{"timed out", "https://git.example.com/acme/api.git", machine.ExecResult{TimedOut: true}, nil, ""},
		{"lost", "https://git.example.com/acme/api.git", machine.ExecResult{}, errors.New("gone"), ""},
		{"a local repository", "file:///srv/api.git", machine.ExecResult{}, nil, ""},
	} {
		var seen []machine.ExecRequest
		m := &probed{res: c.res, err: c.err, seen: &seen}
		if got := visibility(ctx, m, c.url); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if strings.HasPrefix(c.url, "file:") {
			if len(seen) != 0 {
				t.Errorf("%s: ran %v", c.name, seen)
			}
			continue
		}
		if len(seen) != 1 || !strings.Contains(seen[0].Command, "extraHeader=") || !strings.Contains(seen[0].Command, "credential.helper=") ||
			seen[0].Env["GIT_TERMINAL_PROMPT"] != "0" || seen[0].Env["LC_ALL"] != "C" {
			t.Errorf("%s: ran %+v", c.name, seen)
		}
	}
}
