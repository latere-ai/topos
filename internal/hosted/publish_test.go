// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/internal/publish"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/manifest"
	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// publisherAgent runs bash and publish in a Cella sandbox, and is let to
// publish without a person, as an agent whose approvals allow it is.
const publisherAgent = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: builder
spec:
  model: {name: anthropic/claude-haiku-4.5}
  tools: [bash, publish]
  machine: {kind: cella}
  approvals: {alwaysAllow: [publish]}
`

// appHost is a stub app host over the bare repositories under root,
// served by git's http backend at git: an app's preview deploys are its
// branches' heads, each ready at once, and its releases its v tags. An
// app is private until a PATCH changes its visibility.
type appHost struct {
	t    *testing.T
	root string
	git  string
	srv  *httptest.Server

	mu      sync.Mutex
	created []map[string]string
	bearers []string
	// visibility is who may open each app the host was asked to change,
	// by slug, and patched every change it was asked for, "<slug>
	// <body>", with the bearer each came with.
	visibility     map[string]string
	patched        []string
	patchedBearers []string
	// refuse, when set, is the reason the host's authorizer refuses every
	// change of who can open an app with.
	refuse string
}

var slugWord = regexp.MustCompile(`[^a-z0-9]+`)

func newAppHost(t *testing.T, root, git string) *appHost {
	t.Helper()
	h := &appHost{t: t, root: root, git: git, visibility: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /apps", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.created = append(h.created, body)
		h.mu.Unlock()
		slug := strings.Trim(slugWord.ReplaceAllString(strings.ToLower(body["name"]), "-"), "-")
		repo := filepath.Join(root, slug+".git")
		for _, args := range [][]string{{"init", "--quiet", "--bare", "-b", "main", repo}, {"-C", repo, "config", "http.receivepack", "true"}} {
			if out, err := exec.CommandContext(r.Context(), "git", args...).CombinedOutput(); err != nil {
				http.Error(w, fmt.Sprintf("git %v: %v: %s", args, err, out), http.StatusInternalServerError)
				return
			}
		}
		h.write(w, http.StatusCreated, h.app(slug, body["name"]))
	})
	mux.HandleFunc("GET /apps/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		if !h.exists(slug) {
			h.write(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "app_not_found"}})
			return
		}
		h.write(w, http.StatusOK, h.app(slug, slug))
	})
	// A change of who can open an app, as the app host of spec 043 takes
	// it: a body that names visibility alone, unlisted or private, answered
	// with the app, or refused 403 forbidden with the authorizer's reason
	// in the error's details.
	mux.HandleFunc("PATCH /apps/{slug}", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.patched = append(h.patched, slug+" "+string(raw))
		h.patchedBearers = append(h.patchedBearers, r.Header.Get("Authorization"))
		refuse := h.refuse
		h.mu.Unlock()
		if !h.exists(slug) {
			h.write(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "app_not_found"}})
			return
		}
		if refuse != "" {
			h.write(w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": "forbidden", "message": "You do not have permission to do that.",
				"details": map[string]any{"action": "app.settings", "reason": refuse}}})
			return
		}
		var body map[string]string
		if err := json.Unmarshal(raw, &body); err != nil || len(body) != 1 || (body["visibility"] != "unlisted" && body["visibility"] != "private") {
			h.write(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "invalid_request"}})
			return
		}
		h.mu.Lock()
		h.visibility[slug] = body["visibility"]
		h.mu.Unlock()
		h.write(w, http.StatusOK, h.app(slug, slug))
	})
	mux.HandleFunc("GET /apps/{slug}/deploys", func(w http.ResponseWriter, r *http.Request) {
		slug := r.PathValue("slug")
		var deploys []map[string]any
		for _, line := range h.refs(slug, "refs/heads/") {
			sha, _, _ := strings.Cut(line, " ")
			deploys = append(deploys, map[string]any{"id": "d" + sha[:8], "status": "ready", "preview": true, "commit_sha": sha,
				"preview_url": "https://d" + sha[:7] + "--" + slug + ".apps.example", "error": nil})
		}
		h.write(w, http.StatusOK, map[string]any{"deploys": deploys})
	})
	mux.HandleFunc("GET /apps/{slug}/releases", func(w http.ResponseWriter, r *http.Request) {
		h.write(w, http.StatusOK, map[string]any{"releases": h.releases(r.PathValue("slug"))})
	})
	mux.HandleFunc("GET /apps/{slug}/releases/{tag}", func(w http.ResponseWriter, r *http.Request) {
		for _, rel := range h.releases(r.PathValue("slug")) {
			if rel["tag"] == r.PathValue("tag") {
				h.write(w, http.StatusOK, map[string]any{"release": rel})
				return
			}
		}
		h.write(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "release_not_found"}})
	})
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.bearers = append(h.bearers, r.Header.Get("Authorization"))
		h.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// app is the app resource of slug: private, as this host makes an app
// whose create names no visibility, until a change made it otherwise, its
// current deploy the commit of its highest v tag, and its newest preview
// its newest branch head, each nil when it has none.
func (h *appHost) app(slug, name string) map[string]any {
	h.mu.Lock()
	visibility := cmp.Or(h.visibility[slug], "private")
	h.mu.Unlock()
	a := map[string]any{"slug": slug, "name": name, "url": "https://" + slug + ".apps.example", "visibility": visibility,
		"repository": map[string]string{"push_url": h.git + "/" + slug + ".git"}, "current_deploy": nil, "latest_preview": nil}
	if !h.exists(slug) {
		return a
	}
	best := 0
	for _, line := range h.refs(slug, "refs/tags/") {
		sha, ref, _ := strings.Cut(line, " ")
		if n, err := strconv.Atoi(strings.TrimPrefix(ref, "refs/tags/v")); err == nil && n > best {
			best, a["current_deploy"] = n, map[string]any{"id": "d" + sha[:8], "commit_sha": sha}
		}
	}
	out, err := exec.Command("git", "-C", filepath.Join(h.root, slug+".git"), "for-each-ref", "--sort=-committerdate", "--count=1", "--format=%(objectname)", "refs/heads/").Output()
	if err != nil {
		h.t.Errorf("the newest branch of %s: %v", slug, err)
		return a
	}
	if sha := strings.TrimSpace(string(out)); sha != "" {
		a["latest_preview"] = map[string]any{"id": "d" + sha[:8], "commit_sha": sha}
	}
	return a
}

func (h *appHost) exists(slug string) bool {
	return exec.Command("git", "-C", filepath.Join(h.root, slug+".git"), "rev-parse", "--git-dir").Run() == nil
}

// refs are the refs of an app's repository under prefix, each "<sha> <ref>".
func (h *appHost) refs(slug, prefix string) []string {
	out, err := exec.Command("git", "-C", filepath.Join(h.root, slug+".git"), "for-each-ref", "--format=%(objectname) %(refname)", prefix).Output()
	if err != nil {
		h.t.Errorf("the refs of %s: %v", slug, err)
		return nil
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (h *appHost) releases(slug string) []map[string]any {
	var out []map[string]any
	for _, line := range h.refs(slug, "refs/tags/") {
		sha, ref, _ := strings.Cut(line, " ")
		out = append(out, map[string]any{"tag": strings.TrimPrefix(ref, "refs/tags/"), "commit_sha": sha, "status": "released", "reason": nil})
	}
	return out
}

func (h *appHost) write(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.t.Errorf("write an answer: %v", err)
	}
}

func publishCall(id string, input map[string]any) luxstub.Reply {
	args, err := json.Marshal(input)
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: publish.Name, Args: args}}}, StopReason: ir.StopToolUse}}
}

// TestASessionPublishesAFolderAndReleasesIt: a session whose agent names
// publish creates its app with its title, naming no visibility, so who
// may open it is the host's default, which the result records, with its
// token for the app host and the workload session; its sandbox's git
// pushes the folder
// to the session's branch with the session's author and trailers; the
// result names the preview's address; a second publish reuses the app and
// pushes a new commit on the branch, and one with nothing changed pushes
// none; a release tags the last ready preview v1 and the next v2; and no
// token reaches the sandbox or an event.
func TestASessionPublishesAFolderAndReleasesIt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var name string
	host := &gitHost{}
	host.srv = httptest.NewServer(placeholders(t, host, root, func(n string) bool { return n == name }))
	t.Cleanup(host.srv.Close)
	apps := newAppHost(t, root, host.srv.URL)

	c := newCloud(t, func(s *session.Session) { s.Title = "A poem" })
	rs, err := manifest.Resolve(t.Context(), []byte(publisherAgent), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := runner.AgentRef(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	c.s = session.New(ref, session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, time.Now())
	c.s.Title = "A poem"
	if err := c.st.Create(t.Context(), c.s, blobs); err != nil {
		t.Fatal(err)
	}
	name = cella.SandboxName(c.s.ID)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir, OrigoURL: host.srv.URL})
	c.o.Publish = publish.Options{URL: apps.srv.URL, Audience: "apps", GitURL: host.srv.URL, PollEvery: 10 * time.Millisecond}
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}

	c.drive("Publish a poem.",
		bash("toolu_1", `mkdir -p site && printf '<h1>A poem</h1>\n' > site/index.html && mkdir -p site/node_modules && echo x > site/node_modules/left-out.js`),
		publishCall("toolu_2", map[string]any{"path": "site"}),
		said("Published."))
	first := lastPublish(t, c)
	branch := session.Branch(c.s)
	head := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", "refs/heads/"+branch)
	if first.App != "a-poem" || first.Status != session.PublishReady || first.Commit != head || first.URL != "https://a-poem.apps.example" ||
		first.Preview != "https://d"+head[:7]+"--a-poem.apps.example" || first.Deploy != "d"+head[:8] {
		t.Fatalf("the first publish recorded %+v, the branch is at %s", first, head)
	}
	if _, named := apps.created[0]["visibility"]; len(apps.created) != 1 || apps.created[0]["name"] != "A poem" || named {
		t.Fatalf("the app host was asked to create %v", apps.created)
	}
	if first.Visibility != "private" {
		t.Fatalf("the first publish recorded the visibility %q, want the host's private", first.Visibility)
	}
	msg := gitIn(t, filepath.Join(root, "a-poem.git"), "log", "-1", "--format=%an <%ae>%n%B", head)
	if !strings.Contains(msg, "builder <builder@agents.topos.invalid>") || !strings.Contains(msg, runner.TrailerSession+": "+c.s.ID) || !strings.Contains(msg, runner.TrailerAgent+": "+c.s.Agent.ID+"@1") {
		t.Fatalf("the published commit:\n%s", msg)
	}
	files := gitIn(t, filepath.Join(root, "a-poem.git"), "ls-tree", "-r", "--name-only", head)
	if files != "index.html" {
		t.Fatalf("the published tree holds %q, want index.html alone", files)
	}

	c.drive("Make it longer.",
		bash("toolu_3", `printf '<h1>A poem</h1>\n<p>Two lines.</p>\n' > site/index.html`),
		publishCall("toolu_4", map[string]any{"path": "site"}),
		publishCall("toolu_5", map[string]any{"path": "site"}),
		said("Published again."))
	second := lastPublish(t, c)
	again := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", "refs/heads/"+branch)
	if second.App != "a-poem" || second.Commit != again || again == head || len(apps.created) != 1 {
		t.Fatalf("the second publish recorded %+v, the branch is at %s, %d creates", second, again, len(apps.created))
	}
	if parent := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", again+"^"); parent != head {
		t.Fatalf("the second commit's parent is %s, want %s", parent, head)
	}
	if n := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-list", "--count", "refs/heads/"+branch); n != "2" {
		t.Fatalf("a publish with nothing changed made a commit: %s commits", n)
	}

	c.drive("Release it.", publishCall("toolu_6", map[string]any{"release": true}), said("Released."))
	released := lastPublish(t, c)
	if released.Release != "v1" || released.Status != session.PublishReleased || released.Commit != again || released.URL != "https://a-poem.apps.example" {
		t.Fatalf("the release recorded %+v", released)
	}
	if tagged := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", "refs/tags/v1^{commit}"); tagged != again {
		t.Fatalf("v1 is on %s, want %s", tagged, again)
	}
	c.drive("Change it and release again.",
		bash("toolu_7", `printf '<h1>A poem</h1>\n<p>Three.</p>\n' > site/index.html`),
		publishCall("toolu_8", map[string]any{"path": "site"}),
		publishCall("toolu_9", map[string]any{"release": true}),
		said("Released again."))
	if r := lastPublish(t, c); r.Release != "v2" || r.Status != session.PublishReleased {
		t.Fatalf("the second release recorded %+v", r)
	}

	apps.mu.Lock()
	bearers := slices.Clone(apps.bearers)
	apps.mu.Unlock()
	for _, b := range bearers {
		if !strings.HasPrefix(b, "Bearer apps-session-") {
			t.Fatalf("the app host saw %q, want the session's own token for it", b)
		}
	}
	host.mu.Lock()
	seen := slices.Clone(host.seen)
	host.mu.Unlock()
	if len(seen) == 0 || slices.ContainsFunc(seen, func(a string) bool { return a != "Bearer cella-placeholder-"+name+"-origo" }) {
		t.Fatalf("the git host saw %q, want the placeholder alone", seen)
	}
	sb, ok := c.cella.Sandbox(name)
	if !ok {
		t.Fatal("no sandbox")
	}
	for _, v := range sb.Spec.Env {
		if strings.Contains(v, "apps-session-") {
			t.Fatal("the sandbox holds the app host's token")
		}
	}
	if strings.Contains(c.dump(), "apps-session-") {
		t.Fatal("an event carries the app host's token")
	}
}

// lastPublish is the meta of the session's last publish result, which
// must have succeeded.
func lastPublish(t *testing.T, c *cloud) session.PublishMeta {
	t.Helper()
	var last *session.PublishMeta
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
		if p.IsError {
			t.Fatalf("a publish failed: %+v\nevents %s", p, c.dump())
		}
		last = m.Publish
	}
	if last == nil {
		t.Fatalf("no publish result\nevents %s", c.dump())
	}
	return *last
}

// TestPublishIsOfferedWhenConfigured: the runner offers publish to a
// session whose agent names it, in a sandbox, on an installation with an
// app host, and to no other.
func TestPublishIsOfferedWhenConfigured(t *testing.T) {
	configured := publish.Options{URL: "https://apps.example", GitURL: "https://git.example"}
	for name, c := range map[string]struct {
		doc  string
		o    publish.Options
		want bool
	}{
		"named and configured": {publisherAgent, configured, true},
		"not configured":       {publisherAgent, publish.Options{}, false},
		"not named":            {builderAgent, configured, false},
		"on the host":          {strings.Replace(publisherAgent, "kind: cella", "kind: host", 1), configured, false},
	} {
		st := session.NewMemoryStore()
		s := newSession(t, st, c.doc)
		var asked bool
		h, err := Harness(Options{Store: st, ModelsURL: "https://lux.example", ModelsKey: "k", Publish: c.o, Machines: hostMachines(t, &v1.Machine{})})
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := h(t.Context(), s)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, asked = cfg.Tools.Get(publish.Name)
		if asked != c.want {
			t.Errorf("%s: offered %v, want %v", name, asked, c.want)
		}
	}
}

// publishUses are the session's agent.tool_use events of the publish tool,
// by the call's id.
func publishUses(t *testing.T, c *cloud) map[string]session.AgentToolUse {
	t.Helper()
	out := map[string]session.AgentToolUse{}
	for _, e := range c.events(session.TypeAgentToolUse) {
		var use session.AgentToolUse
		if err := e.Decode(&use); err != nil {
			t.Fatal(err)
		}
		if use.Name == publish.Name {
			out[use.ToolUseID] = use
		}
	}
	return out
}

// publishResult is the tool.result of the call id.
func publishResult(t *testing.T, c *cloud, id string) session.ToolResult {
	t.Helper()
	for _, e := range c.events(session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.ToolUseID == id {
			return p
		}
	}
	t.Fatalf("no result of %s\nevents %s", id, c.dump())
	return session.ToolResult{}
}

// TestASessionChangesWhoCanOpenItsApp: a call with access waits for the
// person in a session whose agent always allows publish, where a preview
// and a release run without asking, and after the person's allow of an
// earlier one added publish to the session's own patterns; until the
// person allows it, the app host is asked for no change. Allowed, it asks
// the host for unlisted on link and private on private with the session's
// token and nothing else, and its result names the app with the visibility
// the host answers and no status. A denied call changes nothing, a call
// that also asks for a release is refused before it is asked, the host's
// refusal says that only an organization's owners and admins change who
// can open its app, and a release after all of it still publishes the
// folder the thread published (spec 043).
func TestASessionChangesWhoCanOpenItsApp(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var name string
	host := &gitHost{}
	host.srv = httptest.NewServer(placeholders(t, host, root, func(n string) bool { return n == name }))
	t.Cleanup(host.srv.Close)
	apps := newAppHost(t, root, host.srv.URL)
	if !strings.Contains(publisherAgent, "alwaysAllow: [publish]") {
		t.Fatal("the publisher agent no longer always allows publish")
	}
	c := newCloudOf(t, publisherAgent, func(s *session.Session) { s.Title = "A poem" })
	name = cella.SandboxName(c.s.ID)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir, OrigoURL: host.srv.URL})
	c.o.Publish = publish.Options{URL: apps.srv.URL, Audience: "apps", GitURL: host.srv.URL, PollEvery: 10 * time.Millisecond}
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	asked := func() []string {
		apps.mu.Lock()
		defer apps.mu.Unlock()
		return slices.Clone(apps.patched)
	}
	const held = "changes who can open the app, which a person approves every time"

	// The publish runs unasked, and the change of who can open the app
	// waits: the host is asked for nothing.
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Publish a poem and share it with my friends.")},
		bash("toolu_1", `mkdir -p site && printf '<h1>A poem</h1>\n' > site/index.html`),
		publishCall("toolu_2", map[string]any{"path": "site"}),
		publishCall("toolu_3", map[string]any{"access": "link"}))
	uses := publishUses(t, c)
	if uses["toolu_2"].Verdict != "allow" || uses["toolu_3"].Verdict != "ask" || uses["toolu_3"].Reason != held || string(uses["toolu_3"].Input) != `{"access":"link"}` {
		t.Fatalf("the calls were decided %+v and %+v", uses["toolu_2"], uses["toolu_3"])
	}
	if first := lastPublish(t, c); first.Status != session.PublishReady || first.Visibility != "private" || first.Folder != "site" {
		t.Fatalf("the publish recorded %+v", first)
	}
	if got := asked(); len(got) != 0 {
		t.Fatalf("the host was asked for %q before the person answered", got)
	}

	// The person allows it and lets later publish calls run without
	// asking: the host makes the app unlisted, for the session's token.
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_3", Decision: session.DecisionAllow, Remember: publish.Name})},
		said("Anyone with the link can open it."))
	if got := asked(); !slices.Equal(got, []string{`a-poem {"visibility":"unlisted"}`}) {
		t.Fatalf("the host was asked for %q", got)
	}
	opened := publishResult(t, c, "toolu_3")
	var meta struct {
		Publish session.PublishMeta `json:"publish"`
	}
	if err := json.Unmarshal(opened.Meta, &meta); err != nil {
		t.Fatal(err)
	}
	want := session.PublishMeta{App: "a-poem", Name: "a-poem", URL: "https://a-poem.apps.example", Visibility: "unlisted", Folder: "site"}
	if opened.IsError || opened.Outcome != "ok" || meta.Publish != want || !strings.Contains(opened.Content[0].Text, "Anyone who has the address can open a-poem, without signing in: https://a-poem.apps.example.") {
		t.Fatalf("the change recorded %+v with %+v", opened, meta.Publish)
	}

	// The next change waits again, whatever the agent always allows and
	// the person's earlier answer remembered, and a denial changes nothing.
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Make it private again.")}, publishCall("toolu_4", map[string]any{"access": "private"}))
	if use := publishUses(t, c)["toolu_4"]; use.Verdict != "ask" || use.Reason != held {
		t.Fatalf("a change after a remembered allow was decided %+v", use)
	}
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_4", Decision: session.DecisionDeny, Note: "leave it"})},
		said("It stays as it is."))
	if got := publishResult(t, c, "toolu_4"); got.Outcome != "denied" || len(asked()) != 1 {
		t.Fatalf("a denied change recorded %+v, the host was asked for %q", got, asked())
	}
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Private, please.")}, publishCall("toolu_5", map[string]any{"access": "private", "app": "a-poem"}))
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_5", Decision: session.DecisionAllow})}, said("It is private."))
	if got, _ := lastResult(t, c); !slices.Equal(asked(), []string{`a-poem {"visibility":"unlisted"}`, `a-poem {"visibility":"private"}`}) || got.Visibility != "private" || got.Status != "" || got.Error != nil {
		t.Fatalf("the change back recorded %+v, the host was asked for %q", got, asked())
	}

	// A call that asks for a release beside the change is refused before
	// it is decided: nobody is asked, and nothing is published or changed.
	c.drive("Release it for everyone.", publishCall("toolu_6", map[string]any{"access": "link", "release": true}), said("That takes two calls."))
	if _, decided := publishUses(t, c)["toolu_6"]; decided {
		t.Fatal("a call with access and release was put to a decision")
	}
	if got, text := lastResult(t, c); got.Error == nil || got.Error.Code != session.PublishAccessAlone || got.App != "" || !strings.Contains(text, "it takes neither path nor release") || len(asked()) != 2 {
		t.Fatalf("a call with access and release recorded %+v: %q", got, text)
	}

	// The host's authorizer refuses the change of an organization's app
	// for a member, and the result says so.
	apps.mu.Lock()
	apps.refuse = "role_insufficient"
	apps.mu.Unlock()
	c.driveAfter(session.StopToolConfirmation, []session.Event{c.message("Share it.")}, publishCall("toolu_7", map[string]any{"access": "link"}))
	c.driveAfter(session.StopEndTurn, []session.Event{c.confirmation(session.UserToolConfirmation{ToolUseID: "toolu_7", Decision: session.DecisionAllow})}, said("Only an owner or an admin can."))
	refused, text := lastResult(t, c)
	if refused.Error == nil || refused.Error.Code != "forbidden" || refused.Error.Reason != session.PublishRoleInsufficient || refused.Visibility != "private" || refused.Status != "" ||
		!strings.Contains(text, "only the organization's owners and admins change who can open it") {
		t.Fatalf("a refused change recorded %+v: %q", refused, text)
	}
	if got := publishResult(t, c, "toolu_7"); !got.IsError || got.Outcome != "error" {
		t.Fatalf("a refused change is recorded %+v", got)
	}

	// Every change went with the session's own token for the app host, and
	// none asked for public.
	apps.mu.Lock()
	bearers, changes, creates := slices.Clone(apps.patchedBearers), slices.Clone(apps.patched), len(apps.created)
	apps.refuse = ""
	apps.mu.Unlock()
	if len(bearers) != 3 || slices.ContainsFunc(bearers, func(b string) bool { return !strings.HasPrefix(b, "Bearer apps-session-") }) {
		t.Fatalf("the changes came with %q, want the session's own token for the app host", bearers)
	}
	if slices.ContainsFunc(changes, func(c string) bool { return strings.Contains(c, "public") }) || creates != 1 {
		t.Fatalf("the host was asked for %q and to create %d apps", changes, creates)
	}

	// A release that names no folder still publishes the folder the thread
	// published, as it is now, past the results that changed who can open
	// the app.
	preview := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", "refs/heads/"+session.Branch(c.s))
	c.drive("Add a line and release it.",
		bash("toolu_8", `printf '<h1>A poem</h1>\n<p>Two lines.</p>\n' > site/index.html`),
		publishCall("toolu_9", map[string]any{"release": true}), said("Released."))
	released, _ := lastResult(t, c)
	head := gitIn(t, filepath.Join(root, "a-poem.git"), "rev-parse", "refs/heads/"+session.Branch(c.s))
	if released.Release != "v1" || released.Status != session.PublishReleased || released.Commit != head || head == preview || released.Folder != "site" || released.Visibility != "private" {
		t.Fatalf("the release recorded %+v, the branch is at %s and the preview was %s", released, head, preview)
	}
	if page := gitIn(t, filepath.Join(root, "a-poem.git"), "show", "refs/tags/v1:index.html"); !strings.Contains(page, "Two lines.") {
		t.Fatalf("the released page reads %q", page)
	}
	if strings.Contains(c.dump(), "apps-session-") {
		t.Fatal("an event carries the app host's token")
	}
}
