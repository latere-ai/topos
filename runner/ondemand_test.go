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
	base := filepath.Dir(f.work)
	r, err := New(Options{
		Store: f.store, ID: "run_local", Clock: func() time.Time { return t0 },
		Harness: func(ctx context.Context, s session.Session) (harness.Config, error) {
			reg := tools.NewRegistry()
			if err := reg.AddBuiltin(echo{}); err != nil {
				return harness.Config{}, err
			}
			for _, t := range tools.Builtins() {
				if t.Definition().Name == tools.NameBash {
					if err := reg.AddBuiltin(t); err != nil {
						return harness.Config{}, err
					}
				}
			}
			m := machine.Defer(ctx, machine.KindHost, func(context.Context) (machine.Machine, error) {
				opens.Add(1)
				return host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill", s.ID), Environ: []string{"PATH=" + os.Getenv("PATH")}})
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

// TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt: a turn that only
// talks opens no machine and records none; the first tool that acts on
// the machine opens it, its session.machine is appended beside the turn
// and the turn's next request carries the machine's context and
// instructions; a later drive of a session that has a machine opens it
// at once and records it no second time.
func TestAMachineOnDemandIsRecordedWhenAToolFirstActsOnIt(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemand(f.work, &opens)
	f.stub.Script(model, reply(ir.Block{Type: ir.BlockText, Text: "Hello."}))
	f.message(ctx, "Hi.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 0 || f.count(ctx, session.TypeSessionMachine) != 0 {
		t.Fatalf("a turn that only talks opened %d machines", opens.Load())
	}
	if strings.Contains(f.systemText(0), "Working directory") {
		t.Fatal("a session with no machine was told of one")
	}
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
// session that never needs a machine ends with none opened.
func TestAnEndOnIdleSessionThatOnlyTalksHasNoMachine(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	var opens atomic.Int32
	f.onDemand(f.work, &opens)
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

// TestTheFirstMachineGetsTheSessionsRepositories: at the first machine
// the runner clones the session's first repository into the working
// directory on the session's branch from the ref it names and a second
// one beside it, and a commit made by a bash call carries the session's
// author and trailers and pushes to the git host.
func TestTheFirstMachineGetsTheSessionsRepositories(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	app, lib := bareRepo(t, "app"), bareRepo(t, "lib")
	work := filepath.Join(filepath.Dir(f.work), "fresh")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	var opens atomic.Int32
	f.onDemand(work, &opens)
	s := session.New(session.AgentRef{ID: session.NewID(session.PrefixAgent), Name: "builder", Version: 3}, f.s.Initiator, session.RunnerHosted, session.Machine{Kind: machine.KindCella}, t0)
	s.Resources = []session.Resource{{Type: ResourceRepository, URL: "file://" + app, Ref: "dev"}, {Type: ResourceRepository, URL: "file://" + lib}}
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
	branch := SessionBranch(s)
	if got := gitRun(t, work, "rev-parse", "--abbrev-ref", "HEAD"); got != branch {
		t.Fatalf("the working directory is on %q, want %q", got, branch)
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
	if after := f.systemText(1); !strings.Contains(after, "Repository rules for app.") || !strings.Contains(after, "Git: branch "+branch) {
		t.Fatalf("the request after the delivery lacks the repository's context:\n%s", after)
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
		{Type: ResourceRepository, URL: "file://" + filepath.Join(work, "..", "missing.git")},
		{Type: ResourceRepository, URL: "file://" + empty},
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
	want := []session.DeliveredRepository{{URL: "file://" + empty, Branch: SessionBranch(s)}}
	if err := f.events(ctx, session.TypeSessionMachine)[0].Decode(&attached); err != nil || !slices.Equal(attached.Repositories, want) {
		t.Fatalf("the attachment names %+v, %v; want %+v", attached.Repositories, err, want)
	}
}

func TestRepoDir(t *testing.T) {
	taken := map[string]bool{}
	for _, c := range []struct{ url, want string }{
		{"https://code.example/org/lib.git", "lib"},
		{"https://code.example/other/lib", "lib-2"},
		{"https://code.example/", "repository-3"},
		{"file:///srv/.hidden.git", "repository-4"},
	} {
		if got := repoDir(c.url, len(taken)+1, taken); got != c.want {
			t.Errorf("%s: %q, want %q", c.url, got, c.want)
		}
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
