// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// CodeRepositoryUnavailable is spec 019's code for a repository the
// runner could not deliver into the machine.
const CodeRepositoryUnavailable = "repository_unavailable"

// ResourceRepository is the type of a session resource that names a
// repository (spec 019).
const ResourceRepository = "repository"

// Trailers every commit of a session carries (spec 019).
const (
	TrailerSession = "Topos-Session"
	TrailerAgent   = "Topos-Agent"
)

// deliverTimeout bounds the delivery of one repository: its clone, its
// branch and its configuration.
const deliverTimeout = 15 * time.Minute

// Repositories are the repository resources of a session, in order.
func Repositories(s session.Session) []session.Resource {
	var out []session.Resource
	for _, r := range s.Resources {
		if r.Type == ResourceRepository {
			out = append(out, r)
		}
	}
	return out
}

// dirPattern is a directory name a repository's URL may give.
var dirPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// SessionBranch is the branch a session works on (spec 019).
func SessionBranch(s session.Session) string {
	return "agents/" + agentName(s) + "/" + s.ID
}

func agentName(s session.Session) string {
	if s.Agent.Name == "" {
		return "agent"
	}
	return s.Agent.Name
}

// deliver clones each of the session's repositories into the machine,
// the first into the working directory and each further one into a
// directory named after it, on the session's branch, with the session's
// author and the commit-msg hook that adds its trailers. A repository
// already there, as a restarted delivery finds it, is configured and not
// cloned again. A failure is an OpenError repository_unavailable: the
// machine stays, and the session learns which repository is missing.
func deliver(ctx context.Context, s session.Session, m machine.Machine) error {
	repos := Repositories(s)
	if len(repos) > session.MaxRepositories {
		return &machine.OpenError{Code: CodeRepositoryUnavailable, Err: fmt.Errorf("the session names %d repositories, at most %d", len(repos), session.MaxRepositories)}
	}
	workdir := m.Info().Workdir
	taken := map[string]bool{}
	var errs []error
	for i, repo := range repos {
		dir := workdir
		if i > 0 {
			dir = path.Join(workdir, repoDir(repo.URL, i, taken))
		}
		script, err := deliveryScript(s, repo, dir)
		if err == nil {
			err = runScript(ctx, m, script)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("deliver %s: %w", repo.URL, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return &machine.OpenError{Code: CodeRepositoryUnavailable, Err: err}
	}
	return nil
}

// repoDir is the directory of a further repository: the last segment of
// its URL without .git, or repository-<i> when that is no name.
func repoDir(raw string, i int, taken map[string]bool) string {
	name := "repository-" + strconv.Itoa(i)
	if u, err := url.Parse(raw); err == nil {
		if base := strings.TrimSuffix(path.Base(u.Path), ".git"); dirPattern.MatchString(base) {
			name = base
		}
	}
	for taken[name] {
		name += "-" + strconv.Itoa(i)
	}
	taken[name] = true
	return name
}

// deliveryScript is the shell script that delivers one repository into
// dir, every value quoted. git reaches the git host with the machine's
// own configuration: in a Cella sandbox that sends the placeholder of the
// git host's Secret, set when the sandbox opens (spec 018), so neither
// the script nor the log carries a credential.
func deliveryScript(s session.Session, repo session.Resource, dir string) (string, error) {
	if err := session.CheckRepository(repo, "https", "http", "file"); err != nil {
		return "", err
	}
	agent := s.Agent.ID + "@" + strconv.Itoa(s.Agent.Version)
	hook := "#!/bin/sh\n" +
		"# Names the Topos session and agent a commit came from (spec 019).\n" +
		"exec git interpret-trailers --in-place --if-exists doNothing" +
		" --trailer " + quote(TrailerSession+": "+s.ID) + " --trailer " + quote(TrailerAgent+": "+agent) + ` "$1"` + "\n"
	var b strings.Builder
	w := func(line string) { b.WriteString(line + "\n") }
	w("set -e")
	w("dir=" + quote(dir))
	w(`mkdir -p "$dir"`)
	w(`if [ ! -d "$dir/.git" ]; then`)
	w("  git clone --quiet -- " + quote(repo.URL) + ` "$dir"`)
	w("fi")
	w(`cd "$dir"`)
	w("git config user.name " + quote(agentName(s)))
	w("git config user.email " + quote(agentName(s)+"@agents.topos.invalid"))
	w("git config push.autoSetupRemote true")
	w("branch=" + quote(SessionBranch(s)))
	w(`if ! git rev-parse --verify --quiet "refs/heads/$branch" >/dev/null; then`)
	if repo.Ref != "" {
		w("  ref=" + quote(repo.Ref))
		w(`  if c=$(git rev-parse --verify --quiet "refs/remotes/origin/$ref^{commit}") || c=$(git rev-parse --verify --quiet "$ref^{commit}"); then :; else`)
		w(`    git fetch --quiet origin "$ref"`)
		w(`    c=$(git rev-parse FETCH_HEAD)`)
		w("  fi")
		w(`  git checkout --quiet -b "$branch" "$c"`)
	} else {
		w(`  git checkout --quiet -b "$branch"`)
	}
	w("fi")
	w(`hooks=$(git rev-parse --git-path hooks)`)
	w(`mkdir -p "$hooks"`)
	w(`printf '%s' ` + quote(hook) + ` >"$hooks/commit-msg"`)
	w(`chmod 755 "$hooks/commit-msg"`)
	return b.String(), nil
}

// runScript runs a delivery script, reporting its output on failure.
func runScript(ctx context.Context, m machine.Machine, script string) error {
	res, err := m.Exec(ctx, machine.ExecRequest{Command: script, Timeout: deliverTimeout})
	switch {
	case err != nil:
		return err
	case res.TimedOut:
		return fmt.Errorf("timed out after %s", deliverTimeout)
	case res.ExitCode != 0:
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Output)))
	}
	return nil
}

// quote is s as one word of the shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
