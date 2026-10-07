// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"fmt"
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
// branches' heads, each ready at once, and its releases its v tags.
type appHost struct {
	t    *testing.T
	root string
	git  string
	srv  *httptest.Server

	mu      sync.Mutex
	created []map[string]string
	bearers []string
}

var slugWord = regexp.MustCompile(`[^a-z0-9]+`)

func newAppHost(t *testing.T, root, git string) *appHost {
	t.Helper()
	h := &appHost{t: t, root: root, git: git}
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

// app is the app resource of slug: its current deploy the commit of its
// highest v tag, and its newest preview its newest branch head, each nil
// when it has none.
func (h *appHost) app(slug, name string) map[string]any {
	a := map[string]any{"slug": slug, "name": name, "url": "https://" + slug + ".apps.example",
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
// publish creates its app with its title, public, with its token for the
// app host and the workload session; its sandbox's git pushes the folder
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
	if len(apps.created) != 1 || apps.created[0]["name"] != "A poem" || apps.created[0]["visibility"] != "public" {
		t.Fatalf("the app host was asked to create %v", apps.created)
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
