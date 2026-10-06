// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/manifest"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/cellastub"
	"latere.ai/x/topos/test/stubs/luxstub"
)

const haiku = "anthropic/claude-haiku-4.5"

// builder is an agent that runs bash in a Cella sandbox.
const builderAgent = `apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: builder
spec:
  model: {name: anthropic/claude-haiku-4.5}
  tools: [bash]
  machine: {kind: cella}
`

var (
	helperOnce sync.Once
	helperDir  string
	helperErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if helperDir != "" {
		if err := os.RemoveAll(helperDir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
	}
	os.Exit(code)
}

// helpers are the topos-machine builds for the machine the tests run on,
// where the stub Cella runs its sandboxes' commands, built once.
func helpers(t *testing.T) map[string][]byte {
	t.Helper()
	helperOnce.Do(func() {
		if helperDir, helperErr = os.MkdirTemp("", "topos-machine-"); helperErr != nil {
			return
		}
		build := exec.Command("go", "build", "-o", filepath.Join(helperDir, "topos-machine-"+runtime.GOOS+"-"+runtime.GOARCH), "latere.ai/x/topos/cmd/topos-machine")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := build.CombinedOutput(); err != nil {
			helperErr = fmt.Errorf("build the helper: %w\n%s", err, b)
		}
	})
	if helperErr != nil {
		t.Fatal(helperErr)
	}
	h, err := ReadHelpers(helperDir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// cloud is a hosted runner over a store, a stub Lux and a stub Cella.
type cloud struct {
	t     *testing.T
	st    session.Store
	lux   *luxstub.Server
	cella *cellastub.Server
	o     Options
	s     session.Session
	// creds are the session's credentials the runner reaches; nil acts
	// with the installation's own.
	creds runner.Credentials
	// checkpointHost is the runner's CheckpointHost.
	checkpointHost string
	// blobs are the agent's, which a fork of the session carries.
	blobs map[session.Digest][]byte
}

// newCloud is a cloud whose sessions run the builder agent, with its
// session changed by mut before it is created.
func newCloud(t *testing.T, mut func(*session.Session)) *cloud {
	t.Helper()
	return newCloudOf(t, builderAgent, mut)
}

// newCloudOf is a cloud whose sessions run the agent the document
// defines.
func newCloudOf(t *testing.T, agentDoc string, mut func(*session.Session)) *cloud {
	t.Helper()
	c := &cloud{t: t, st: session.NewMemoryStore(), lux: luxstub.New(t), cella: cellastub.New(t)}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o = Options{Store: c.st, ModelsURL: c.lux.URL() + "/anthropic", ModelsKey: "k",
		Machines: Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir})}
	rs, err := manifest.Resolve(t.Context(), []byte(agentDoc), manifest.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, blobs, err := runner.AgentRef(rs[0])
	if err != nil {
		t.Fatal(err)
	}
	c.s = session.New(ref, session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, session.RunnerHosted, session.Machine{Kind: session.MachineCella}, time.Now())
	if mut != nil {
		mut(&c.s)
	}
	if err := c.st.Create(t.Context(), c.s, blobs); err != nil {
		t.Fatal(err)
	}
	c.blobs = blobs
	return c
}

// drive sends a message and drives the session once with a runner of
// its own, as a runner that claims it does.
func (c *cloud) drive(text string, replies ...luxstub.Reply) {
	c.t.Helper()
	ctx := c.t.Context()
	s, err := c.st.Get(ctx, c.s.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	e, err := session.NewEvent(session.TypeUserMessage, session.UserMessage{Sender: s.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: text}}}, time.Now())
	if err != nil {
		c.t.Fatal(err)
	}
	c.driveAfter(session.StopEndTurn, []session.Event{e}, replies...)
}

// driveAfter appends evs as a client does and drives the session once
// with a runner of its own, which must leave it idle for want.
func (c *cloud) driveAfter(want session.StopReason, evs []session.Event, replies ...luxstub.Reply) {
	c.t.Helper()
	ctx := c.t.Context()
	c.lux.Script(haiku, replies...)
	s, err := c.st.Get(ctx, c.s.ID)
	if err != nil {
		c.t.Fatal(err)
	}
	session.Stamp(s.ID, s.LastSeq, evs)
	if _, err := c.st.Append(ctx, s.ID, s.LastSeq, evs); err != nil {
		c.t.Fatal(err)
	}
	h, err := Harness(c.o)
	if err != nil {
		c.t.Fatal(err)
	}
	o := runner.Options{Store: c.st, Harness: h, ID: "run_" + session.NewID("x"), Kind: runner.KindServe, CheckpointHost: c.checkpointHost}
	if c.creds != nil {
		o.Credentials = func(string, session.Lease) runner.Credentials { return c.creds }
	}
	r, err := runner.New(o)
	if err != nil {
		c.t.Fatal(err)
	}
	out, err := r.Drive(ctx, s.ID)
	if err != nil || out.StopReason != want {
		c.t.Fatalf("drive %+v, %v, want %s\nevents %s", out, err, want, c.dump())
	}
}

func (c *cloud) events(typ session.Type) []session.Event {
	c.t.Helper()
	evs, err := c.st.Events(c.t.Context(), c.s.ID, 1, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	var out []session.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (c *cloud) dump() string {
	evs, err := c.st.Events(c.t.Context(), c.s.ID, 1, 0)
	if err != nil {
		return err.Error()
	}
	b, err := json.Marshal(evs)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// results are the texts of the session's tool results, failing on one
// that is an error.
func (c *cloud) results() []string {
	c.t.Helper()
	var out []string
	for _, e := range c.events(session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil || p.IsError {
			c.t.Fatalf("a call failed: %+v %v\nevents %s", p, err, c.dump())
		}
		out = append(out, p.Content[0].Text)
	}
	return out
}

func said(text string) luxstub.Reply {
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: []ir.Block{{Type: ir.BlockText, Text: text}}, StopReason: ir.StopEndTurn}}
}

func bash(id, command string) luxstub.Reply {
	args, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		panic(err)
	}
	return luxstub.Reply{Response: ir.Response{Model: haiku, Blocks: []ir.Block{{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: "bash", Args: args}}}, StopReason: ir.StopToolUse}}
}

// TestASessionThatOnlyTalksCreatesNoSandbox: a hosted session of an agent
// whose tools act on a machine, whose turns call no such tool, creates no
// sandbox, starts none and records no machine (spec 048).
func TestASessionThatOnlyTalksCreatesNoSandbox(t *testing.T) {
	c := newCloud(t, nil)
	c.drive("Hello.", said("Hello."))
	c.drive("How are you?", said("Well."))
	if n, m := c.cella.Count(cellastub.OpCreate), c.cella.Count(cellastub.OpStart); n != 0 || m != 0 {
		t.Fatalf("%d sandboxes created and %d started", n, m)
	}
	if n := len(c.events(session.TypeSessionMachine)); n != 0 {
		t.Fatalf("%d session.machine events", n)
	}
}

// TestTheFirstBashCreatesTheSandboxAndLaterCallsReuseIt: the first bash
// call creates the session's sandbox and records it; the turn's later
// calls and the next turn's run in the same sandbox without another.
func TestTheFirstBashCreatesTheSandboxAndLaterCallsReuseIt(t *testing.T) {
	c := newCloud(t, nil)
	c.drive("Make a file.", bash("toolu_1", "echo one > f.txt && pwd"), bash("toolu_2", "cat f.txt"), said("Made."))
	if n := c.cella.Count(cellastub.OpCreate); n != 1 {
		t.Fatalf("%d sandboxes created", n)
	}
	machines := c.events(session.TypeSessionMachine)
	var p session.SessionMachine
	if len(machines) != 1 || machines[0].Decode(&p) != nil || p.Machine.Kind != machine.KindCella || p.Reason != "attached" {
		t.Fatalf("session.machine %+v", machines)
	}
	workspace := c.cella.Workspace(cella.SandboxName(c.s.ID))
	if got := c.results(); !strings.Contains(got[0], workspace) || !strings.Contains(got[1], "one") {
		t.Fatalf("results %q, workspace %s", got, workspace)
	}
	// The record lands beside the turn, after the call that opened the
	// machine and before its result.
	uses, results := c.events(session.TypeAgentToolUse), c.events(session.TypeToolResult)
	if uses[0].Seq >= machines[0].Seq || machines[0].Seq >= results[0].Seq {
		t.Fatalf("session.machine at %d, the call at %d, its result at %d", machines[0].Seq, uses[0].Seq, results[0].Seq)
	}
	c.drive("Again.", bash("toolu_3", "cat f.txt"), said("Still there."))
	if n := c.cella.Count(cellastub.OpCreate); n != 1 {
		t.Fatalf("%d sandboxes created over two turns", n)
	}
	if got := c.results(); !strings.Contains(got[2], "one") {
		t.Fatalf("the next turn's call ran elsewhere: %q", got)
	}
	if n := len(c.events(session.TypeSessionMachine)); n != 1 {
		t.Fatalf("%d session.machine events", n)
	}
}

// TestARestartedRunnerReattachesTheSandboxByName: a runner that claims a
// session whose sandbox exists, and which Cella stopped meanwhile, finds
// it by name and starts it, and runs the next call in it without
// creating another.
func TestARestartedRunnerReattachesTheSandboxByName(t *testing.T) {
	c := newCloud(t, nil)
	c.drive("Make a file.", bash("toolu_1", "echo kept > f.txt"), said("Made."))
	if !c.cella.Stop(cella.SandboxName(c.s.ID)) {
		t.Fatal("the session has no sandbox to stop")
	}
	c.drive("Read it.", bash("toolu_2", "cat f.txt"), said("Read."))
	if n := c.cella.Count(cellastub.OpCreate); n != 1 {
		t.Fatalf("%d sandboxes created, want the first one found again", n)
	}
	if n := c.cella.Count(cellastub.OpStart); n < 1 {
		t.Fatal("the stopped sandbox was not started")
	}
	if got := c.results(); !strings.Contains(got[1], "kept") {
		t.Fatalf("results %q", got)
	}
	if n := len(c.events(session.TypeSessionMachine)); n != 1 {
		t.Fatalf("%d session.machine events", n)
	}
}

// TestATalkOnlyTurnLeavesAStoppedSandboxStopped: a turn that only talks,
// in a session whose sandbox Cella stopped, starts nothing and completes
// on the context the session recorded; the next turn that calls a tool
// on the machine starts the sandbox once and runs in it (spec 048).
func TestATalkOnlyTurnLeavesAStoppedSandboxStopped(t *testing.T) {
	c := newCloud(t, nil)
	c.drive("Make a file.", bash("toolu_1", "echo kept > f.txt"), said("Made."))
	if !c.cella.Stop(cella.SandboxName(c.s.ID)) {
		t.Fatal("the session has no sandbox to stop")
	}
	c.drive("Thanks.", said("You are welcome."))
	if n := c.cella.Count(cellastub.OpStart); n != 0 {
		t.Fatalf("a turn that only talks started the sandbox %d times", n)
	}
	reqs := c.lux.Requests()
	var system []string
	for _, b := range reqs[len(reqs)-1].Request.System {
		system = append(system, b.Text)
	}
	if !strings.Contains(strings.Join(system, "\n"), "Cella sandbox") {
		t.Fatalf("the talking turn's request lacks the recorded machine context: %q", system)
	}
	c.drive("Read it.", bash("toolu_2", "cat f.txt"), bash("toolu_3", "cat f.txt"), said("Read."))
	if n, m := c.cella.Count(cellastub.OpStart), c.cella.Count(cellastub.OpCreate); n != 1 || m != 1 {
		t.Fatalf("the tool turn started the sandbox %d times and created %d, want one start and the first create", n, m)
	}
	if got := c.results(); !strings.Contains(got[1], "kept") || !strings.Contains(got[2], "kept") {
		t.Fatalf("the tool turn's calls ran elsewhere: %q", got)
	}
	if n := len(c.events(session.TypeSessionMachine)); n != 1 {
		t.Fatalf("%d session.machine events", n)
	}
}

// TestAStartRefusedForTheRunningCeilingIsTheCallsResult: a tool call on a
// session whose stopped sandbox Cella refuses to start, because its owner
// runs as many sandboxes as the ceiling allows, is answered with Cella's
// sentence, which the model reads in the call's result; the drive
// completes and the session waits for its next message (spec 048).
func TestAStartRefusedForTheRunningCeilingIsTheCallsResult(t *testing.T) {
	const sentence = "You have reached your limit of running sandboxes. Stop one to start another."
	c := newCloud(t, nil)
	c.drive("Make a file.", bash("toolu_1", "echo kept > f.txt"), said("Made."))
	if !c.cella.Stop(cella.SandboxName(c.s.ID)) {
		t.Fatal("the session has no sandbox to stop")
	}
	c.cella.Fail(cellastub.OpStart, cellastub.Failure{Status: 422, Code: "quota_exceeded", Detail: "starting it would make 5 running sandboxes of a ceiling of 4"})
	c.drive("Read it.", bash("toolu_2", "cat f.txt"), said("Stop another sandbox and ask again."))
	var res session.ToolResult
	results := c.events(session.TypeToolResult)
	if len(results) != 2 || results[1].Decode(&res) != nil || !res.IsError || !strings.Contains(res.Content[0].Text, sentence) {
		t.Fatalf("the refused call's result: %+v", res)
	}
	reqs := c.lux.Requests()
	if body := string(reqs[len(reqs)-1].Body); !strings.Contains(body, sentence) {
		t.Fatalf("the model was not told the refusal: %s", body)
	}
	var se session.SessionError
	if errs := c.events(session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != machine.CodeUnavailable {
		t.Fatalf("session.error %+v", errs)
	}
	s, err := c.st.Get(t.Context(), c.s.ID)
	if err != nil || s.Status != session.StatusIdle {
		t.Fatalf("the session after the refused start: %+v %v", s.Status, err)
	}
}

// TestTheSandboxReachesItsRepositoriesHosts: a session's repositories'
// git host is on its sandbox's egress allowlist, beside the agent's own
// hosts, and one that cannot be cloned is reported as
// repository_unavailable while the sandbox stays.
func TestTheSandboxReachesItsRepositoriesHosts(t *testing.T) {
	c := newCloud(t, func(s *session.Session) {
		s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: "https://127.0.0.1:1/org/app.git"}}
	})
	c.drive("Look.", bash("toolu_1", "ls"), said("No repository."))
	sb, ok := c.cella.Sandbox(cella.SandboxName(c.s.ID))
	if !ok || !slices.Contains(sb.Spec.Network.Egress.AllowedHosts, "127.0.0.1") {
		t.Fatalf("the allowlist %+v lacks the repository's host", sb.Spec.Network.Egress)
	}
	var se session.SessionError
	if errs := c.events(session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != runner.CodeRepositoryUnavailable {
		t.Fatalf("session.error %+v", errs)
	}
}

// gitHost serves bare repositories under root over git's own http
// backend, admitting only requests whose Authorization header is what
// want answers, and records every Authorization it saw.
type gitHost struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newGitHost(t *testing.T, root string, want func() string) *gitHost {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	g := &gitHost{}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		g.mu.Lock()
		g.seen = append(g.seen, auth)
		g.mu.Unlock()
		if auth != want() {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.com", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestASessionClonesCommitsAndPushesThroughItsSandbox: a session naming a
// repository on the installation's git host gets it cloned into its
// sandbox's working directory at the first bash call, on its own branch;
// git in the sandbox sends the git host the placeholder of the git host's
// Secret the sandbox's manifest names, never a credential; the sandbox's
// session.machine names the repository, its branch and the commit the
// branch started at; and a commit made by bash carries the session's
// trailers and author and pushes to the git host.
func TestASessionClonesCommitsAndPushesThroughItsSandbox(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "app.git")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitIn(t, bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "--quiet", "-m", "Start")
	gitIn(t, seed, "push", "--quiet", "origin", "HEAD:main")
	start := gitIn(t, bare, "rev-parse", "refs/heads/main")

	var name string
	host := newGitHost(t, root, func() string { return "Bearer cella-placeholder-" + name + "-origo" })
	u, err := url.Parse(host.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c := newCloud(t, func(s *session.Session) {
		s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: host.srv.URL + "/app.git"}}
	})
	name = cella.SandboxName(c.s.ID)
	// The installation mints the session's token for its git host, and no
	// Lux key for the sandbox, since it names no model URL for it.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir, OrigoURL: host.srv.URL})
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	commit := `echo change >> README.md && git commit -q -am "Change the readme" && git push -q && git log -1 --format=%B`
	c.drive("Change the readme and push.", bash("toolu_1", commit), said("Pushed."))
	got := c.results()
	branch := session.Branch(c.s)
	for _, want := range []string{"Change the readme", runner.TrailerSession + ": " + c.s.ID, runner.TrailerAgent + ": " + c.s.Agent.ID + "@1"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("the commit lacks %q: %q", want, got[0])
		}
	}
	msg := gitIn(t, bare, "log", "-1", "--format=%an <%ae>%n%B", "refs/heads/"+branch)
	if !strings.Contains(msg, "builder <builder@agents.topos.invalid>") || !strings.Contains(msg, runner.TrailerSession+": "+c.s.ID) {
		t.Fatalf("the git host's branch %s holds:\n%s", branch, msg)
	}
	host.mu.Lock()
	seen := slices.Clone(host.seen)
	host.mu.Unlock()
	if len(seen) == 0 || slices.ContainsFunc(seen, func(a string) bool { return a != "Bearer cella-placeholder-"+name+"-origo" }) {
		t.Fatalf("the git host saw %q, want the placeholder alone", seen)
	}
	sec, value, ok := c.cella.Secret(name + "-origo")
	if !ok || !strings.HasPrefix(value, "origo-sandbox-") || !slices.Equal(sec.Spec.Scope.Hosts, []string{u.Hostname()}) {
		t.Fatalf("the git host's secret %+v %q %v", sec.Spec, value, ok)
	}
	sb, ok := c.cella.Sandbox(name)
	if !ok || !slices.Equal(sb.Spec.Secrets, []cellav1.SecretMount{{Name: name + "-origo", Env: EnvOrigoToken}}) || !slices.Contains(sb.Spec.Network.Egress.AllowedHosts, u.Hostname()) {
		t.Fatalf("the sandbox's manifest %+v", sb.Spec)
	}
	for _, e := range c.events(session.TypeToolResult) {
		if strings.Contains(string(e.Payload), value) {
			t.Fatal("a tool result carries the git host's token")
		}
	}
	// The sandbox's attachment names the delivered repository, its branch
	// and the commit the branch started at, the default branch's.
	machines := c.events(session.TypeSessionMachine)
	var attached session.SessionMachine
	if len(machines) != 1 || machines[0].Decode(&attached) != nil || attached.Reason != "attached" {
		t.Fatalf("session.machine %+v", machines)
	}
	want := []session.DeliveredRepository{{URL: host.srv.URL + "/app.git", Branch: branch, Commit: start}}
	if !slices.Equal(attached.Repositories, want) {
		t.Fatalf("the attachment names %+v, want %+v", attached.Repositories, want)
	}
}

// TestAHostedSessionKeepsItsCheckpointsAtTheGitHost: a hosted session
// that works in a private repository on the git host pushes each turn's
// checkpoint there from its sandbox, sending the git host the
// placeholder of its Secret, and nothing at all on the read that finds
// the repository private; once the sandbox is gone,
// a fork of the session restores the file at its first call, in a
// sandbox of its own, and a fork whose checkpoint the git host lost runs
// that call on its repository with checkpoint_missing beside it.
func TestAHostedSessionKeepsItsCheckpointsAtTheGitHost(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "app.git")
	gitIn(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitIn(t, bare, "config", "http.receivepack", "true")
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "--quiet", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "--quiet", "-m", "Start")
	gitIn(t, seed, "push", "--quiet", "origin", "HEAD:main")

	var mu sync.Mutex
	sandboxes := map[string]bool{}
	host := &gitHost{}
	host.srv = httptest.NewServer(placeholders(t, host, root, func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return sandboxes[name]
	}))
	t.Cleanup(host.srv.Close)
	repo := host.srv.URL + "/app.git"
	c := newCloud(t, func(s *session.Session) {
		s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: repo}}
	})
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.o.Machines = Cella(CellaOptions{URL: c.cella.URL(), Token: client.StaticToken("installation-bearer"), Helpers: helpers(t), Dir: dir, OrigoURL: host.srv.URL})
	c.creds = &issued{life: 15 * time.Minute, err: map[string]error{runner.AudienceLux: runner.ErrNotMinted}}
	c.checkpointHost = host.srv.URL
	parent := c.s
	mu.Lock()
	sandboxes[cella.SandboxName(parent.ID)] = true
	mu.Unlock()
	c.drive("Write a draft.", bash("toolu_1", "echo draft > notes.txt"), said("Drafted."))
	var status session.SessionStatus
	statuses := c.events(session.TypeSessionStatus)
	if err := statuses[len(statuses)-1].Decode(&status); err != nil || status.Checkpoint == nil || status.Checkpoint.Remote != repo {
		t.Fatalf("the turn's checkpoint %+v, %v; want it kept at %s", status.Checkpoint, err, repo)
	}
	if got := gitIn(t, bare, "rev-parse", "refs/topos/checkpoints/"+parent.ID+"/latest"); got != status.Checkpoint.Commit {
		t.Fatalf("the git host's latest is %s, want %s", got, status.Checkpoint.Commit)
	}
	if files := gitIn(t, bare, "ls-tree", "-r", "--name-only", status.Checkpoint.Commit); !strings.Contains(files, "notes.txt") {
		t.Fatalf("the kept checkpoint holds %q", files)
	}
	host.mu.Lock()
	seen := slices.Clone(host.seen)
	host.mu.Unlock()
	// Every request carries the sandbox's placeholder but the one that
	// asks, with no credential, whether the repository is private, which
	// the git host refused, so the checkpoint went there.
	placeholder := "Bearer cella-placeholder-" + cella.SandboxName(parent.ID) + "-origo"
	if len(seen) == 0 || slices.ContainsFunc(seen, func(a string) bool { return a != placeholder && a != "" }) || !slices.Contains(seen, "") {
		t.Fatalf("the git host saw %q, want the sandbox's placeholder and one request with none", seen)
	}
	if !c.cella.Remove(cella.SandboxName(parent.ID)) {
		t.Fatal("the parent has no sandbox to delete")
	}
	end, err := session.NewEvent(session.TypeSessionStatus, session.SessionStatus{Status: session.StatusEnded, StopReason: session.StopCompleted}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ended, err := c.st.Get(t.Context(), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{end}
	session.Stamp(parent.ID, ended.LastSeq, evs)
	if _, err := c.st.Append(t.Context(), parent.ID, ended.LastSeq, evs); err != nil {
		t.Fatal(err)
	}

	fork := func() session.Session {
		evs, err := c.st.Events(t.Context(), parent.ID, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := session.ForkPoint(evs, nil)
		if err != nil {
			t.Fatal(err)
		}
		child := session.New(parent.Agent, parent.Initiator, session.RunnerHosted, parent.Machine, time.Now())
		child.Resources = parent.Resources
		if child, err = session.Fork(t.Context(), c.st, child, c.blobs, parent, evs[:seq]); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		sandboxes[cella.SandboxName(child.ID)] = true
		mu.Unlock()
		c.s = child
		return child
	}
	child := fork()
	c.drive("Read the draft.", bash("toolu_2", "cat notes.txt"), said("Read."))
	// The fork's log starts with its parent's call; its own is the last.
	if got := c.results(); len(got) != 2 || !strings.Contains(got[1], "draft") {
		t.Fatalf("the fork's call read %q, want the parent's draft", got)
	}
	var m session.SessionMachine
	if ms := c.events(session.TypeSessionMachine); len(ms) == 0 || ms[len(ms)-1].Decode(&m) != nil || m.Reason != "restored" || m.Checkpoint == nil || m.Checkpoint.Commit != status.Checkpoint.Commit {
		t.Fatalf("the fork's machine %+v, want restored at %s", m, status.Checkpoint.Commit)
	}
	if n := len(c.events(session.TypeSessionError)); n != 0 {
		t.Fatalf("%d session errors\n%s", n, c.dump())
	}
	// The file is in the fork's own sandbox, where its call ran.
	if b, err := os.ReadFile(filepath.Join(c.cella.Workspace(cella.SandboxName(child.ID)), "notes.txt")); err != nil || string(b) != "draft\n" {
		t.Fatalf("the fork's sandbox holds notes.txt %q, %v", b, err)
	}
	if !c.cella.Remove(cella.SandboxName(child.ID)) {
		t.Fatal("the fork has no sandbox")
	}

	// The git host loses the parent's checkpoint.
	gitIn(t, bare, "update-ref", "-d", "refs/topos/checkpoints/"+parent.ID+"/latest")
	gitIn(t, bare, "update-ref", "-d", "refs/topos/checkpoints/"+child.ID+"/latest")
	gitIn(t, bare, "-c", "gc.reflogExpireUnreachable=now", "gc", "--quiet", "--prune=now")
	fork()
	c.drive("Read the draft.", bash("toolu_3", "cat README.md && test ! -e notes.txt && echo no-notes"), said("No draft."))
	if got := c.results(); len(got) != 2 || !strings.Contains(got[1], "app") || !strings.Contains(got[1], "no-notes") {
		t.Fatalf("the fork's call answered %q, want its repository and no draft", got)
	}
	var missing session.SessionError
	errs := c.events(session.TypeSessionError)
	if len(errs) != 1 || errs[0].Decode(&missing) != nil || missing.Code != "checkpoint_missing" || !strings.Contains(missing.Detail, status.Checkpoint.Commit) {
		t.Fatalf("session errors %s, want one checkpoint_missing", c.dump())
	}
	var again session.SessionMachine
	if ms := c.events(session.TypeSessionMachine); len(ms) == 0 || ms[len(ms)-1].Decode(&again) != nil || again.Reason != "attached" || again.Checkpoint != nil {
		t.Fatalf("the fork's machine %+v, want attached", again)
	}
}

// placeholders serves the git host's repositories under root to a
// request that carries the placeholder of the git host's Secret of a
// sandbox ok names, and records every Authorization it saw on host.
func placeholders(t *testing.T, host *gitHost, root string, ok func(name string) bool) http.Handler {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	backend := &cgi.Handler{Path: git, Args: []string{"http-backend"}, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		host.mu.Lock()
		host.seen = append(host.seen, auth)
		host.mu.Unlock()
		name, found := strings.CutPrefix(auth, "Bearer cella-placeholder-")
		name, suffixed := strings.CutSuffix(name, "-origo")
		if !found || !suffixed || !ok(name) {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	})
}
