// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a git checkout with one commit on main, the worktree directory
// beside it, and the environment git runs in.
func repo(t *testing.T) (work, worktrees string, env []string) {
	t.Helper()
	if _, err := osexec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work, worktrees = filepath.Join(base, "work"), filepath.Join(base, "data", "worktrees")
	env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + base, "GIT_AUTHOR_NAME=p", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=p", "GIT_COMMITTER_EMAIL=p@example.com"}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		gitIn(t, work, env, args...)
	}
	return work, worktrees, env
}

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, err := git(t.Context(), dir, env, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// sessions answers Active from a set of sessions that have not ended.
type sessions map[string]bool

func (s sessions) active(_ context.Context, id string) (bool, error) { return s[id], nil }

// TestSecondSessionGetsAWorktree: the first session writes the checkout
// and owns it; while it has not ended a second and a third each get a
// worktree of their own on agents/<agent>/<session>, owned by them;
// once it has ended the next session writes the checkout again; the
// owner reclaiming its checkout, and a directory that is no checkout,
// are written in place.
func TestSecondSessionGetsAWorktree(t *testing.T) {
	work, worktrees, env := repo(t)
	ctx := t.Context()
	live := sessions{"ses_1": true}
	claim := func(id string) Checkout {
		t.Helper()
		c, err := Claim(ctx, ClaimOptions{Dir: work, WorktreeDir: worktrees, Agent: "reviewer", Session: id, Active: live.active, Environ: env})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := claim("ses_1"); c.Workdir != work || c.Branch != "" {
		t.Fatalf("the first session %+v", c)
	}
	for _, id := range []string{"ses_2", "ses_3"} {
		c := claim(id)
		if c.Workdir != filepath.Join(worktrees, id) || c.Branch != "agents/reviewer/"+id {
			t.Fatalf("%s: %+v", id, c)
		}
		if got := gitIn(t, c.Workdir, env, "branch", "--show-current"); got != c.Branch {
			t.Fatalf("%s's worktree is on %q", id, got)
		}
		gitDir := gitIn(t, c.Workdir, env, "rev-parse", "--absolute-git-dir")
		if owner, err := readTrimmed(filepath.Join(gitDir, ownerFile)); err != nil || owner != id {
			t.Fatalf("%s's worktree is owned by %q, %v", id, owner, err)
		}
	}
	if c := claim("ses_1"); c.Workdir != work {
		t.Fatalf("the owner reclaiming its checkout %+v", c)
	}
	live["ses_1"] = false
	if c := claim("ses_4"); c.Workdir != work || c.Branch != "" {
		t.Fatalf("after the owner ended %+v", c)
	}
	if owner, err := readTrimmed(filepath.Join(work, ".git", ownerFile)); err != nil || owner != "ses_4" {
		t.Fatalf("the checkout's owner %q, %v", owner, err)
	}
	plain := t.TempDir()
	if c, err := Claim(ctx, ClaimOptions{Dir: plain, WorktreeDir: worktrees, Agent: "reviewer", Session: "ses_5", Active: live.active, Environ: env}); err != nil || c.Workdir != plain {
		t.Fatalf("a directory that is no checkout: %+v, %v", c, err)
	}
	if _, err := os.Stat(filepath.Join(plain, ".git")); !os.IsNotExist(err) {
		t.Fatalf("a claim made a git directory in a plain one: %v", err)
	}
}

// TestWorktreeRemovalRule: at the session's end a clean worktree whose
// head is merged into its base, or contained in its upstream, is removed
// and its branch kept; one with an uncommitted change, and one with a
// commit neither merged nor pushed, stay; a checkout written in place
// loses its owner file, and a directory the session does not own is left
// alone.
func TestWorktreeRemovalRule(t *testing.T) {
	work, worktrees, env := repo(t)
	ctx := t.Context()
	live := sessions{"ses_owner": true}
	claim := func(id string) Checkout {
		t.Helper()
		c, err := Claim(ctx, ClaimOptions{Dir: work, WorktreeDir: worktrees, Agent: "a", Session: id, Active: live.active, Environ: env})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	claim("ses_owner")
	merged, dirty, ahead, pushed := claim("ses_merged"), claim("ses_dirty"), claim("ses_ahead"), claim("ses_pushed")
	if err := os.WriteFile(filepath.Join(dirty.Workdir, "notes.txt"), []byte("not committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, ahead.Workdir, env, "commit", "-q", "--allow-empty", "-m", "ahead of main")
	remote := filepath.Join(filepath.Dir(worktrees), "remote.git")
	gitIn(t, work, env, "init", "-q", "--bare", remote)
	gitIn(t, work, env, "remote", "add", "origin", remote)
	gitIn(t, pushed.Workdir, env, "commit", "-q", "--allow-empty", "-m", "pushed")
	gitIn(t, pushed.Workdir, env, "push", "-q", "-u", "origin", pushed.Branch)

	for _, c := range []struct {
		name    string
		co      Checkout
		session string
		removed bool
	}{
		{"merged", merged, "ses_merged", true},
		{"pushed", pushed, "ses_pushed", true},
		{"dirty", dirty, "ses_dirty", false},
		{"ahead", ahead, "ses_ahead", false},
		{"not the owner", ahead, "ses_merged", false},
	} {
		removed, err := ReleaseCheckout(ctx, c.co.Workdir, c.session, env)
		if err != nil || removed != c.removed {
			t.Fatalf("%s: removed %v, %v", c.name, removed, err)
		}
		_, statErr := os.Stat(c.co.Workdir)
		if gone := os.IsNotExist(statErr); gone != c.removed {
			t.Fatalf("%s: the worktree is gone %v", c.name, gone)
		}
		if c.removed && !strings.Contains(gitIn(t, work, env, "branch", "--list", c.co.Branch), c.co.Branch) {
			t.Fatalf("%s: the branch %s went with the worktree", c.name, c.co.Branch)
		}
	}
	if removed, err := ReleaseCheckout(ctx, work, "ses_owner", env); err != nil || removed {
		t.Fatalf("the checkout's owner: removed %v, %v", removed, err)
	}
	if owner, err := readTrimmed(filepath.Join(work, ".git", ownerFile)); err != nil || owner != "" {
		t.Fatalf("the owner file after the owner's end: %q, %v", owner, err)
	}
}
