// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
)

type fixture struct {
	h       *Host
	work    string
	outside string
	home    string
}

func open(t *testing.T) fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{work: filepath.Join(base, "work"), outside: filepath.Join(base, "outside"), home: filepath.Join(base, "home")}
	for _, d := range []string{f.work, f.outside, filepath.Join(f.home, ".ssh"), filepath.Join(base, "memory")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	h, err := Open(Options{
		Workdir: f.work, Roots: []string{filepath.Join(base, "memory"), f.work}, SpillDir: filepath.Join(base, "spill"),
		Home: f.home, DataDir: filepath.Join(base, "data"), ID: "host-1",
		Environ: []string{"PATH=" + os.Getenv("PATH"), "GITHUB_TOKEN=ghp_secret", "LANG=C"},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.h = h
	t.Cleanup(func() {
		if err := h.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	return f
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

func read(t *testing.T, h *Host, path string) (string, error) {
	t.Helper()
	rc, err := h.ReadFile(t.Context(), path)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestOpenValidates(t *testing.T) {
	dir := t.TempDir()
	for name, o := range map[string]Options{
		"no workdir":         {SpillDir: filepath.Join(dir, "s")},
		"no spill":           {Workdir: dir},
		"missing workdir":    {Workdir: filepath.Join(dir, "absent"), SpillDir: filepath.Join(dir, "s")},
		"spill under a file": {Workdir: dir, SpillDir: filepath.Join(dir, "file", "s")},
	} {
		if name == "spill under a file" {
			write(t, filepath.Join(dir, "file"), "x")
		}
		if _, err := Open(o); err == nil {
			t.Fatalf("%s: opened", name)
		}
	}
}

func TestInfoAndRoots(t *testing.T) {
	f := open(t)
	info := f.h.Info()
	if info.Kind != machine.KindHost || info.Workdir != f.work || info.ID != "host-1" || info.OS == "" || info.Arch == "" {
		t.Fatalf("info %+v", info)
	}
	roots := f.h.Roots()
	if len(roots) != 3 || roots[0] != f.work || !strings.HasSuffix(f.h.SpillDir(), "spill") || roots[2] != f.h.SpillDir() {
		t.Fatalf("roots %v, spill %s", roots, f.h.SpillDir())
	}
}

func TestFilesAreConfinedToTheRoots(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	if err := f.h.WriteFile(ctx, "src/deep/a.txt", strings.NewReader("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, f.h, filepath.Join(f.work, "src/deep/a.txt")); err != nil || got != "hello" {
		t.Fatalf("read back %q, %v", got, err)
	}
	if err := f.h.WriteFile(ctx, "src/deep/a.txt", strings.NewReader("again"), 0); err != nil {
		t.Fatal(err)
	}
	fi, err := f.h.Stat(ctx, "src/deep/a.txt")
	if err != nil || fi.Size != 5 || fi.IsDir || fi.Path != filepath.Join(f.work, "src/deep/a.txt") || fi.Mode.Perm() != 0o644 {
		t.Fatalf("stat %+v, %v", fi, err)
	}
	write(t, filepath.Join(f.outside, "secret.txt"), "outside")
	if err := os.Symlink(filepath.Join(f.outside, "secret.txt"), filepath.Join(f.work, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(f.outside, "secret.txt"), "../outside/secret.txt", "link", "/etc/hosts"} {
		if _, err := read(t, f.h, p); !errors.Is(err, machine.ErrOutside) {
			t.Fatalf("read %s: %v, want ErrOutside", p, err)
		}
	}
	if err := f.h.WriteFile(ctx, filepath.Join(f.outside, "x"), strings.NewReader("x"), 0); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("a write outside: %v", err)
	}
	if _, err := read(t, f.h, "src"); err == nil {
		t.Fatal("read a directory")
	}
	if err := f.h.WriteFile(ctx, f.work, strings.NewReader("x"), 0); err == nil {
		t.Fatal("wrote over the working directory")
	}
	if _, err := read(t, f.h, "absent.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
}

func TestTheDenyListHoldsInsideARoot(t *testing.T) {
	f := open(t)
	write(t, filepath.Join(f.work, ".env"), "API_KEY=abc")
	write(t, filepath.Join(f.work, ".env.example"), "API_KEY=")
	write(t, filepath.Join(f.work, "notes.md"), "API_KEY lives in .env")
	if _, err := read(t, f.h, ".env"); !errors.Is(err, machine.ErrDenied) {
		t.Fatalf("read .env: %v", err)
	}
	if _, err := read(t, f.h, ".env.example"); err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	list, err := f.h.List(t.Context(), ".")
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, fi := range list {
		names = append(names, filepath.Base(fi.Path))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{".env.example", "notes.md"}) {
		t.Fatalf("list shows %v", names)
	}
	res, err := f.h.Search(t.Context(), machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "API_KEY"})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Lines {
		if strings.HasSuffix(l, "/.env") {
			t.Fatalf("grep read .env: %v", res.Lines)
		}
	}
	if len(res.Lines) != 2 {
		t.Fatalf("grep %v", res.Lines)
	}
	if _, err := f.h.Search(t.Context(), machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: f.outside}); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("a search outside: %v", err)
	}
}

func TestRemoveAndRename(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	write(t, filepath.Join(f.work, "a.txt"), "a")
	if err := f.h.Rename(ctx, "a.txt", "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Rename(ctx, "b.txt", filepath.Join(f.h.SpillDir(), "b.txt")); err == nil {
		t.Fatal("renamed across roots")
	}
	if err := f.h.Rename(ctx, "b.txt", "../outside/b.txt"); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("rename outside: %v", err)
	}
	if err := f.h.Rename(ctx, "../outside/x", "b.txt"); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("rename from outside: %v", err)
	}
	if err := f.h.Remove(ctx, "b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Remove(ctx, "."); err == nil {
		t.Fatal("removed the working directory")
	}
	if _, err := f.h.Stat(ctx, "b.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat after remove: %v", err)
	}
	if _, err := f.h.List(ctx, "absent"); err == nil {
		t.Fatal("listed a missing directory")
	}
}

func run(t *testing.T, h *Host, r machine.ExecRequest) machine.ExecResult {
	t.Helper()
	res, err := h.Exec(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExec(t *testing.T) {
	f := open(t)
	res := run(t, f.h, machine.ExecRequest{Command: `echo out; echo err >&2; echo ${GITHUB_TOKEN:-unset}; echo $EXTRA; exit 3`, Env: map[string]string{"EXTRA": "added"}})
	if string(res.Output) != "out\nerr\nunset\nadded\n" || res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("exec %q code %d", res.Output, res.ExitCode)
	}
	if err := os.Mkdir(filepath.Join(f.work, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = run(t, f.h, machine.ExecRequest{Command: "cd sub && pwd", ReportDir: true})
	if res.Dir != filepath.Join(f.work, "sub") || strings.TrimSpace(string(res.Output)) != res.Dir {
		t.Fatalf("report dir %q, output %q", res.Dir, res.Output)
	}
	res = run(t, f.h, machine.ExecRequest{Command: "cd sub; exit 4", ReportDir: true, Dir: f.work})
	if res.Dir != filepath.Join(f.work, "sub") || res.ExitCode != 4 {
		t.Fatalf("an explicit exit still reports the directory: %q, %d", res.Dir, res.ExitCode)
	}
	res = run(t, f.h, machine.ExecRequest{Command: "pwd", Dir: "sub"})
	if strings.TrimSpace(string(res.Output)) != filepath.Join(f.work, "sub") {
		t.Fatalf("a relative start directory: %q", res.Output)
	}
	res = run(t, f.h, machine.ExecRequest{Command: "read x; echo got $x", Stdin: strings.NewReader("abc\n")})
	if string(res.Output) != "got abc\n" {
		t.Fatalf("stdin %q", res.Output)
	}
}

func TestExecTimeoutAndCancel(t *testing.T) {
	f := open(t)
	start := time.Now()
	res := run(t, f.h, machine.ExecRequest{Command: "echo started; /bin/sleep 30", Timeout: 200 * time.Millisecond})
	if !res.TimedOut || time.Since(start) > 10*time.Second || !strings.Contains(string(res.Output), "started") {
		t.Fatalf("timeout %+v after %s", res, time.Since(start))
	}
	old := KillGrace
	KillGrace = 200 * time.Millisecond
	defer func() { KillGrace = old }()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	res, err := f.h.Exec(ctx, machine.ExecRequest{Command: "trap '' TERM; /bin/sleep 30"})
	if err != nil || !res.Canceled || time.Since(start) > 20*time.Second {
		t.Fatalf("cancel %+v, %v", res, err)
	}
	start = time.Now()
	res = run(t, f.h, machine.ExecRequest{Command: "/bin/sleep 30 & echo left"})
	if time.Since(start) > 10*time.Second || string(res.Output) != "left\n" {
		t.Fatalf("a stray child held the call: %q after %s", res.Output, time.Since(start))
	}
}

func TestExecStream(t *testing.T) {
	f := open(t)
	s, err := f.h.ExecStream(t.Context(), machine.ExecRequest{Command: "echo one; echo two"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(s)
	if err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("stream %q, %v", b, err)
	}
	res, err := s.Wait()
	if err != nil || res.ExitCode != 0 || len(res.Output) != 0 {
		t.Fatalf("wait %+v, %v", res, err)
	}
	if _, err := f.h.ExecStream(t.Context(), machine.ExecRequest{Command: "x", Background: true}); err == nil {
		t.Fatal("a background stream")
	}
}

func TestBackgroundJobsEndWithTheSession(t *testing.T) {
	f := open(t)
	res := run(t, f.h, machine.ExecRequest{Command: "echo serving; /bin/sleep 30", Background: true})
	if res.PID == 0 || !strings.HasPrefix(res.Log, f.h.SpillDir()) {
		t.Fatalf("job %+v", res)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(res.Log)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("serving")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job never wrote its log: %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	done := run(t, f.h, machine.ExecRequest{Command: "exit 5", Background: true})
	for {
		b, err := os.ReadFile(done.Log)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("exited with code 5")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a finished job's log has no exit line: %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := f.h.Release(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := f.h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(res.Log)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("exited with code")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job survived the session: %q", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := f.h.Exec(t.Context(), machine.ExecRequest{Command: "true"}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("exec after release: %v", err)
	}
	if _, err := f.h.Exec(t.Context(), machine.ExecRequest{Command: "true", Background: true}); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("a job after release: %v", err)
	}
	if _, err := read(t, f.h, "x"); !errors.Is(err, machine.ErrReleased) {
		t.Fatalf("read after release: %v", err)
	}
	if err := f.h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
}

func TestEveryFileOperationRefusesOutsideAndDenied(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	write(t, filepath.Join(f.work, ".env"), "x")
	for _, p := range []string{".env", filepath.Join(f.home, ".ssh", "id_ed25519")} {
		if _, err := f.h.Stat(ctx, p); !errors.Is(err, machine.ErrDenied) {
			t.Fatalf("stat %s: %v", p, err)
		}
		if err := f.h.Remove(ctx, p); !errors.Is(err, machine.ErrDenied) {
			t.Fatalf("remove %s: %v", p, err)
		}
		if _, err := f.h.List(ctx, p); !errors.Is(err, machine.ErrDenied) {
			t.Fatalf("list %s: %v", p, err)
		}
		if err := f.h.WriteFile(ctx, p, strings.NewReader("x"), 0); !errors.Is(err, machine.ErrDenied) {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := f.h.Rename(ctx, "a", p); !errors.Is(err, machine.ErrDenied) {
			t.Fatalf("rename onto %s: %v", p, err)
		}
	}
	if _, err := f.h.Stat(ctx, f.outside); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("stat outside: %v", err)
	}
	if err := f.h.Remove(ctx, "../outside"); !errors.Is(err, machine.ErrOutside) {
		t.Fatalf("remove outside: %v", err)
	}
	write(t, filepath.Join(f.work, "file"), "x")
	if err := f.h.WriteFile(ctx, "file/child.txt", strings.NewReader("x"), 0); err == nil {
		t.Fatal("wrote under a file")
	}
	if err := f.h.WriteFile(ctx, "script.sh", strings.NewReader("echo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := f.h.Stat(ctx, "script.sh"); err != nil || fi.Mode.Perm() != 0o755 {
		t.Fatalf("an explicit mode: %v, %v", fi.Mode, err)
	}
	if err := f.h.WriteFile(ctx, "fail.txt", io.MultiReader(strings.NewReader("a"), failing{}), 0); err == nil {
		t.Fatal("a write whose reader failed succeeded")
	}
	if _, err := f.h.Stat(ctx, "fail.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed write left a file: %v", err)
	}
	d := denyFS{FS: os.DirFS(f.work), base: f.work, deny: machine.DenyList{}}
	if _, err := d.Open(".env"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("the search filesystem opened .env: %v", err)
	}
	if _, err := d.ReadDir("absent"); err == nil {
		t.Fatal("read a missing directory")
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("disk full") }

func TestOpenRefusesAMissingExtraRoot(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(Options{Workdir: dir, SpillDir: filepath.Join(dir, "s"), Roots: []string{filepath.Join(dir, "absent")}}); err == nil {
		t.Fatal("opened with a root that does not exist")
	}
	h, err := Open(Options{Workdir: dir, SpillDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if h.SpillDir() != h.Info().Workdir {
		t.Fatalf("a spill directory equal to the working directory: %s", h.SpillDir())
	}
	if err := h.Release(t.Context(), true); err != nil {
		t.Fatal(err)
	}
}

func TestWorktrees(t *testing.T) {
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base, "GIT_AUTHOR_NAME=p", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=p", "GIT_COMMITTER_EMAIL=p@example.com"}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	h, err := Open(Options{Workdir: work, SpillDir: filepath.Join(base, "spill"), WorktreeDir: filepath.Join(base, "wt"), Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	})
	if _, err := h.Worktree(t.Context(), "t1", "agents/a/s.t1"); err == nil {
		t.Fatal("a worktree outside a repository")
	}
	for _, cmd := range []string{"git init -q -b main", "echo x > a.txt", "git add a.txt", "git commit -q -m init"} {
		if res := run(t, h, machine.ExecRequest{Command: cmd}); res.ExitCode != 0 {
			t.Fatalf("%s: %s", cmd, res.Output)
		}
	}
	m, err := h.Worktree(t.Context(), "t1", "agents/a/s.t1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Info().Workdir != filepath.Join(base, "wt", "t1") || !strings.HasSuffix(m.SpillDir(), filepath.Join("worktrees", "t1")) {
		t.Fatalf("worktree machine %+v spill %s", m.Info(), m.SpillDir())
	}
	if got, err := read(t, m.(*Host), "a.txt"); err != nil || got != "x\n" {
		t.Fatalf("the worktree's files %q, %v", got, err)
	}
	again, err := h.Worktree(t.Context(), "t1", "agents/a/s.t1")
	if err != nil || again.Info().Workdir != m.Info().Workdir {
		t.Fatalf("reopen %v", err)
	}
	for _, bad := range []string{"", "..", "a/b"} {
		if _, err := h.Worktree(t.Context(), bad, "b"); err == nil {
			t.Fatalf("worktree name %q accepted", bad)
		}
	}
	plain, err := Open(Options{Workdir: work, SpillDir: filepath.Join(base, "spill2"), Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Worktree(t.Context(), "t1", "b"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("a host without a worktree directory: %v", err)
	}
	for _, r := range []machine.Machine{m, again, plain} {
		if err := r.Release(t.Context(), true); err != nil {
			t.Error(err)
		}
	}
}

// TestAScriptPastTheArgumentLimitRuns: a command longer than one
// argument may be, as a long heredoc makes, runs in the foreground and
// in the background, and leaves no script file behind.
func TestAScriptPastTheArgumentLimitRuns(t *testing.T) {
	f := open(t)
	long := "cd /\necho start\n" + strings.Repeat(": padding\n", 16<<10) + "echo end\n"
	res, err := f.h.Exec(t.Context(), machine.ExecRequest{Command: long, ReportDir: true})
	if err != nil || string(res.Output) != "start\nend\n" || res.Dir != "/" {
		t.Fatalf("a long script: %q %+v %v", res.Output, res, err)
	}
	job, err := f.h.Exec(t.Context(), machine.ExecRequest{Command: long, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		b, err := os.ReadFile(job.Log)
		if err == nil && strings.Contains(string(b), "exited with code 0") {
			if !strings.HasPrefix(string(b), "start\nend\n") {
				t.Fatalf("the job's log %q", b)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's log %q, %v", b, err)
		}
	}
	for _, dir := range []string{f.h.SpillDir(), filepath.Join(f.h.SpillDir(), "jobs")} {
		if left, _ := filepath.Glob(filepath.Join(dir, "topos-script-*")); len(left) != 0 {
			t.Fatalf("script files left in %s: %v", dir, left)
		}
	}
}
