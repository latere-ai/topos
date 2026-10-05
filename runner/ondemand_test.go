// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/models/dialect"
	"latere.ai/x/topos/session"
)

// onDemand makes the fixture's runner open its machine, a host machine
// in work, on demand, and counts the opens. The agent runs echo and bash,
// and bash needs no confirmation.
func (f *fixture) onDemand(work string, opens *atomic.Int32) {
	f.t.Helper()
	f.onDemandWith(work, opens, demand{})
}

// demand varies the agent and the machine of onDemand.
type demand struct {
	// noTools is an agent with no tools, none of which acts on a machine.
	noTools bool
	// opening runs at each open, before the machine opens, as the time a
	// sandbox takes to come up, and opened after it opened.
	opening, opened func()
}

func (f *fixture) onDemandWith(work string, opens *atomic.Int32, d demand) {
	f.t.Helper()
	base := filepath.Dir(f.work)
	r, err := New(Options{
		Store: f.store, ID: "run_local", Clock: func() time.Time { return t0 },
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			reg := tools.NewRegistry()
			for _, t := range append([]tools.Tool{echo{}}, tools.Builtins()...) {
				if n := t.Definition().Name; !d.noTools && (n == "echo" || n == tools.NameBash) {
					if err := reg.AddBuiltin(t); err != nil {
						return harness.Config{}, err
					}
				}
			}
			m := machine.Defer(ctx, machine.KindHost, func(context.Context) (machine.Machine, error) {
				opens.Add(1)
				if d.opening != nil {
					d.opening()
				}
				m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill", s.ID), Environ: []string{"PATH=" + os.Getenv("PATH")}})
				if d.opened != nil {
					d.opened()
				}
				return m, err
			})
			return harness.Config{
				Model: &dialect.Model{}, Connection: models.Connection{BaseURL: f.stub.URL() + "/anthropic", Model: model, Family: models.FamilyAnthropic},
				Entry: models.Entry{InputWindow: 100_000, MaxOutputTokens: 8_000}, Machine: m, Tools: reg,
				Policy: harness.Policy{AlwaysAllow: []string{"bash"}},
				Clock:  func() time.Time { return t0 }, Sleep: func(context.Context, time.Duration) error { return nil },
			}, nil
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.r = r
}

// systemText is the system prompt of the stub's request n.
func (f *fixture) systemText(n int) string {
	f.t.Helper()
	reqs := f.stub.Requests()
	if n >= len(reqs) {
		f.t.Fatalf("request %d of %d", n, len(reqs))
	}
	var texts []string
	for _, b := range reqs[n].Request.System {
		texts = append(texts, b.Text)
	}
	return strings.Join(texts, "\n")
}

func toolUse(id, name, args string) ir.Block {
	return ir.Block{Type: ir.BlockToolUse, ToolUse: &ir.ToolUse{ID: id, Name: name, Args: json.RawMessage(args)}}
}

// TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt: a turn of an
// agent with no tool that acts on a machine opens no machine and records
// none; once the agent has one, the turn starts the machine, the first
// tool that acts on it runs on it, its session.machine is appended beside
// the turn and the turn's next request carries the machine's context and
// instructions; a later drive of a session that has a machine opens it
// at once and records it no second time.
func TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{noTools: true})
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Hello."}))
	f.message(ctx, "Hi.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || f.count(ctx, session.TypeSessionMachine) != 0 {
		t.Fatalf("a turn of an agent with no machine tools opened %d machines", opens.Load())
	}
	if first := f.systemText(0); strings.Contains(first, "Working directory") || strings.Contains(first, "Repositories") {
		t.Fatalf("a session with no machine and no repositories was told of one:\n%s", first)
	}
	f.onDemand(f.work, &opens)
	f.stub.Script(model,
		reply(toolUse("toolu_1", "echo", `{}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Done."}),
	)
	f.message(ctx, "Echo.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 || f.count(ctx, session.TypeSessionMachine) != 1 {
		t.Fatalf("%d opens, %d session.machine", opens.Load(), f.count(ctx, session.TypeSessionMachine))
	}
	if strings.Contains(f.systemText(1), "Working directory") {
		t.Fatal("the request before the tool call carried a machine")
	}
	if after := f.systemText(2); !strings.Contains(after, "Working directory: "+f.work) || !strings.Contains(after, "Run make check") {
		t.Fatalf("the request after the machine opened lacks its context:\n%s", after)
	}
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Again."}))
	f.message(ctx, "Once more.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 2 || f.count(ctx, session.TypeSessionMachine) != 1 {
		t.Fatalf("the session's machine was not reopened at once, or was recorded again: %d opens, %d session.machine", opens.Load(), f.count(ctx, session.TypeSessionMachine))
	}
}

// TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine: an end_on_idle
// session of an agent with no tool that acts on a machine ends with none
// opened.
func TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{noTools: true})
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	s.EndOnIdle = true
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Finished."}))
	f.message(ctx, "One shot.")
	if out, err := f.r.Drive(ctx, s.ID); err != nil || out.Status != session.StatusEnded {
		t.Fatalf("drive %+v, %v", out, err)
	}
	if opens.Load() != 0 {
		t.Fatalf("%d machines opened", opens.Load())
	}
}

// gitRun runs git in dir for a test.
func gitRun(t *testing.T, dir string, args ...string) string {
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

// bareRepo makes a bare repository standing in for a git host's, with a
// main branch that holds AGENTS.md and a dev branch one commit ahead,
// and returns its path.
func bareRepo(t *testing.T, name string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(base, name+".git")
	gitRun(t, base, "init", "--quiet", "--bare", "-b", "main", bare)
	seed := filepath.Join(base, "seed")
	gitRun(t, base, "clone", "--quiet", bare, seed)
	write(t, filepath.Join(seed, "AGENTS.md"), "Repository rules for "+name+".\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "--quiet", "-m", "Start")
	gitRun(t, seed, "push", "--quiet", "origin", "HEAD:main")
	gitRun(t, seed, "checkout", "--quiet", "-b", "dev")
	write(t, filepath.Join(seed, "dev.txt"), "dev\n")
	gitRun(t, seed, "add", ".")
	gitRun(t, seed, "commit", "--quiet", "-m", "Dev")
	gitRun(t, seed, "push", "--quiet", "origin", "HEAD:dev")
	return bare
}

// TestTheFirstMachineGetsTheSessionsRepositories: the first request names
// the session's repositories before any machine opens; at the first
// machine the runner clones the session's first repository into the
// working directory on the session's branch from the ref it names and a
// second one beside it, and a commit made by a bash call carries the
// session's author and trailers and pushes to the git host.
func TestTheFirstMachineGetsTheSessionsRepositories(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	app, lib := bareRepo(t, "app"), bareRepo(t, "lib")
	work := filepath.Join(filepath.Dir(f.work), "fresh")
	// A sandbox's workspace volume is not empty when it opens: it holds
	// lost+found, which a git clone into the directory refuses.
	if err := os.MkdirAll(filepath.Join(work, "lost+found"), 0o700); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	f.onDemand(work, &opens)
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 3}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	s.Resources = []session.Resource{{Type: session.ResourceRepository, URL: "file://" + app, Ref: "dev"}, {Type: session.ResourceRepository, URL: "file://" + lib}}
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	commit := `echo change >> dev.txt && git commit -q -am "Change dev" && git push -q`
	f.stub.Script(model,
		reply(toolUse("toolu_1", "bash", `{"command":`+jsonString(commit)+`}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "Pushed."}),
	)
	f.message(ctx, "Change dev.txt and push.")
	if _, err := f.r.Drive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	var res session.ToolResult
	for _, e := range f.events(ctx, session.TypeToolResult) {
		if err := e.Decode(&res); err != nil || res.IsError {
			t.Fatalf("the bash call failed: %+v %v", res, err)
		}
	}
	branch := session.Branch(s)
	if got := gitRun(t, work, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the working directory is on %q, want %q", got, branch)
	}
	if got := gitRun(t, work, "status", "--porcelain", "--untracked-files=all"); strings.Contains(got, "lost+found") {
		t.Fatalf("the working directory shows %q; lost+found is not git's", got)
	}
	if got := gitRun(t, filepath.Join(work, "lib"), "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the second repository is on %q", got)
	}
	msg := gitRun(t, app, "log", "-1", "--format=%an <%ae>%n%B", "refs/heads/"+branch)
	for _, want := range []string{"builder <builder@agents.topos.invalid>", "Change dev", "Topos-Session: " + s.ID, "Topos-Agent: " + s.Agent.ID + "@3"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the pushed commit lacks %q:\n%s", want, msg)
		}
	}
	if parent := gitRun(t, app, "rev-parse", "refs/heads/"+branch+"^"); parent != gitRun(t, app, "rev-parse", "refs/heads/dev") {
		t.Fatal("the session's branch did not start at its ref")
	}
	// The first request names the repositories the machine will get,
	// though it has not opened; the request after the delivery carries the
	// machine's own context in that block's place.
	first := f.systemText(0)
	for _, want := range []string{
		"Repositories, cloned the first time a file or command tool runs:",
		"- file://" + app + " at dev, on branch " + branch + ", into the working directory",
		"- file://" + lib + ", on branch " + branch + ", into lib/ in the working directory",
	} {
		if !strings.Contains(first, want) {
			t.Fatalf("the first request lacks %q:\n%s", want, first)
		}
	}
	if strings.Contains(first, "Working directory") {
		t.Fatalf("the first request names a machine that has not opened:\n%s", first)
	}
	if after := f.systemText(1); !strings.Contains(after, "Repository rules for app.") || !strings.Contains(after, "Git: branch "+branch) || strings.Contains(after, "Repositories, cloned") {
		t.Fatalf("the request after the delivery lacks the repository's context, or still names the repositories to clone:\n%s", after)
	}
	// The attachment names each delivered repository with the commit its
	// branch started at: the ref's for the first, the default branch's
	// for the second.
	machines := f.events(ctx, session.TypeSessionMachine)
	var attached session.SessionMachine
	if len(machines) != 1 || machines[0].Decode(&attached) != nil || attached.Reason != "attached" {
		t.Fatalf("session.machine %+v", machines)
	}
	want := []session.DeliveredRepository{
		{URL: "file://" + app, Branch: branch, Commit: gitRun(t, app, "rev-parse", "refs/heads/dev")},
		{URL: "file://" + lib, Branch: branch, Commit: gitRun(t, lib, "rev-parse", "refs/heads/main")},
	}
	if !slices.Equal(attached.Repositories, want) {
		t.Fatalf("the attachment names %+v, want %+v", attached.Repositories, want)
	}
}

// TestARepositoryThatCannotBeDeliveredIsReported: a repository the runner
// cannot clone answers the call that opened the machine with
// repository_unavailable, the machine stays and is recorded with the
// repositories that were delivered, an empty one with no commit, and the
// next call runs on it.
func TestARepositoryThatCannotBeDeliveredIsReported(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	work := filepath.Join(filepath.Dir(f.work), "fresh")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	f.onDemand(work, &opens)
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 1}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	empty := filepath.Join(t.TempDir(), "empty.git")
	gitRun(t, filepath.Dir(empty), "init", "--quiet", "--bare", "-b", "main", empty)
	s.Resources = []session.Resource{
		{Type: session.ResourceRepository, URL: "file://" + filepath.Join(work, "..", "missing.git")},
		{Type: session.ResourceRepository, URL: "file://" + empty},
	}
	if err := f.store.Create(ctx, s, nil); err != nil {
		t.Fatal(err)
	}
	f.s = s
	f.stub.Script(model,
		reply(toolUse("toolu_1", "bash", `{"command":"ls"}`)),
		reply(toolUse("toolu_2", "bash", `{"command":"echo still here"}`)),
		reply(ir.Block{Type: ir.BlockText, Text: "No repository."}),
	)
	f.message(ctx, "Look.")
	if _, err := f.r.Drive(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	results := f.events(ctx, session.TypeToolResult)
	var first, second session.ToolResult
	if err := results[0].Decode(&first); err != nil || !first.IsError || !strings.Contains(first.Content[0].Text, CodeRepositoryUnavailable) {
		t.Fatalf("the first call: %+v %v", first, err)
	}
	if err := results[1].Decode(&second); err != nil || second.IsError || !strings.Contains(second.Content[0].Text, "still here") {
		t.Fatalf("the second call: %+v %v", second, err)
	}
	var se session.SessionError
	if errs := f.events(ctx, session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&se) != nil || se.Code != CodeRepositoryUnavailable {
		t.Fatalf("session.error %+v", errs)
	}
	if opens.Load() != 1 || f.count(ctx, session.TypeSessionMachine) != 1 {
		t.Fatalf("%d opens, %d session.machine", opens.Load(), f.count(ctx, session.TypeSessionMachine))
	}
	var attached session.SessionMachine
	want := []session.DeliveredRepository{{URL: "file://" + empty, Branch: session.Branch(s)}}
	if err := f.events(ctx, session.TypeSessionMachine)[0].Decode(&attached); err != nil || !slices.Equal(attached.Repositories, want) {
		t.Fatalf("the attachment names %+v, %v; want %+v", attached.Repositories, err, want)
	}
}

// events are the fixture session's events of a type.
func (f *fixture) events(ctx context.Context, typ session.Type) []session.Event {
	f.t.Helper()
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []session.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// jsonString is s as a JSON string.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestTheMachineStartsBesideTheFirstModelCall: the first turn of an
// agent whose tools act on the machine starts the machine as it begins,
// so the machine's start and the model's first answer overlap instead of
// adding up before the first tool call runs (spec 046). Both take took
// here: the turn ends after about one of them where it took both.
func TestTheMachineStartsBesideTheFirstModelCall(t *testing.T) {
	const took = time.Second
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	var mu sync.Mutex
	var openedAt, answeredAt time.Time
	f.onDemandWith(f.work, &opens, demand{opening: func() {
		mu.Lock()
		openedAt = time.Now()
		mu.Unlock()
		time.Sleep(took)
	}})
	first := reply(toolUse("toolu_1", "bash", `{"command":"echo ran"}`))
	first.Respond = func(*ir.Request, *ir.Response) {
		time.Sleep(took)
		mu.Lock()
		answeredAt = time.Now()
		mu.Unlock()
	}
	f.stub.Script(model, first, reply(ir.Block{Type: ir.BlockText, Text: "Done."}))
	f.message(ctx, "Run it.")
	start := time.Now()
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("the turn took %s with a machine and a first answer of %s each", elapsed.Round(time.Millisecond), took)
	mu.Lock()
	defer mu.Unlock()
	if openedAt.IsZero() || !openedAt.Before(answeredAt) {
		t.Fatalf("the machine opened at %v, after the first answer at %v", openedAt, answeredAt)
	}
	if elapsed >= took*8/5 {
		t.Fatalf("the turn took %s: the machine's start and the first answer added up", elapsed)
	}
	var res session.ToolResult
	results := f.events(ctx, session.TypeToolResult)
	if len(results) != 1 || results[0].Decode(&res) != nil || res.IsError || !strings.Contains(res.Content[0].Text, "ran") {
		t.Fatalf("the bash call: %+v", res)
	}
	if opens.Load() != 1 || f.count(ctx, session.TypeSessionMachine) != 1 {
		t.Fatalf("%d opens, %d session.machine", opens.Load(), f.count(ctx, session.TypeSessionMachine))
	}
}

// TestATurnThatOnlyTalksStartsTheMachineAndRecordsNone: a turn of an
// agent whose tools act on the machine starts it even when the model
// only talks, and the session records no machine, since no tool used it;
// the drive does not wait for the start to finish before it lets the
// session go.
func TestATurnThatOnlyTalksStartsTheMachineAndRecordsNone(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	slow, finished := make(chan struct{}), make(chan struct{})
	f.onDemandWith(f.work, &opens, demand{opening: func() { <-slow }, opened: func() { close(finished) }})
	// The open the drive left behind finishes before the test's
	// directories go.
	defer func() {
		close(slow)
		<-finished
	}()
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Hello."}))
	f.message(ctx, "Hi.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 || f.count(ctx, session.TypeSessionMachine) != 0 {
		t.Fatalf("%d opens, %d session.machine", opens.Load(), f.count(ctx, session.TypeSessionMachine))
	}
	s, err := f.store.Get(ctx, f.s.ID)
	if err != nil || s.Status != session.StatusIdle {
		t.Fatalf("the session after the turn: %+v %v", s.Status, err)
	}
}
