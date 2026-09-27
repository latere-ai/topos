// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

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
