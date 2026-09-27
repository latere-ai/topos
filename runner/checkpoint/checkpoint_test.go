// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package checkpoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

func open(t *testing.T, env []string) (*host.Host, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if env == nil {
		env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base}
	}
	h, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill"), Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	})
	return h, work
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func git(t *testing.T, m machine.Machine, env map[string]string, args string) string {
	t.Helper()
	res, err := m.Exec(t.Context(), machine.ExecRequest{Command: "git " + args, Env: env})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("git %s: %v %s", args, err, res.Output)
	}
	return strings.TrimSpace(string(res.Output))
}

func TestCheckpointsOutsideARepository(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	repo := filepath.Join(filepath.Dir(work), "repos", "ses_1.git")
	c := &Checkpointer{Machine: h, SessionID: "ses_1", AgentID: "agent_1", SessionRepo: repo}
	write(t, filepath.Join(work, "a.txt"), "one")
	write(t, filepath.Join(work, ".env"), "SECRET=1")
	write(t, filepath.Join(work, "keys", "server.pem"), "pem")
	first, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Ref != "refs/topos/checkpoints/ses_1/1" || len(first.Commit) < 40 {
		t.Fatalf("checkpoint %+v", first)
	}
	env := map[string]string{"GIT_DIR": repo}
	files := git(t, h, env, "ls-tree -r --name-only "+first.Commit)
	if files != "a.txt" {
		t.Fatalf("the checkpoint holds %q; deny-listed files are never taken", files)
	}
	if msg := git(t, h, env, "log -1 --format=%B "+first.Commit); !strings.Contains(msg, "Topos-Session: ses_1") || !strings.Contains(msg, "Topos-Agent: agent_1") {
		t.Fatalf("message %q", msg)
	}

	write(t, filepath.Join(work, "a.txt"), "two")
	write(t, filepath.Join(work, "b.txt"), "new")
	second, err := c.Take(t.Context(), 2, first.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if parent := git(t, h, env, "rev-parse "+second.Commit+"^"); parent != first.Commit {
		t.Fatalf("parent %s, want the previous checkpoint %s", parent, first.Commit)
	}
	third, err := c.Take(t.Context(), 3, second.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if git(t, h, env, "rev-parse "+third.Commit+"^{tree}") != git(t, h, env, "rev-parse "+second.Commit+"^{tree}") {
		t.Fatal("an unchanged turn did not reuse the tree")
	}

	if err := c.Restore(t.Context(), first.Commit, third.Commit); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(work, "a.txt")) != "one" {
		t.Fatal("a.txt was not restored")
	}
	if _, err := os.Stat(filepath.Join(work, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("a file the checkpoint lacks survived: %v", err)
	}
	if read(t, filepath.Join(work, ".env")) != "SECRET=1" {
		t.Fatal("rewind touched a deny-listed file")
	}
	if err := c.Restore(t.Context(), strings.Repeat("0", 40), ""); err == nil || !strings.Contains(err.Error(), CodeMissing) {
		t.Fatalf("a missing checkpoint: %v", err)
	}
}

func TestCheckpointsInACheckoutLeaveHeadAndIndexAlone(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	id := map[string]string{"GIT_AUTHOR_NAME": "p", "GIT_AUTHOR_EMAIL": "p@example.com", "GIT_COMMITTER_NAME": "p", "GIT_COMMITTER_EMAIL": "p@example.com"}
	git(t, h, nil, "init --quiet -b main")
	write(t, filepath.Join(work, "main.go"), "package main\n")
	write(t, filepath.Join(work, ".gitignore"), "build/\n")
	git(t, h, id, "add main.go .gitignore")
	git(t, h, id, "commit --quiet -m init")
	head := git(t, h, nil, "rev-parse HEAD")
	write(t, filepath.Join(work, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(work, "staged.go"), "package main\n")
	git(t, h, nil, "add staged.go")
	write(t, filepath.Join(work, "build", "out.bin"), "ignored")
	staged := git(t, h, nil, "diff --cached --name-only")

	old := maxFileBytes
	maxFileBytes = 1024
	defer func() { maxFileBytes = old }()
	write(t, filepath.Join(work, "big.dat"), strings.Repeat("x", 4096))

	c := &Checkpointer{Machine: h, SessionID: "ses_2", AgentID: "agent_1", Thread: "evt_t"}
	cp, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if cp.Ref != "refs/topos/checkpoints/ses_2/threads/evt_t/1" {
		t.Fatalf("ref %s", cp.Ref)
	}
	if git(t, h, nil, "rev-parse HEAD") != head || git(t, h, nil, "diff --cached --name-only") != staged || git(t, h, nil, "rev-parse --abbrev-ref HEAD") != "main" {
		t.Fatal("the checkpoint moved HEAD, the branch or the index")
	}
	files := git(t, h, nil, "ls-tree -r --name-only "+cp.Commit)
	if !strings.Contains(files, "main.go") || !strings.Contains(files, "staged.go") || strings.Contains(files, "build/") || strings.Contains(files, "big.dat") {
		t.Fatalf("checkpoint files %q", files)
	}
	if msg := git(t, h, nil, "log -1 --format=%B "+cp.Commit); !strings.Contains(msg, "Topos-Excluded: big.dat") {
		t.Fatalf("message %q", msg)
	}
	if refs := git(t, h, nil, "for-each-ref '--format=%(refname)' refs/heads refs/tags"); refs != "refs/heads/main" {
		t.Fatalf("refs %q", refs)
	}
}

func TestAMachineWithoutGitKeepsNoCheckpoint(t *testing.T) {
	h, _ := open(t, []string{"PATH=/nonexistent"})
	c := &Checkpointer{Machine: h, SessionID: "ses_3", SessionRepo: "/tmp/never"}
	if _, err := c.Take(t.Context(), 1, ""); !errors.Is(err, ErrNoGit) {
		t.Fatalf("take without git: %v", err)
	}
	if err := c.Restore(t.Context(), "abc", ""); !errors.Is(err, ErrNoGit) {
		t.Fatalf("restore without git: %v", err)
	}
}

func TestANonCheckoutNeedsASessionRepository(t *testing.T) {
	needGit(t)
	h, _ := open(t, nil)
	c := &Checkpointer{Machine: h, SessionID: "ses_4"}
	if _, err := c.Take(t.Context(), 1, ""); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("a checkpoint with nowhere to keep it: %v", err)
	}
	if quote("it's") != `'it'\''s'` {
		t.Fatal("quote")
	}
}

func TestGitFailuresAreReported(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	file := filepath.Join(filepath.Dir(work), "file")
	write(t, file, "x")
	bad := &Checkpointer{Machine: h, SessionID: "ses_5", SessionRepo: filepath.Join(file, "repo.git")}
	if _, err := bad.Take(t.Context(), 1, ""); err == nil || errors.Is(err, ErrNoGit) {
		t.Fatalf("a session repository under a file: %v", err)
	}
	c := &Checkpointer{Machine: h, SessionID: "ses_5", SessionRepo: filepath.Join(filepath.Dir(work), "repos", "ses_5.git")}
	write(t, filepath.Join(work, "a.txt"), "a")
	if _, err := c.Take(t.Context(), 1, strings.Repeat("f", 40)); err == nil {
		t.Fatal("chained to a commit that does not exist")
	}
	first, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, made, err := c.Save(t.Context(), 1, first.Commit); err != nil || made {
		t.Fatalf("saving an unchanged tree: %v, %v", made, err)
	}
	write(t, filepath.Join(work, "a.txt"), "changed")
	saved, made, err := c.Save(t.Context(), 1, first.Commit)
	if err != nil || !made || !strings.HasSuffix(saved.Ref, "/saved/1") {
		t.Fatalf("save %+v %v %v", saved, made, err)
	}
	if err := c.Restore(t.Context(), first.Commit, strings.Repeat("e", 40)); err == nil {
		t.Fatal("restored against a saved state that does not exist")
	}
	if _, _, err := (&Checkpointer{Machine: h, SessionID: "x"}).Save(t.Context(), 1, ""); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("save with nowhere to keep it: %v", err)
	}
}

func TestAReleasedMachineTakesNoCheckpoint(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	c := &Checkpointer{Machine: h, SessionID: "ses_6", SessionRepo: filepath.Join(filepath.Dir(work), "ses_6.git")}
	write(t, filepath.Join(work, "a.txt"), "a")
	first, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(work, "gone.txt"), "b")
	second, err := c.Take(t.Context(), 2, first.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(work, 0o500); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		if err := c.Restore(t.Context(), first.Commit, second.Commit); err == nil {
			t.Fatal("a restore that could not remove a file succeeded")
		}
	}
	if err := os.Chmod(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Take(t.Context(), 3, second.Commit); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("take on a released machine: %v", err)
	}
	if err := c.Restore(t.Context(), first.Commit, ""); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("restore on a released machine: %v", err)
	}
}

func TestAnUnwritableIndexFailsTheCheckpoint(t *testing.T) {
	needGit(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	h, work := open(t, nil)
	c := &Checkpointer{Machine: h, SessionID: "ses_7", SessionRepo: filepath.Join(filepath.Dir(work), "ses_7.git")}
	write(t, filepath.Join(work, "a.txt"), "a")
	first, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	spill := h.SpillDir()
	if err := os.Chmod(spill, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(spill, 0o755); err != nil {
			t.Error(err)
		}
	})
	if _, err := c.Take(t.Context(), 2, first.Commit); err == nil {
		t.Fatal("a checkpoint whose index cannot be written succeeded")
	}
	if err := c.Restore(t.Context(), first.Commit, ""); err == nil {
		t.Fatal("a restore whose index cannot be written succeeded")
	}
}
