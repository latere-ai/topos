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

// TestAdoptAnotherSessionsCheckpoint: a fork adopts the checkpoint it
// forked at from its own repository when that holds the commit, from the
// parent's session repository otherwise, under its own ref of the turn;
// a commit neither holds is adopted nowhere and recorded nowhere.
func TestAdoptAnotherSessionsCheckpoint(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	repos := filepath.Join(filepath.Dir(work), "repos")
	parent := &Checkpointer{Machine: h, SessionID: "ses_p", SessionRepo: filepath.Join(repos, "ses_p.git")}
	write(t, filepath.Join(work, "a.txt"), "the parent's")
	cp, err := parent.Take(t.Context(), 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if have, err := parent.Has(t.Context(), cp.Commit); err != nil || !have {
		t.Fatalf("the parent's own checkpoint: %v, %v", have, err)
	}

	child := &Checkpointer{Machine: h, SessionID: "ses_c", SessionRepo: filepath.Join(repos, "ses_c.git")}
	if have, err := child.Has(t.Context(), cp.Commit); err != nil || have {
		t.Fatalf("the child holds the parent's checkpoint before adopting it: %v, %v", have, err)
	}
	if _, ok, err := child.Adopt(t.Context(), 2, cp.Commit, "", cp.Ref); err != nil || ok {
		t.Fatalf("adopted with nowhere to take it from: %v, %v", ok, err)
	}
	if _, ok, err := child.Adopt(t.Context(), 2, cp.Commit, filepath.Join(repos, "ses_none.git"), cp.Ref); err != nil || ok {
		t.Fatalf("adopted from a repository that does not exist: %v, %v", ok, err)
	}
	got, ok, err := child.Adopt(t.Context(), 2, cp.Commit, parent.SessionRepo, cp.Ref)
	if err != nil || !ok || got.Ref != "refs/topos/checkpoints/ses_c/2" || got.Commit != cp.Commit {
		t.Fatalf("adopt from the parent's repository: %+v, %v, %v", got, ok, err)
	}
	if have, err := child.Has(t.Context(), cp.Commit); err != nil || !have {
		t.Fatalf("the child after adopting: %v, %v", have, err)
	}
	// Held already, the commit is adopted under the child's own ref alone.
	again := &Checkpointer{Machine: h, SessionID: "ses_d", SessionRepo: child.SessionRepo}
	got, ok, err = again.Adopt(t.Context(), 2, cp.Commit, "", "")
	if err != nil || !ok || got.Ref != "refs/topos/checkpoints/ses_d/2" {
		t.Fatalf("adopt a held commit: %+v, %v, %v", got, ok, err)
	}
	if ref := git(t, h, map[string]string{"GIT_DIR": child.SessionRepo}, "rev-parse "+got.Ref); ref != cp.Commit {
		t.Fatalf("the adopted ref points at %s, want %s", ref, cp.Commit)
	}
	write(t, filepath.Join(work, "a.txt"), "changed since")
	if err := child.Restore(t.Context(), cp.Commit, ""); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(work, "a.txt")) != "the parent's" {
		t.Fatal("the adopted checkpoint was not restored")
	}
	// The parent's ref there names another commit than the one asked.
	other := filepath.Join(repos, "ses_q.git")
	git(t, h, nil, "init --quiet --bare "+other)
	git(t, h, map[string]string{"GIT_DIR": other}, "fetch --quiet "+parent.SessionRepo+" "+cp.Ref+":"+cp.Ref)
	moved := &Checkpointer{Machine: h, SessionID: "ses_p", SessionRepo: other}
	write(t, filepath.Join(work, "a.txt"), "a later turn")
	later, err := moved.Take(t.Context(), 2, cp.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if later.Ref != cp.Ref {
		t.Fatalf("the later turn's ref %s", later.Ref)
	}
	fresh := &Checkpointer{Machine: h, SessionID: "ses_e", SessionRepo: filepath.Join(repos, "ses_e.git")}
	if _, ok, err := fresh.Adopt(t.Context(), 2, strings.Repeat("0", 40), other, cp.Ref); err != nil || ok {
		t.Fatalf("adopted a commit the other repository lacks: %v, %v", ok, err)
	}
	if _, ok, err := fresh.Adopt(t.Context(), 2, cp.Commit, parent.SessionRepo, "refs/topos/checkpoints/ses_p/9"); err == nil || ok {
		t.Fatalf("adopted through a ref the parent lacks: %v, %v", ok, err)
	}
	// A ref of the other repository that names an unrelated commit brings
	// nothing of the one asked.
	unrelated, err := moved.Take(t.Context(), 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := fresh.Adopt(t.Context(), 2, cp.Commit, other, unrelated.Ref); err != nil || ok {
		t.Fatalf("adopted through a ref that does not reach the commit: %v, %v", ok, err)
	}

	gone, _ := open(t, nil)
	if err := gone.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Checkpointer{Machine: gone, SessionID: "ses_g"}).exists(t.Context(), nil, cp.Commit); err == nil {
		t.Fatal("a released machine answered whether it holds a commit")
	}
	bare, _ := open(t, []string{"PATH=/nonexistent"})
	none := &Checkpointer{Machine: bare, SessionID: "ses_n", SessionRepo: "/tmp/never"}
	if _, err := none.Has(t.Context(), cp.Commit); !errors.Is(err, ErrNoGit) {
		t.Fatalf("has without git: %v", err)
	}
	if _, _, err := none.Adopt(t.Context(), 2, cp.Commit, "", ""); !errors.Is(err, ErrNoGit) {
		t.Fatalf("adopt without git: %v", err)
	}
	if _, _, err := (&Checkpointer{Machine: h, SessionID: "x"}).Adopt(t.Context(), 1, cp.Commit, "", ""); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("adopt with nowhere to keep it: %v", err)
	}
}

// remote makes a bare repository with one commit on main, and answers
// its file URL and its path. With options it takes push options, and a
// pre-receive hook writes each push's first option to pushed-option.
func remote(t *testing.T, h machine.Machine, base, name string, options bool) (string, string) {
	t.Helper()
	bare := filepath.Join(base, name)
	git(t, h, nil, "init --quiet --bare -b main "+bare)
	if options {
		git(t, h, map[string]string{"GIT_DIR": bare}, "config receive.advertisePushOptions true")
		write(t, filepath.Join(bare, "hooks", "pre-receive"), "#!/bin/sh\nprintf '%s' \"$GIT_PUSH_OPTION_0\" > \""+filepath.Join(bare, "pushed-option")+"\"\n")
		if err := os.Chmod(filepath.Join(bare, "hooks", "pre-receive"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return "file://" + bare, bare
}

// checkout clones url into the machine's working directory, as a
// delivery does.
func checkout(t *testing.T, h machine.Machine, url string) {
	t.Helper()
	id := map[string]string{"GIT_AUTHOR_NAME": "p", "GIT_AUTHOR_EMAIL": "p@example.com", "GIT_COMMITTER_NAME": "p", "GIT_COMMITTER_EMAIL": "p@example.com"}
	git(t, h, nil, "init --quiet -b main")
	git(t, h, nil, "remote add origin "+url)
	git(t, h, nil, "fetch --quiet origin")
	if out := git(t, h, nil, "ls-remote --heads origin"); out == "" {
		write(t, filepath.Join(h.Info().Workdir, "README.md"), "app\n")
		git(t, h, id, "add README.md")
		git(t, h, id, "commit --quiet -m start")
		git(t, h, nil, "push --quiet origin HEAD:main")
		return
	}
	git(t, h, nil, "checkout --quiet -B main origin/main")
}

// TestACheckpointIsKeptAtItsRepository: a checkpointer with a repository
// to keep its checkpoints at pushes each of the session's own there under
// the session's latest ref, with origo.event=off where the repository
// takes push options and without it where it does not, and names the
// repository on the checkpoint; a thread's checkpoint is not pushed.
func TestACheckpointIsKeptAtItsRepository(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	base := filepath.Dir(work)
	url, bare := remote(t, h, base, "app.git", true)
	checkout(t, h, url)
	c := &Checkpointer{Machine: h, SessionID: "ses_k", AgentID: "agent_1", Remote: url}
	write(t, filepath.Join(work, "notes.txt"), "draft\n")
	first, err := c.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Remote != url || first.Ref != "refs/topos/checkpoints/ses_k/1" {
		t.Fatalf("checkpoint %+v, want kept at %s", first, url)
	}
	at := map[string]string{"GIT_DIR": bare}
	if got := git(t, h, at, "rev-parse "+KeptRef("ses_k")); got != first.Commit {
		t.Fatalf("the repository's latest is %s, want %s", got, first.Commit)
	}
	if got := read(t, filepath.Join(bare, "pushed-option")); got != eventOff {
		t.Fatalf("the push carried %q, want %q", got, eventOff)
	}
	write(t, filepath.Join(work, "notes.txt"), "draft two\n")
	second, err := c.Take(t.Context(), 2, first.Commit)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, h, at, "rev-parse "+KeptRef("ses_k")); got != second.Commit || git(t, h, at, "rev-parse "+second.Commit+"^") != first.Commit {
		t.Fatalf("the repository's latest is %s, want %s chained to %s", got, second.Commit, first.Commit)
	}
	if refs := git(t, h, at, "for-each-ref '--format=%(refname)' refs/topos"); refs != KeptRef("ses_k") {
		t.Fatalf("the repository holds %q; a session keeps one ref there", refs)
	}

	plain, plainBare := remote(t, h, base, "plain.git", false)
	git(t, h, nil, "remote set-url origin "+plain)
	git(t, h, nil, "push --quiet origin HEAD:main")
	c.Remote = plain
	third, err := c.Take(t.Context(), 3, second.Commit)
	if err != nil || third.Remote != plain {
		t.Fatalf("a repository that takes no push options: %+v, %v", third, err)
	}
	if got := git(t, h, map[string]string{"GIT_DIR": plainBare}, "rev-parse "+KeptRef("ses_k")); got != third.Commit {
		t.Fatalf("the plain repository's latest is %s, want %s", got, third.Commit)
	}

	thread := &Checkpointer{Machine: h, SessionID: "ses_k", Thread: "evt_t", Remote: url}
	if cp, err := thread.Take(t.Context(), 1, ""); err != nil || cp.Remote != "" {
		t.Fatalf("a thread's checkpoint: %+v, %v; want it kept on the machine", cp, err)
	}
}

// TestAPushTheRepositoryRefusesKeepsTheCheckpointLocal: a repository that
// cannot be reached, and one whose hook refuses the push, leave the
// checkpoint taken, its ref on the machine, and no repository named.
func TestAPushTheRepositoryRefusesKeepsTheCheckpointLocal(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	base := filepath.Dir(work)
	url, bare := remote(t, h, base, "app.git", false)
	checkout(t, h, url)
	write(t, filepath.Join(bare, "hooks", "pre-receive"), "#!/bin/sh\necho over quota >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(bare, "hooks", "pre-receive"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, at := range []string{url, "file://" + filepath.Join(base, "missing.git")} {
		c := &Checkpointer{Machine: h, SessionID: "ses_r", Remote: at}
		cp, err := c.Take(t.Context(), 1, "")
		if err != nil || cp.Remote != "" || cp.Commit == "" {
			t.Fatalf("a refused push to %s: %+v, %v; want the checkpoint kept on the machine", at, cp, err)
		}
		if got := git(t, h, nil, "rev-parse "+cp.Ref); got != cp.Commit {
			t.Fatalf("the turn's ref is %s, want %s", got, cp.Commit)
		}
	}
	gone, _ := open(t, nil)
	if err := gone.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if (&Checkpointer{Machine: gone, SessionID: "ses_r", Remote: url}).keep(t.Context(), strings.Repeat("0", 40)) {
		t.Fatal("a released machine kept a checkpoint")
	}
}

// TestAKeptCheckpointIsFetchedByID: a fresh clone of the repository,
// which carries no checkpoint ref, fetches a kept checkpoint by its id,
// or through the session's latest ref from a repository that serves no
// object by its id, and adopts and restores it; a commit the repository
// does not have is ErrNotKept.
func TestAKeptCheckpointIsFetchedByID(t *testing.T) {
	needGit(t)
	h, work := open(t, nil)
	url, _ := remote(t, h, filepath.Dir(work), "app.git", false)
	checkout(t, h, url)
	parent := &Checkpointer{Machine: h, SessionID: "ses_p", Remote: url}
	write(t, filepath.Join(work, "notes.txt"), "the parent's\n")
	first, err := parent.Take(t.Context(), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(work, "notes.txt"), "later\n")
	if _, err := parent.Take(t.Context(), 2, first.Commit); err != nil {
		t.Fatal(err)
	}

	for _, v := range []string{"2", "0"} {
		home := t.TempDir()
		env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.version", "GIT_CONFIG_VALUE_0=" + v}
		fork, forkWork := open(t, env)
		checkout(t, fork, url)
		c := &Checkpointer{Machine: fork, SessionID: "ses_c", Remote: url}
		if have, err := c.Has(t.Context(), first.Commit); err != nil || have {
			t.Fatalf("protocol %s: a fresh clone holds the checkpoint: %v, %v", v, have, err)
		}
		if err := c.Fetch(t.Context(), first.Commit, "ses_p"); err != nil {
			t.Fatalf("protocol %s: fetch: %v", v, err)
		}
		cp, ok, err := c.Adopt(t.Context(), 1, first.Commit, "", "")
		if err != nil || !ok || cp.Ref != "refs/topos/checkpoints/ses_c/1" {
			t.Fatalf("protocol %s: adopt %+v, %v, %v", v, cp, ok, err)
		}
		if err := c.Restore(t.Context(), cp.Commit, ""); err != nil {
			t.Fatal(err)
		}
		if got := read(t, filepath.Join(forkWork, "notes.txt")); got != "the parent's\n" {
			t.Fatalf("protocol %s: notes.txt is %q", v, got)
		}
		err = c.Fetch(t.Context(), strings.Repeat("0", 40), "ses_p")
		if !errors.Is(err, ErrNotKept) || !strings.Contains(err.Error(), url) {
			t.Fatalf("protocol %s: a commit the repository lacks: %v", v, err)
		}
	}
	if err := (&Checkpointer{Machine: h, SessionID: "ses_c"}).Fetch(t.Context(), first.Commit, "ses_p"); !errors.Is(err, ErrNotKept) {
		t.Fatalf("a fetch with no repository to fetch from: %v", err)
	}
	none, _ := open(t, nil)
	if err := (&Checkpointer{Machine: none, SessionID: "ses_c", Remote: url}).Fetch(t.Context(), first.Commit, "ses_p"); !errors.Is(err, ErrNoRepository) {
		t.Fatalf("a fetch into a directory with no repository: %v", err)
	}
	if err := none.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	gone := &Checkpointer{Machine: none, SessionID: "ses_c", Remote: url, SessionRepo: filepath.Join(t.TempDir(), "s.git")}
	if err := gone.Fetch(t.Context(), first.Commit, "ses_p"); err == nil || errors.Is(err, ErrNotKept) {
		t.Fatalf("a fetch on a released machine: %v", err)
	}
	if firstLine("\n\n") != "git said nothing" || firstLine("\n fatal: no\nmore") != "fatal: no" {
		t.Fatal("firstLine")
	}
}
