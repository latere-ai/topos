// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/internal/publish"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// apps is an app host and a git host over the bare repositories under
// root, which accepts the sandboxes of the sessions it names.
type apps struct {
	root  string
	git   *gitHost
	host  *appHost
	names map[string]bool
}

func newApps(t *testing.T) *apps {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &apps{root: root, git: &gitHost{}, names: map[string]bool{}}
	a.git.srv = httptest.NewServer(placeholders(t, a.git, root, func(n string) bool { return a.names[n] }))
	t.Cleanup(a.git.srv.Close)
	a.host = newAppHost(t, root, a.git.srv.URL)
	return a
}

// seed creates the app slug's repository with index.html on main, as a
// person's first push leaves it, and answers a clone of it to release
// from.
func (a *apps) seed(t *testing.T, slug string) string {
	t.Helper()
	bare := filepath.Join(a.root, slug+".git")
	gitIn(t, a.root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitIn(t, bare, "config", "http.receivepack", "true")
	clone := filepath.Join(t.TempDir(), slug)
	gitIn(t, a.root, "clone", "--quiet", bare, clone)
	if err := os.WriteFile(filepath.Join(clone, "index.html"), []byte("<h1>Tide</h1>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", ".")
	gitIn(t, clone, "commit", "--quiet", "-m", "Start")
	gitIn(t, clone, "push", "--quiet", "origin", "HEAD:main")
	return clone
}

// release commits file in the clone on a branch of another session from
// main and releases it as tag, as another session's release leaves the
// repository, and answers the released commit.
func release(t *testing.T, clone, branch, file, tag string) string {
	t.Helper()
	gitIn(t, clone, "checkout", "--quiet", "-B", branch, "origin/main")
	if err := os.WriteFile(filepath.Join(clone, file), []byte(file+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, clone, "add", ".")
	gitIn(t, clone, "commit", "--quiet", "-m", "Release "+file)
	gitIn(t, clone, "tag", tag)
	gitIn(t, clone, "push", "--quiet", "origin", "HEAD:refs/heads/"+branch, tag)
	return gitIn(t, clone, "rev-parse", "HEAD")
}

// attached is a cloud of the publisher agent whose session the allow
// attached the app slug, on the app host and the git host of a.
func (a *apps) attached(t *testing.T, slug string) *cloud {
	t.Helper()
	c := newCloud(t, nil)
	rs, err := manifest.Resolve(t.Context(), []byte(publisherAgent), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := runner.AgentRef(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	c.s = session.New(ref, session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, time.Now())
	c.s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: a.git.srv.URL + "/" + slug + ".git", Attached: true,
		App: &session.ResourceApp{Slug: slug, Name: "Tide tables", URL: "https://" + slug + ".apps.example"}}}
	if err := c.st.Create(t.Context(), c.s, blobs); err != nil {
		t.Fatal(err)
	}
	a.names[cella.SandboxName(c.s.ID)] = true
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir, OrigoURL: a.git.srv.URL})
	c.o.Publish = publish.Options{URL: a.host.srv.URL, Audience: "apps", GitURL: a.git.srv.URL, PollEvery: 10 * time.Millisecond}
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	return c
}

// lastResult is the meta and the text of the session's last publish
// result, whatever its outcome.
func lastResult(t *testing.T, c *cloud) (session.PublishMeta, string) {
	t.Helper()
	var meta session.PublishMeta
	var text string
	found := false
	for _, e := range c.events(session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		var m struct {
			Publish *session.PublishMeta `json:"publish"`
		}
		if len(p.Meta) == 0 || json.Unmarshal(p.Meta, &m) != nil || m.Publish == nil {
			continue
		}
		meta, found = *m.Publish, true
		text = ""
		for _, b := range p.Content {
			text += b.Text
		}
	}
	if !found {
		t.Fatalf("no publish result\nevents %s", c.dump())
	}
	return meta, text
}

// home is the sandbox's home directory in the stub Cella.
func home(c *cloud) string {
	return filepath.Join(filepath.Dir(c.cella.Workspace(cella.SandboxName(c.s.ID))), "home")
}

// TestASessionPublishesAnAttachedApp: a session the allow attached an
// app checks it out at the commit the app serves, the released one,
// which a tag alone holds; publish with the app's slug commits the
// checkout on the session's branch with the session's author and
// trailers and pushes that branch, building on the live release; the
// session's own git directory is never made; a publish with nothing
// changed pushes no new commit; and a release whose commit descends from
// the live one is tagged.
func TestASessionPublishesAnAttachedApp(t *testing.T) {
	a := newApps(t)
	clone := a.seed(t, "tide")
	live := release(t, clone, "agents/builder/ses_other", "released.html", "v1")
	gitIn(t, filepath.Join(a.root, "tide.git"), "update-ref", "-d", "refs/heads/agents/builder/ses_other")
	c := a.attached(t, "tide")
	c.drive("Make the page say hello.",
		bash("toolu_1", `printf '<h1>Hello</h1>\n' > tide/index.html && mkdir -p tide/node_modules && echo x > tide/node_modules/left-out.js`),
		publishCall("toolu_2", map[string]any{"app": "tide"}),
		said("Published."))
	first, _ := lastResult(t, c)
	branch := session.Branch(c.s)
	bare := filepath.Join(a.root, "tide.git")
	head := gitIn(t, bare, "rev-parse", "refs/heads/"+branch)
	if first.App != "tide" || !first.Attached || first.Status != session.PublishReady || first.Commit != head || first.Error != nil {
		t.Fatalf("the first publish recorded %+v, the branch is at %s", first, head)
	}
	if parent := gitIn(t, bare, "rev-parse", head+"^"); parent != live {
		t.Fatalf("the branch builds on %s, want the live release %s", parent, live)
	}
	msg := gitIn(t, bare, "log", "-1", "--format=%an <%ae>%n%B", head)
	for _, want := range []string{"builder <builder@agents.topos.invalid>", "Publish tide", runner.TrailerSession + ": " + c.s.ID} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the published commit lacks %q:\n%s", want, msg)
		}
	}
	if files := gitIn(t, bare, "ls-tree", "-r", "--name-only", head); files != "index.html\nreleased.html" {
		t.Fatalf("the published tree holds %q", files)
	}
	if _, err := os.Stat(filepath.Join(home(c), ".topos", "publish")); !os.IsNotExist(err) {
		t.Fatalf("an attached app made the session's git directory: %v", err)
	}
	var attachedMachine session.SessionMachine
	if ms := c.events(session.TypeSessionMachine); len(ms) == 0 || ms[0].Decode(&attachedMachine) != nil || len(attachedMachine.Repositories) != 1 ||
		attachedMachine.Repositories[0].Base != session.BaseLive || attachedMachine.Repositories[0].Commit != live || attachedMachine.Repositories[0].App != "tide" {
		t.Fatalf("the delivery recorded %+v", attachedMachine.Repositories)
	}

	c.drive("Publish it again.", publishCall("toolu_3", map[string]any{"app": "tide"}), said("Published again."))
	if n := gitIn(t, bare, "rev-list", "--count", "refs/heads/"+branch); n != "3" {
		t.Fatalf("a publish with nothing changed made a commit: %s commits", n)
	}

	c.drive("Release it.", publishCall("toolu_4", map[string]any{"app": "tide", "release": true}), said("Released."))
	released, _ := lastResult(t, c)
	if released.Status != session.PublishReleased || released.Release != "v2" || !released.Attached || released.Commit != head {
		t.Fatalf("the release recorded %+v", released)
	}
	if tagged := gitIn(t, bare, "rev-parse", "refs/tags/v2^{commit}"); tagged != head {
		t.Fatalf("v2 is on %s, want %s", tagged, head)
	}
}

// TestAReleaseBehindTheLiveVersionIsRefused: a release of an attached app
// with nothing live is tagged at once; once another session's release is
// live, a release whose commit does not hold it is refused behind_live,
// no tag is pushed, and the text names the merge; after the merge the
// commands name, a publish and a release are tagged on a commit that
// holds both.
func TestAReleaseBehindTheLiveVersionIsRefused(t *testing.T) {
	a := newApps(t)
	clone := a.seed(t, "tide")
	c := a.attached(t, "tide")
	bare := filepath.Join(a.root, "tide.git")
	c.drive("Publish and release.",
		bash("toolu_1", `printf '<h1>Ours</h1>\n' > tide/ours.html`),
		publishCall("toolu_2", map[string]any{"app": "tide"}),
		publishCall("toolu_3", map[string]any{"release": true}),
		said("Released."))
	if r, _ := lastResult(t, c); r.Status != session.PublishReleased || r.Release != "v1" {
		t.Fatalf("a release with nothing live recorded %+v", r)
	}
	theirs := release(t, clone, "agents/builder/ses_other", "theirs.html", "v2")

	c.drive("Change it and release.",
		bash("toolu_4", `printf '<h1>Ours, again</h1>\n' > tide/ours.html`),
		publishCall("toolu_5", map[string]any{"app": "tide"}),
		publishCall("toolu_6", map[string]any{"app": "tide", "release": true}),
		said("Refused."))
	refused, text := lastResult(t, c)
	if refused.Status != session.PublishRefused || refused.Error == nil || refused.Error.Code != session.PublishBehindLive || refused.Release != "" || !refused.Attached {
		t.Fatalf("a release behind the live one recorded %+v", refused)
	}
	if !strings.Contains(text, "tide is live at "+theirs[:7]) || !strings.Contains(text, "git -C tide merge "+theirs[:7]) {
		t.Fatalf("the refusal reads %q", text)
	}
	if tags := gitIn(t, bare, "tag", "--list"); tags != "v1\nv2" {
		t.Fatalf("a refused release pushed a tag: %q", tags)
	}

	c.drive("Merge the live release and release again.",
		bash("toolu_7", `git -C tide fetch -q origin --tags && git -C tide merge -q --no-edit `+theirs[:7]),
		publishCall("toolu_8", map[string]any{"app": "tide"}),
		publishCall("toolu_9", map[string]any{"app": "tide", "release": true}),
		said("Released."))
	r, _ := lastResult(t, c)
	if r.Status != session.PublishReleased || r.Release != "v3" {
		t.Fatalf("the release after the merge recorded %+v", r)
	}
	tagged := gitIn(t, bare, "rev-parse", "refs/tags/v3^{commit}")
	gitIn(t, bare, "merge-base", "--is-ancestor", theirs, tagged)
	if files := gitIn(t, bare, "ls-tree", "-r", "--name-only", tagged); files != "index.html\nours.html\ntheirs.html" {
		t.Fatalf("v3 holds %q", files)
	}
}

// TestTwoSessionsBuildOneApp: two sessions attached to one app, both
// checked out before either publishes, each publish on their own branch;
// the first releases; the second is refused behind_live until it merges
// the first's release, then is released on a commit that holds both
// sessions' work.
func TestTwoSessionsBuildOneApp(t *testing.T) {
	a := newApps(t)
	a.seed(t, "tide")
	first, second := a.attached(t, "tide"), a.attached(t, "tide")
	bare := filepath.Join(a.root, "tide.git")
	first.drive("Look at the app.", bash("toolu_0", `ls tide`), said("Looked."))
	second.drive("Look at the app.", bash("toolu_0", `ls tide`), said("Looked."))
	first.drive("Add a page.", bash("toolu_1", `printf 'one\n' > tide/one.html`), publishCall("toolu_2", map[string]any{"app": "tide"}), said("Published."))
	second.drive("Add another page.", bash("toolu_1", `printf 'two\n' > tide/two.html`), publishCall("toolu_2", map[string]any{"app": "tide"}), said("Published."))
	a1 := gitIn(t, bare, "rev-parse", "refs/heads/"+session.Branch(first.s))
	b1 := gitIn(t, bare, "rev-parse", "refs/heads/"+session.Branch(second.s))
	if a1 == b1 {
		t.Fatal("two sessions pushed one branch")
	}
	first.drive("Release.", publishCall("toolu_3", map[string]any{"app": "tide", "release": true}), said("Released."))
	if r, _ := lastResult(t, first); r.Release != "v1" || r.Status != session.PublishReleased {
		t.Fatalf("the first session's release recorded %+v", r)
	}
	second.drive("Release.", publishCall("toolu_3", map[string]any{"app": "tide", "release": true}), said("Refused."))
	if r, _ := lastResult(t, second); r.Error == nil || r.Error.Code != session.PublishBehindLive {
		t.Fatalf("the second session's release recorded %+v", r)
	}
	second.drive("Merge and release.",
		bash("toolu_4", `git -C tide fetch -q origin --tags && git -C tide merge -q --no-edit `+a1[:7]),
		publishCall("toolu_5", map[string]any{"app": "tide"}),
		publishCall("toolu_6", map[string]any{"app": "tide", "release": true}),
		said("Released."))
	if r, _ := lastResult(t, second); r.Release != "v2" || r.Status != session.PublishReleased {
		t.Fatalf("the second session's release after the merge recorded %+v", r)
	}
	tagged := gitIn(t, bare, "rev-parse", "refs/tags/v2^{commit}")
	if files := gitIn(t, bare, "ls-tree", "-r", "--name-only", tagged); files != "index.html\none.html\ntwo.html" {
		t.Fatalf("v2 holds %q", files)
	}
}
