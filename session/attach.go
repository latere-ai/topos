// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// ResourceApp is the app at the installation's app host a repository is
// the source of (spec 058): its slug, the name a person reads, and its
// public address.
type ResourceApp struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// ContextPart is one titled text the allow of a session's create
// attached, which the model reads for the session's whole life (spec
// 058).
type ContextPart struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

// The bounds of what an allow attaches (spec 058).
const (
	// MaxAppName is the most characters of an attached app's name.
	MaxAppName = 200
	// MaxContextTitle is the most characters of a context part's title.
	MaxContextTitle = 100
	// MaxContext is the most bytes of a session's context, every part's
	// title and text together.
	MaxContext = 32 << 10
)

// appSlug is the form of an app's slug at the app host.
var appSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidAppSlug reports whether slug has the form of an app's slug.
func ValidAppSlug(slug string) bool { return appSlug.MatchString(slug) }

// oneLine reports why s cannot be a line a person reads, "" when it can:
// empty, longer than limit characters, or holding a control character or
// a line or paragraph separator.
func oneLine(s string, limit int) string {
	switch n := utf8.RuneCountInString(s); {
	case n == 0:
		return "it is empty"
	case n > limit:
		return fmt.Sprintf("it is %d characters, more than %d", n, limit)
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return fmt.Sprintf("it holds the control character %U", r)
		}
	}
	return ""
}

// CheckApp reports why a is not an app a repository can name: a slug of
// the app host's form, a name on one line of at most MaxAppName
// characters, and an absolute https address with no credential.
func CheckApp(a ResourceApp) error {
	if !ValidAppSlug(a.Slug) {
		return fmt.Errorf("the slug %q is not an app's slug", a.Slug)
	}
	if why := oneLine(a.Name, MaxAppName); why != "" {
		return fmt.Errorf("the name of %s: %s", a.Slug, why)
	}
	u, err := url.Parse(a.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("the url of %s is not an absolute https address", a.Slug)
	}
	return nil
}

// CheckContext reports why parts cannot be a session's context: a title
// on one line of at most MaxContextTitle characters, a text of at least
// one byte of UTF-8, and at most MaxContext bytes of titles and texts
// together.
func CheckContext(parts []ContextPart) error {
	total := 0
	for i, p := range parts {
		if why := oneLine(p.Title, MaxContextTitle); why != "" {
			return fmt.Errorf("context[%d].title: %s", i, why)
		}
		if p.Text == "" {
			return fmt.Errorf("context[%d].text is empty", i)
		}
		if !utf8.ValidString(p.Text) {
			return fmt.Errorf("context[%d].text is not valid UTF-8", i)
		}
		total += len(p.Title) + len(p.Text)
	}
	if total > MaxContext {
		return fmt.Errorf("context is %d bytes in all, more than %d", total, MaxContext)
	}
	return nil
}

// Requested are the resources of rs the request or the agent named,
// without those an allow attached: what a fork of a session carries
// before its own allow attaches what it reaches (spec 058).
func Requested(rs []Resource) []Resource {
	var out []Resource
	for _, r := range rs {
		if !r.Attached {
			out = append(out, r)
		}
	}
	return out
}

// ErrTooManyRepositories is an attach past MaxRepositories.
var ErrTooManyRepositories = errors.New("session: more repositories than a session holds")

// Attach answers rs, the request's or the agent's resources, followed by
// each of attached the allow named whose URL rs does not already name, in
// the allow's order, each marked Attached; the request's keeps its place
// and its ref. More than MaxRepositories repositories together are
// ErrTooManyRepositories.
func Attach(rs, attached []Resource) ([]Resource, error) {
	out := append([]Resource(nil), rs...)
	named := map[string]bool{}
	for _, r := range rs {
		named[r.URL] = true
	}
	for _, r := range attached {
		if named[r.URL] {
			continue
		}
		named[r.URL] = true
		r.Attached = true
		out = append(out, r)
	}
	if n := len(Repositories(Session{Resources: out})); n > MaxRepositories {
		return nil, fmt.Errorf("%w: %d repositories with the allow's, at most %d", ErrTooManyRepositories, n, MaxRepositories)
	}
	return out, nil
}

// App is the repository resource of s whose app has slug, and the
// directory it is delivered into under the working directory; ok is
// false when no repository of s names that app.
func App(s Session, slug string) (r Resource, dir string, ok bool) {
	repos := Repositories(s)
	dirs := RepositoryDirs(repos)
	for i, r := range repos {
		if r.App != nil && r.App.Slug == slug {
			return r, dirs[i], true
		}
	}
	return Resource{}, "", false
}
