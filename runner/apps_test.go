// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// releasedRepo is bareRepo with a release: a commit on a session's
// branch alone, tagged v1, as an app host's release of another session's
// work leaves it, and returns the repository and that commit.
func releasedRepo(t *testing.T, name string) (string, string) {
	t.Helper()
	bare := bareRepo(t, name)
	seed := filepath.Join(filepath.Dir(bare), "release")
	gitRun(t, filepath.Dir(bare), "clone", "--quiet", bare, seed)
	gitRun(t, seed, "checkout", "--quiet", "-b", "agents/builder/ses_other")
	write(t, filepath.Join(seed, "index.html"), "<h1>Released</h1>\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "--quiet", "-m", "Release")
	gitRun(t, seed, "tag", "v1")
	gitRun(t, seed, "push", "--quiet", "origin", "HEAD:refs/heads/agents/builder/ses_other", "v1")
	// The session's branch is gone after its release; the tag holds it.
	gitRun(t, seed, "push", "--quiet", "origin", ":refs/heads/agents/builder/ses_other")
	return bare, gitRun(t, seed, "rev-parse", "HEAD")
}

// TestAnAppsCheckoutStartsAtItsLiveCommit: each repository an allow
// attached with an app is delivered into the directory of its slug,
// never the working directory, which stays the request's repository's,
// on the session's branch at the app's live commit, which only a tag
// holds; else at its newest ready preview's; else at the default branch,
// when the host has neither, cannot be read, or names a commit the
// repository does not hold; a ref the allow named wins. The attachment
// records each app's slug, the base it started from and its commit.
func TestAnAppsCheckoutStartsAtItsLiveCommit(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	web := bareRepo(t, "web")
	live, released := releasedRepo(t, "live")
	preview := bareRepo(t, "preview")
	gone, down, pinned := bareRepo(t, "gone"), bareRepo(t, "down"), bareRepo(t, "pinned")
	work := filepath.Join(filepath.Dir(f.work), "fresh")
	if err := os.MkdirAll(filepath.Join(work, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	served := map[string]AppCommits{
		"tide":    {Live: released, Preview: gitRun(t, live, "rev-parse", "refs/heads/dev")},
		"preview": {Preview: gitRun(t, preview, "rev-parse", "refs/heads/dev")},
		"gone":    {Live: strings.Repeat("ab", 20)},
		"pinned":  {Live: released},
	}
	var asked []string
	var opens atomic.Int32
	f.onDemandWith(work, &opens, demand{apps: func(_ context.Context, _ *TokenSource, slug string) (AppCommits, error) {
		asked = append(asked, slug)
		if slug == "down" {
			return AppCommits{}, errors.New("the app host answered 503")
		}
		return served[slug], nil
	}})
	app := func(slug string) *session.ResourceApp {
		return &session.ResourceApp{Slug: slug, Name: slug, URL: "https://" + slug + ".apps.example.com"}
	}
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 3}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	s.Resources = []session.Resource{
		{Type: session.ResourceRepository, URL: "file://" + web},
		{Type: session.ResourceRepository, URL: "file://" + live, App: app("tide"), Attached: true},
		{Type: session.ResourceRepository, URL: "file://" + preview, App: app("preview"), Attached: true},
		{Type: session.ResourceRepository, URL: "file://" + gone, App: app("gone"), Attached: true},
		{Type: session.ResourceRepository, URL: "file://" + down, App: app("down"), Attached: true},
		{Type: session.ResourceRepository, URL: "file://" + pinned, Ref: "dev", App: app("pinned"), Attached: true},
	}
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.stub.Script(model,
		reply(toolUse("toolu_1", "bash", `{"command":"ls"}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Listed."}),
	)
	f.message(ctx, "List the apps.")
	if _, err := f.r.Drive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	branch := session.Branch(s)
	if got := gitRun(t, work, "remote", "get-url", "origin"); got != "file://"+web {
		t.Fatalf("the working directory is %s, want the request's repository", got)
	}
	if got := gitRun(t, filepath.Join(work, "tide"), "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the app's checkout is on %q, want %q", got, branch)
	}
	if !slices.Equal(asked, []string{"tide", "preview", "gone", "down"}) {
		t.Fatalf("the host was asked about %v; an app with a ref is not asked", asked)
	}
	machines := f.events(ctx, session.TypeSessionMachine)
	var attached session.SessionMachine
	if len(machines) != 1 || machines[0].Decode(&attached) != nil {
		t.Fatalf("session.machine %+v", machines)
	}
	want := []session.DeliveredRepository{
		{URL: "file://" + web, Branch: branch, Commit: gitRun(t, web, "rev-parse", "refs/heads/main")},
		{URL: "file://" + live, Branch: branch, Commit: released, App: "tide", Base: session.BaseLive},
		{URL: "file://" + preview, Branch: branch, Commit: served["preview"].Preview, App: "preview", Base: session.BasePreview},
		{URL: "file://" + gone, Branch: branch, Commit: gitRun(t, gone, "rev-parse", "refs/heads/main"), App: "gone", Base: session.BaseDefault,
			BaseError: "the repository does not hold the commit " + served["gone"].Live},
		{URL: "file://" + down, Branch: branch, Commit: gitRun(t, down, "rev-parse", "refs/heads/main"), App: "down", Base: session.BaseDefault,
			BaseError: "the app host could not be read: the app host answered 503"},
		{URL: "file://" + pinned, Branch: branch, Commit: gitRun(t, pinned, "rev-parse", "refs/heads/dev"), App: "pinned"},
	}
	if !slices.Equal(attached.Repositories, want) {
		t.Fatalf("the attachment names\n%+v\nwant\n%+v", attached.Repositories, want)
	}
	for _, dir := range []string{"tide", "preview", "gone", "down", "pinned"} {
		if _, err := os.Stat(filepath.Join(work, dir, ".git")); err != nil {
			t.Fatalf("no checkout at %s: %v", dir, err)
		}
	}
}

// TestBaseWithNoAppHost: with no app host an app's checkout starts at the
// default branch and nothing is wrong; a commit that is no commit's id
// is not used.
func TestBaseWithNoAppHost(t *testing.T) {
	if c, kind, why := base(t.Context(), nil, "tide"); c != "" || kind != session.BaseDefault || why != "" {
		t.Fatalf("no app host: %q %q %q", c, kind, why)
	}
	odd := func(context.Context, string) (AppCommits, error) { return AppCommits{Live: "main; rm -rf /"}, nil }
	if c, kind, why := base(t.Context(), odd, "tide"); c != "" || kind != session.BaseDefault || !strings.Contains(why, "no commit's id") {
		t.Fatalf("an odd live commit: %q %q %q", c, kind, why)
	}
	oddPreview := func(context.Context, string) (AppCommits, error) { return AppCommits{Preview: "HEAD"}, nil }
	if c, kind, why := base(t.Context(), oddPreview, "tide"); c != "" || kind != session.BaseDefault || !strings.Contains(why, "no commit's id") {
		t.Fatalf("an odd preview commit: %q %q %q", c, kind, why)
	}
}
