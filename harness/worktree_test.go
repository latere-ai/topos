// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/luxstub"
)

// writer writes the file its input names through the call's machine.
type writer struct{}

func (writer) Definition() tools.Definition {
	return tools.Definition{Name: "put", Description: "write a file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"text":{"type":"string"}}}`)}
}
func (writer) Properties() tools.Properties { return tools.Properties{Effect: tools.EffectWrite} }
func (writer) Run(ctx context.Context, c tools.Call) (tools.Result, error) {
	var in struct{ Path, Text string }
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return tools.Result{}, err
	}
	if err := c.Machine.WriteFile(ctx, in.Path, strings.NewReader(in.Text), 0o644); err != nil {
		return tools.Result{}, err
	}
	return tools.Text(tools.OutcomeOK, "wrote "+in.Path), nil
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=p", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=p", "GIT_COMMITTER_EMAIL=p@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAnIsolatedThreadWorksOnItsOwnBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "main.go")
	gitIn(t, work, "commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(work, "wip.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill"), WorktreeDir: filepath.Join(base, "worktrees"), Environ: []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	e := setup(t, func(c *Config) {
		withReviewer(func(s *Subagent) { s.Tools = []string{"put"} })(c)
		c.Machine = h
		c.Policy.Mode = ModeProgressive
	})
	if err := e.cfg.Tools.AddBuiltin(writer{}); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	e.stub.Script(model,
		reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"Add notes.txt.","isolation":"worktree"}`)),
		luxstub.Reply{Response: ir.Response{Model: model}, Respond: func(_ *ir.Request, r *ir.Response) {
			id, _ := e.thread(e.all())
			r.Blocks = []ir.Block{call("toolu_m", ToolMessage, `{"thread":"`+id+`","content":"Add more.txt too."}`)}
			r.StopReason = ir.StopToolUse
		}},
		reply(ir.StopEndTurn, text("I will merge both.")),
	)
	e.stub.Script(reviewerModel,
		reply(ir.StopToolUse, call("toolu_p", "put", `{"path":"notes.txt","text":"from the thread\n"}`)),
		reply(ir.StopEndTurn, text("Added notes.txt.")),
		reply(ir.StopToolUse, call("toolu_p2", "put", `{"path":"more.txt","text":"second turn\n"}`)),
		reply(ir.StopEndTurn, text("Added more.txt.")),
	)
	e.send(ctx, "Get notes written.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	id, started := e.thread(e.all())
	if started.Isolation != "worktree" || !strings.HasPrefix(started.Branch, "agents/reviewer/"+e.s.ID+".") || started.Workdir != filepath.Join(base, "worktrees", id) {
		t.Fatalf("thread.started %+v", started)
	}
	if _, err := os.Stat(filepath.Join(work, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("the thread wrote into the parent's working directory: %v", err)
	}
	files := gitIn(t, work, "ls-tree", "-r", "--name-only", started.Branch)
	if !strings.Contains(files, "notes.txt") || !strings.Contains(files, "more.txt") || strings.Contains(files, "wip.go") {
		t.Fatalf("the branch holds %q", files)
	}
	if n := gitIn(t, work, "rev-list", "--count", "main.."+started.Branch); n != "2" {
		t.Fatalf("%s commits on the branch, want one per turn", n)
	}
	if gitIn(t, work, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatal("the parent's checkout moved")
	}
	res := e.parentResults(t)
	if len(res) != 2 {
		t.Fatalf("results %+v", res)
	}
	var all []string
	for _, b := range res[0].Content {
		all = append(all, b.Text)
	}
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, "Its work is on branch "+started.Branch) || !strings.Contains(joined, "uncommitted changes are not in its worktree") {
		t.Fatalf("the spawn result %q", joined)
	}
	if !strings.Contains(res[1].Content[len(res[1].Content)-1].Text, started.Branch) {
		t.Fatalf("the message result %+v", res[1].Content)
	}
}

func TestWorktreeIsolationNeedsAMachineThatKeepsThem(t *testing.T) {
	e := setup(t, withReviewer(nil))
	ctx := t.Context()
	e.stub.Script(model, reply(ir.StopToolUse, spawnCall("toolu_s", `{"agent":"reviewer","task":"t","isolation":"worktree"}`)), reply(ir.StopEndTurn, text("ok")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.StopReason != session.StopEndTurn {
		t.Fatalf("outcome %+v", out)
	}
	if res := e.parentResults(t); len(res) != 1 || !strings.HasPrefix(res[0].Content[0].Text, CodeIsolationUnavailable) {
		t.Fatalf("results %+v", res)
	}
	if _, ok := any(fakeMachine{}).(machine.Worktrees); ok {
		t.Fatal("the fake machine keeps worktrees")
	}
}
