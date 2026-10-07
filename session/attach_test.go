// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestAnAppsCheckoutIsItsSlug: a repository that names an app is
// delivered into its slug, never the working directory, which stays the
// first repository that names none, or no repository at all; a taken
// name gains a suffix.
func TestAnAppsCheckoutIsItsSlug(t *testing.T) {
	app := func(slug string) *ResourceApp {
		return &ResourceApp{Slug: slug, Name: slug, URL: "https://" + slug + ".apps.example"}
	}
	for _, c := range []struct {
		name  string
		repos []Resource
		want  []string
	}{
		{"apps alone", []Resource{{URL: "https://g.example/r/1.git", App: app("tide")}, {URL: "https://g.example/r/2.git", App: app("notes")}}, []string{"tide", "notes"}},
		{"a request's repository first", []Resource{{URL: "https://g.example/acme/web.git"}, {URL: "https://g.example/r/1.git", App: app("tide")}}, []string{"", "tide"}},
		{"an app before the working directory's", []Resource{{URL: "https://g.example/r/1.git", App: app("tide")}, {URL: "https://g.example/acme/web.git"}, {URL: "https://g.example/acme/api.git"}}, []string{"tide", "", "api"}},
		{"a slug a repository took", []Resource{{URL: "https://g.example/acme/web.git"}, {URL: "https://g.example/acme/tide.git"}, {URL: "https://g.example/r/1.git", App: app("tide")}}, []string{"", "tide", "tide-2"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RepositoryDirs(c.repos); !slices.Equal(got, c.want) {
				t.Fatalf("RepositoryDirs = %q, want %q", got, c.want)
			}
		})
	}
	s := Session{Resources: []Resource{{Type: ResourceRepository, URL: "https://g.example/acme/web.git"}, {Type: ResourceRepository, URL: "https://g.example/r/1.git", App: app("tide")}}}
	if r, dir, ok := App(s, "tide"); !ok || dir != "tide" || r.URL != "https://g.example/r/1.git" {
		t.Fatalf("App(tide) = %+v, %q, %v", r, dir, ok)
	}
	if _, _, ok := App(s, "web"); ok {
		t.Fatal("App found a repository that names no app")
	}
}

// TestCheckApp holds an attached app to its rules: a slug of the host's
// form, a name on one line, and an absolute https address.
func TestCheckApp(t *testing.T) {
	ok := ResourceApp{Slug: "tide-tables", Name: "Tide tables", URL: "https://tide-tables.apps.example"}
	if err := CheckApp(ok); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]ResourceApp{
		"an upper-case slug":  {Slug: "Tide", Name: "T", URL: ok.URL},
		"a slug with a dot":   {Slug: "tide.tables", Name: "T", URL: ok.URL},
		"a long slug":         {Slug: strings.Repeat("a", 64), Name: "T", URL: ok.URL},
		"no name":             {Slug: "tide", URL: ok.URL},
		"a name of two lines": {Slug: "tide", Name: "Tide\ntables", URL: ok.URL},
		"a long name":         {Slug: "tide", Name: strings.Repeat("n", MaxAppName+1), URL: ok.URL},
		"an http address":     {Slug: "tide", Name: "T", URL: "http://tide.apps.example"},
		"a relative address":  {Slug: "tide", Name: "T", URL: "/tide"},
		"a credential":        {Slug: "tide", Name: "T", URL: "https://u:p@tide.apps.example"},
	} {
		if err := CheckApp(a); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}

// TestCheckContext holds an allow's context to its rules: a title on one
// line of at most MaxContextTitle characters, a text that is not empty,
// and at most MaxContext bytes together.
func TestCheckContext(t *testing.T) {
	if err := CheckContext([]ContextPart{{Title: "Project", Text: "Tide tables."}, {Title: "Memory", Text: "- The club's color is navy.\n"}}); err != nil {
		t.Fatal(err)
	}
	if err := CheckContext(nil); err != nil {
		t.Fatal(err)
	}
	full := []ContextPart{{Title: "P", Text: strings.Repeat("x", MaxContext-1)}}
	if err := CheckContext(full); err != nil {
		t.Fatalf("exactly %d bytes: %v", MaxContext, err)
	}
	for name, parts := range map[string][]ContextPart{
		"no title":           {{Text: "x"}},
		"a title of 2 lines": {{Title: "a\nb", Text: "x"}},
		"a long title":       {{Title: strings.Repeat("t", MaxContextTitle+1), Text: "x"}},
		"no text":            {{Title: "P"}},
		"invalid UTF-8":      {{Title: "P", Text: "\xff"}},
		"past the bound":     {{Title: "P", Text: strings.Repeat("x", MaxContext)}},
		"past it together":   {{Title: "P", Text: strings.Repeat("x", MaxContext/2)}, {Title: "Q", Text: strings.Repeat("x", MaxContext/2)}},
	} {
		if err := CheckContext(parts); err == nil {
			t.Errorf("%s passed", name)
		}
	}
}

// TestAttach: the allow's repositories follow the request's, each marked
// attached, one whose URL the request names is dropped and the request's
// keeps its place and its ref, more than MaxRepositories together is
// refused, and Requested drops what an allow attached.
func TestAttach(t *testing.T) {
	repo := func(url, ref string) Resource { return Resource{Type: ResourceRepository, URL: url, Ref: ref} }
	rs := []Resource{repo("https://g.example/a.git", "main")}
	app := repo("https://g.example/r/1.git", "")
	app.App = &ResourceApp{Slug: "tide", Name: "Tide", URL: "https://tide.apps.example"}
	got, err := Attach(rs, []Resource{repo("https://g.example/a.git", ""), app, app})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != rs[0] || got[1].App == nil || !got[1].Attached || got[1].URL != app.URL {
		t.Fatalf("Attach = %+v", got)
	}
	if req := Requested(got); len(req) != 1 || req[0] != rs[0] {
		t.Fatalf("Requested = %+v", req)
	}
	var many []Resource
	for i := range MaxRepositories {
		many = append(many, repo("https://g.example/"+strings.Repeat("x", i+1)+".git", ""))
	}
	if _, err := Attach(many[:MaxRepositories-1], []Resource{app}); err != nil {
		t.Fatalf("exactly %d: %v", MaxRepositories, err)
	}
	if _, err := Attach(many, []Resource{app}); !errors.Is(err, ErrTooManyRepositories) {
		t.Fatalf("past %d: %v", MaxRepositories, err)
	}
}
