// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ResourceRepository is the type of a session resource that names a
// repository (spec 019).
const ResourceRepository = "repository"

// Repositories are the repository resources of a session, in order.
func Repositories(s Session) []Resource {
	var out []Resource
	for _, r := range s.Resources {
		if r.Type == ResourceRepository {
			out = append(out, r)
		}
	}
	return out
}

// Branch is the branch a session works on in each of its repositories
// (spec 019).
func Branch(s Session) string {
	return "agents/" + s.AgentName() + "/" + s.ID
}

// AgentName is the name of the session's agent, the author of its
// commits, or "agent" for a session whose agent has none.
func (s Session) AgentName() string {
	if s.Agent.Name == "" {
		return "agent"
	}
	return s.Agent.Name
}

// dirPattern is a directory name a repository's URL may give.
var dirPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RepositoryDirs are the directories repos are delivered into, relative
// to the machine's working directory and in their order: "" for the
// first repository that names no app, which is the working directory
// itself; for a repository that names an app, its slug (spec 058); and
// for each further one the last segment of its URL without .git, or
// repository-<i> when that is no name; each made unique with -<i>. An
// app's checkout is never the working directory, so the working
// directory is the same whatever number of apps a session is given.
func RepositoryDirs(repos []Resource) []string {
	out := make([]string, len(repos))
	taken := map[string]bool{}
	workdir := false
	for i, r := range repos {
		switch {
		case r.App != nil:
			out[i] = repositoryDir(r.App.Slug, i, taken)
		case !workdir:
			workdir = true
		default:
			out[i] = repositoryDir(urlName(r.URL), i, taken)
		}
	}
	return out
}

// urlName is the last segment of a repository's URL without .git, ""
// when that is no directory name.
func urlName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(path.Base(u.Path), ".git")
}

// repositoryDir is name as the directory of the i-th repository, or
// repository-<i> when it is no directory name, made unique with -<i>.
func repositoryDir(name string, i int, taken map[string]bool) string {
	if !dirPattern.MatchString(name) {
		name = "repository-" + strconv.Itoa(i)
	}
	for taken[name] {
		name += "-" + strconv.Itoa(i)
	}
	taken[name] = true
	return name
}

// MaxRepositories is how many repositories a session names at most, and
// so how many an agent names for the sessions that take its own (spec
// 019).
const MaxRepositories = 8

// refPattern is a ref a repository may name: a branch, a tag or a
// commit, with none of git's option or range syntax.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,255}$`)

// CheckRepository reports why a repository resource cannot be delivered:
// an absolute URL naming its host, with no credential in it, and a ref
// git reads as a name rather than an option. schemes are the URL schemes
// allowed; the API and the manifest allow https alone. The error never
// quotes a URL that holds a credential, so a refusal does not carry it.
func CheckRepository(r Resource, schemes ...string) error {
	u, err := url.Parse(r.URL)
	switch {
	case err != nil:
		return fmt.Errorf("the url %q does not parse", r.URL)
	case u.User != nil:
		return errors.New("the url holds a credential; the session's credential reaches the git host outside the machine")
	case !slices.Contains(schemes, u.Scheme):
		return fmt.Errorf("the url %q is not %s", r.URL, strings.Join(schemes, " or "))
	case u.Scheme != "file" && u.Host == "":
		return fmt.Errorf("the url %q names no host", r.URL)
	case u.RawQuery != "" || u.Fragment != "":
		return fmt.Errorf("the url %q has a query or a fragment", r.URL)
	}
	if r.Ref != "" && (!refPattern.MatchString(r.Ref) || strings.HasPrefix(r.Ref, "-") || strings.Contains(r.Ref, "..")) {
		return fmt.Errorf("the ref %q is not a branch, a tag or a commit", r.Ref)
	}
	return nil
}
