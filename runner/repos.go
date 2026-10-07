// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
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

// Trailers every commit of a session carries (spec 019).
const (
	TrailerSession = "Topos-Session"
	TrailerAgent   = "Topos-Agent"
)

// deliverTimeout bounds the delivery of one repository: its clone, its
// branch and its configuration.
const deliverTimeout = 15 * time.Minute

// served reads the commits an app is served at with the drive's tokens,
// nil when the runner has no app host to read.
func (r *Runner) served(tokens *TokenSource) func(ctx context.Context, slug string) (AppCommits, error) {
	if r.o.Apps == nil {
		return nil
	}
	return func(ctx context.Context, slug string) (AppCommits, error) { return r.o.Apps(ctx, tokens, slug) }
}

// commitForm is a commit's id as git names it in full or abbreviated.
var commitForm = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// base is the commit an app's checkout starts at and the base it is, read
// from the app host by served (spec 058): the live commit, else the newest
// ready preview's, else the default branch, "" for the commit; why says
// why the host's answer could not be used, "" when it could or when there
// is no app host.
func base(ctx context.Context, served func(context.Context, string) (AppCommits, error), slug string) (commit, kind, why string) {
	if served == nil {
		return "", session.BaseDefault, ""
	}
	c, err := served(ctx, slug)
	switch {
	case err != nil:
		return "", session.BaseDefault, "the app host could not be read: " + err.Error()
	case c.Live != "" && commitForm.MatchString(c.Live):
		return c.Live, session.BaseLive, ""
	case c.Live != "":
		return "", session.BaseDefault, fmt.Sprintf("the app host named the live commit %q, which is no commit's id", c.Live)
	case c.Preview != "" && commitForm.MatchString(c.Preview):
		return c.Preview, session.BasePreview, ""
	case c.Preview != "":
		return "", session.BaseDefault, fmt.Sprintf("the app host named the preview's commit %q, which is no commit's id", c.Preview)
	}
	return "", session.BaseDefault, ""
}

// deliver clones each of the session's repositories into the machine,
// the first that names no app into the working directory and each
// further one into a directory named after it or after its app, on the
// session's branch, with the session's author and the commit-msg hook
// that adds its trailers, and returns the ones it delivered with the
// commit each branch starts at. An app's branch starts at the commit the
// app is served at, which served reads (spec 058). A repository already
// there, as a restarted delivery finds it, is configured and not cloned
// again. A failure is an OpenError repository_unavailable beside the
// repositories that were delivered: the machine stays, and the session
// learns which repository is missing.
func deliver(ctx context.Context, s session.Session, m machine.Machine, served func(context.Context, string) (AppCommits, error)) ([]session.DeliveredRepository, error) {
	repos := session.Repositories(s)
	if len(repos) > session.MaxRepositories {
		return nil, &machine.OpenError{Code: CodeRepositoryUnavailable, Err: fmt.Errorf("the session names %d repositories, at most %d", len(repos), session.MaxRepositories)}
	}
	workdir := m.Info().Workdir
	branch := session.Branch(s)
	dirs := session.RepositoryDirs(repos)
	var delivered []session.DeliveredRepository
	var errs []error
	for i, repo := range repos {
		dir := path.Join(workdir, dirs[i])
		got := session.DeliveredRepository{URL: repo.URL, Branch: branch}
		start := ""
		if repo.App != nil {
			got.App = repo.App.Slug
			if repo.Ref == "" {
				start, got.Base, got.BaseError = base(ctx, served, repo.App.Slug)
			}
		}
		commit, err := deliverOne(ctx, s, m, repo, dir, start)
		if err != nil {
			errs = append(errs, fmt.Errorf("deliver %s: %w", repo.URL, err))
			continue
		}
		// A commit the repository does not hold starts the branch at the
		// default branch, which the record says.
		if start != "" && !strings.HasPrefix(commit, start) {
			got.Base, got.BaseError = session.BaseDefault, "the repository does not hold the commit "+start
		}
		got.Commit = commit
		delivered = append(delivered, got)
	}
	if err := errors.Join(errs...); err != nil {
		return delivered, &machine.OpenError{Code: CodeRepositoryUnavailable, Err: err}
	}
	return delivered, nil
}

// deliverOne delivers one repository into dir, its branch starting at the
// commit start when it names one, and returns the commit its checked-out
// branch is at.
func deliverOne(ctx context.Context, s session.Session, m machine.Machine, repo session.Resource, dir, start string) (string, error) {
	script, err := deliveryScript(s, repo, dir, start)
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

// deliveryScript is the shell script that delivers one repository into
// dir, every value quoted. git reaches the git host with the machine's
// own configuration: in a Cella sandbox that sends the placeholder of the
// git host's Secret, set when the sandbox opens (spec 018), so neither
// the script nor the log carries a credential. An app's repository is
// fetched with its tags, so a released commit is present wherever its
// branch went, and its branch starts at start when the repository holds
// that commit, at the default branch otherwise (spec 058).
func deliveryScript(s session.Session, repo session.Resource, dir, start string) (string, error) {
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
	fetch := "git fetch --quiet origin"
	if repo.App != nil {
		fetch = "git fetch --quiet --tags origin"
	}
	w("  if ! { git remote add origin " + quote(repo.URL) + " && " + fetch + "; }; then rm -rf .git; exit 1; fi")
	w(`  default=$(git ls-remote --symref origin HEAD | sed -n 's|^ref: refs/heads/\(.*\)[[:space:]]HEAD$|\1|p')`)
	w(`  if [ -n "$default" ] && git rev-parse --verify --quiet "refs/remotes/origin/$default" >/dev/null; then`)
	w(`    git checkout --quiet -B "$default" --track "origin/$default"`)
	w(`  elif [ -n "$default" ]; then`)
	w(`    git symbolic-ref HEAD "refs/heads/$default"`)
	w("  fi")
	w("fi")
	w(`cd "$dir"`)
	w("git config user.name " + quote(s.AgentName()))
	w("git config user.email " + quote(s.AgentName()+"@agents.topos.invalid"))
	w("git config push.autoSetupRemote true")
	w("branch=" + quote(session.Branch(s)))
	w(`if ! git rev-parse --verify --quiet "refs/heads/$branch" >/dev/null; then`)
	if repo.Ref != "" {
		w("  ref=" + quote(repo.Ref))
		w(`  if c=$(git rev-parse --verify --quiet "refs/remotes/origin/$ref^{commit}") || c=$(git rev-parse --verify --quiet "$ref^{commit}"); then :; else`)
		w(`    git fetch --quiet origin "$ref"`)
		w(`    c=$(git rev-parse FETCH_HEAD)`)
		w("  fi")
		w(`  git checkout --quiet -b "$branch" "$c"`)
	} else if start != "" {
		w("  start=" + quote(start))
		w(`  if c=$(git rev-parse --verify --quiet "$start^{commit}") || { git fetch --quiet origin "$start" 2>/dev/null && c=$(git rev-parse --verify --quiet "$start^{commit}"); }; then`)
		w(`    git checkout --quiet -b "$branch" "$c"`)
		w("  else")
		w(`    git checkout --quiet -b "$branch"`)
		w("  fi")
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
