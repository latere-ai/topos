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
	"slices"
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

// MaxRepositories is how many repositories a session names at most.
const MaxRepositories = 8

// Trailers every commit of a session carries (spec 019).
const (
	TrailerSession = "Topos-Session"
	TrailerAgent   = "Topos-Agent"
)

// deliverTimeout bounds the delivery of one repository: its clone, its
// branch and its configuration.
const deliverTimeout = 15 * time.Minute

// GitCredentials names how a session's machine authenticates to a git
// host without holding the credential (specs 018 and 019): the variable
// of the machine's environment whose value is the placeholder of a Cella
// Secret scoped to host, which Cella's egress gateway replaces with the
// session's token on requests to host. ok is false when the session has
// no credential for host, and git reaches it without one.
type GitCredentials interface {
	GitCredential(ctx context.Context, s session.Session, host string) (env string, ok bool, err error)
}

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

// refPattern is a ref a session may name: a branch, a tag or a commit,
// with none of git's option or range syntax.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// dirPattern is a directory name a repository's URL may give.
var dirPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// envPattern is a variable name a shell expands.
var envPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckRepository reports why a repository resource cannot be delivered:
// an absolute URL naming its host, with no credential in it, and a ref
// git reads as a name rather than an option. schemes are the URL schemes
// allowed; the API allows https alone.
func CheckRepository(r session.Resource, schemes ...string) error {
	u, err := url.Parse(r.URL)
	switch {
	case err != nil:
		return fmt.Errorf("the url %q does not parse", r.URL)
	case !slices.Contains(schemes, u.Scheme):
		return fmt.Errorf("the url %q is not %s", r.URL, strings.Join(schemes, " or "))
	case u.Scheme != "file" && u.Host == "":
		return fmt.Errorf("the url %q names no host", r.URL)
	case u.User != nil:
		return fmt.Errorf("the url %q holds a credential; the session's credential reaches the git host outside the machine", r.URL)
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("the url %q has a query or a fragment", r.URL)
	}
	if r.Ref != "" && (!refPattern.MatchString(r.Ref) || strings.HasPrefix(r.Ref, "-") || strings.Contains(r.Ref, "..")) {
		return fmt.Errorf("the ref %q is not a branch, a tag or a commit", r.Ref)
	}
	return nil
}

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
func (r *Runner) deliver(ctx context.Context, s session.Session, m machine.Machine) error {
	repos := Repositories(s)
	if len(repos) > MaxRepositories {
		return &machine.OpenError{Code: CodeRepositoryUnavailable, Err: fmt.Errorf("the session names %d repositories, at most %d", len(repos), MaxRepositories)}
	}
	workdir := m.Info().Workdir
	taken := map[string]bool{}
	var errs []error
	for i, repo := range repos {
		dir := workdir
		if i > 0 {
			dir = path.Join(workdir, repoDir(repo.URL, i, taken))
		}
		script, err := r.deliveryScript(ctx, s, repo, dir)
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
// dir. Every value is quoted; the credential's placeholder is expanded
// from the machine's own environment, so the script and the log never
// carry it.
func (r *Runner) deliveryScript(ctx context.Context, s session.Session, repo session.Resource, dir string) (string, error) {
	if err := CheckRepository(repo, "https", "http", "file"); err != nil {
		return "", err
	}
	// key and value are the header git sends on every request to the
	// host; the value's variable expands to the placeholder Cella's
	// egress gateway swaps for the credential.
	var key, value string
	if u, _ := url.Parse(repo.URL); r.o.GitCredentials != nil && (u.Scheme == "https" || u.Scheme == "http") {
		env, ok, err := r.o.GitCredentials.GitCredential(ctx, s, strings.ToLower(u.Hostname()))
		switch {
		case err != nil:
			return "", fmt.Errorf("the git credential for %s: %w", u.Hostname(), err)
		case ok && !envPattern.MatchString(env):
			return "", fmt.Errorf("the git credential for %s names the variable %q", u.Hostname(), env)
		case ok:
			key = quote("http." + u.Scheme + "://" + u.Host + "/.extraHeader")
			value = `"Authorization: Bearer $` + env + `"`
		}
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
	if key != "" {
		w("  git -c " + key + "=" + value + " clone --quiet -- " + quote(repo.URL) + ` "$dir"`)
	} else {
		w("  git clone --quiet -- " + quote(repo.URL) + ` "$dir"`)
	}
	w("fi")
	w(`cd "$dir"`)
	if key != "" {
		w("git config --replace-all " + key + " " + value)
	}
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
