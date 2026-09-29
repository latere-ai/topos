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
// author and the commit-msg hook that adds its trailers, and returns the
// ones it delivered with the commit each branch starts at. A repository
// already there, as a restarted delivery finds it, is configured and not
// cloned again. A failure is an OpenError repository_unavailable beside
// the repositories that were delivered: the machine stays, and the
// session learns which repository is missing.
func deliver(ctx context.Context, s session.Session, m machine.Machine) ([]session.DeliveredRepository, error) {
	repos := Repositories(s)
	if len(repos) > session.MaxRepositories {
		return nil, &machine.OpenError{Code: CodeRepositoryUnavailable, Err: fmt.Errorf("the session names %d repositories, at most %d", len(repos), session.MaxRepositories)}
	}
	workdir := m.Info().Workdir
	branch := SessionBranch(s)
	taken := map[string]bool{}
	var delivered []session.DeliveredRepository
	var errs []error
	for i, repo := range repos {
		dir := workdir
		if i > 0 {
			dir = path.Join(workdir, repoDir(repo.URL, i, taken))
		}
		commit, err := deliverOne(ctx, s, m, repo, dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("deliver %s: %w", repo.URL, err))
			continue
		}
		delivered = append(delivered, session.DeliveredRepository{URL: repo.URL, Branch: branch, Commit: commit})
	}
	if err := errors.Join(errs...); err != nil {
		return delivered, &machine.OpenError{Code: CodeRepositoryUnavailable, Err: err}
	}
	return delivered, nil
}

// deliverOne delivers one repository into dir and returns the commit its
// checked-out branch is at.
func deliverOne(ctx context.Context, s session.Session, m machine.Machine, repo session.Resource, dir string) (string, error) {
	script, err := deliveryScript(s, repo, dir)
	if err != nil {
		return "", err
	}
	if err := runScript(ctx, m, script); err != nil {
		return "", err
	}
	return head(ctx, m, dir)
}

// head is the commit HEAD of the repository in dir is at, empty on a
// branch with no commit yet, as an empty repository's. git rev-parse
// --verify --quiet exits 1 silently for such a HEAD and fails otherwise
// with a message, so only a silent exit 1 is read as no commit.
func head(ctx context.Context, m machine.Machine, dir string) (string, error) {
	res, err := m.Exec(ctx, machine.ExecRequest{Command: "git rev-parse --verify --quiet HEAD", Dir: dir, Timeout: deliverTimeout})
	out := strings.TrimSpace(string(res.Output))
	switch {
	case err != nil:
		return "", err
	case res.TimedOut:
		return "", fmt.Errorf("read HEAD: timed out after %s", deliverTimeout)
	case res.ExitCode == 1 && out == "":
		return "", nil
	case res.ExitCode != 0:
		return "", fmt.Errorf("read HEAD: exit %d: %s", res.ExitCode, out)
	}
	return out, nil
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
	// The repository is fetched into the directory as it is, not cloned: a
	// clone refuses a directory that is not empty, and a sandbox's
	// workspace volume starts with lost+found, which git is told to skip.
	w(`if [ ! -d "$dir/.git" ]; then`)
	w(`  git init --quiet "$dir"`)
	w(`  cd "$dir"`)
	w(`  if [ -d lost+found ]; then mkdir -p .git/info && printf '/lost+found/\n' >>.git/info/exclude; fi`)
	// A fetch that fails leaves the directory as it found it, as a failed
	// clone does, so the machine holds no repository without its commits.
	w("  if ! { git remote add origin " + quote(repo.URL) + " && git fetch --quiet origin; }; then rm -rf .git; exit 1; fi")
	w(`  default=$(git ls-remote --symref origin HEAD | sed -n 's|^ref: refs/heads/\(.*\)[[:space:]]HEAD$|\1|p')`)
	w(`  if [ -n "$default" ] && git rev-parse --verify --quiet "refs/remotes/origin/$default" >/dev/null; then`)
	w(`    git checkout --quiet -B "$default" --track "origin/$default"`)
	w(`  elif [ -n "$default" ]; then`)
	w(`    git symbolic-ref HEAD "refs/heads/$default"`)
	w("  fi")
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
