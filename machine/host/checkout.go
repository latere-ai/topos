// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The files a claim keeps in a git directory (spec 009): the owner names
// the session that writes the checkout, and a worktree's own git
// directory also records where it came from.
const (
	ownerFile    = "topos/owner"
	worktreeFile = "topos/worktree"
)

// ClaimOptions say where a new session on the host starts and how to
// tell whether a session named by an owner file has ended.
type ClaimOptions struct {
	// Dir is the directory the session starts in.
	Dir string
	// WorktreeDir holds the session worktrees, one per session id.
	WorktreeDir string
	// Agent and Session name the branch, agents/<agent>/<session>, and
	// the owner file's content.
	Agent, Session string
	// Active reports whether the session an owner file names has not
	// ended.
	Active func(ctx context.Context, id string) (bool, error)
	// Environ is git's environment; nil is this process's.
	Environ []string
}

// Checkout is where a session on the host writes.
type Checkout struct {
	// Workdir is the session's working directory: the checkout itself, or
	// the session's own worktree of it.
	Workdir string
	// Branch is the worktree's branch; empty when the session writes the
	// directory it started in.
	Branch string
}

// worktree is what a session worktree's own git directory records: the
// checkout it was made from and the branch or commit that was checked
// out there, which a merged worktree's head is an ancestor of.
type worktree struct {
	Checkout string `json:"checkout"`
	Base     string `json:"base"`
}

// Claim picks the working directory of a new session (spec 009). Two
// sessions never write one working directory: a git checkout whose owner
// file names another session that has not ended gives the new session a
// worktree of its own, on agents/<agent>/<session> from HEAD, whose git
// directory names it; otherwise the session writes the checkout in place
// and its id becomes the owner. A directory that is no git checkout, or a
// host without git, is written in place with no owner.
func Claim(ctx context.Context, o ClaimOptions) (Checkout, error) {
	gitDir, ok, err := gitOutput(ctx, o.Dir, o.Environ, "rev-parse", "--absolute-git-dir")
	if err != nil || !ok {
		return Checkout{Workdir: o.Dir}, err
	}
	owner, err := readTrimmed(filepath.Join(gitDir, ownerFile))
	if err != nil {
		return Checkout{}, err
	}
	if owner != "" && owner != o.Session {
		active, err := o.Active(ctx, owner)
		if err != nil {
			return Checkout{}, fmt.Errorf("machine: the session %s that owns %s: %w", owner, o.Dir, err)
		}
		if active {
			return newWorktree(ctx, o)
		}
	}
	if err := writeFile(filepath.Join(gitDir, ownerFile), o.Session); err != nil {
		return Checkout{}, err
	}
	return Checkout{Workdir: o.Dir}, nil
}

// newWorktree makes the session's worktree of the checkout at o.Dir and
// records its owner and origin in the worktree's own git directory.
func newWorktree(ctx context.Context, o ClaimOptions) (Checkout, error) {
	if o.WorktreeDir == "" {
		return Checkout{}, errors.New("machine: another session writes the checkout, and this host keeps no worktrees")
	}
	top, err := git(ctx, o.Dir, o.Environ, "rev-parse", "--show-toplevel")
	if err != nil {
		return Checkout{}, err
	}
	base, err := git(ctx, o.Dir, o.Environ, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return Checkout{}, err
	}
	if base == "HEAD" {
		// A detached checkout has no branch; its commit is the base.
		if base, err = git(ctx, o.Dir, o.Environ, "rev-parse", "HEAD"); err != nil {
			return Checkout{}, err
		}
	}
	dir := filepath.Join(o.WorktreeDir, o.Session)
	branch := "agents/" + o.Agent + "/" + o.Session
	if err := os.MkdirAll(o.WorktreeDir, 0o755); err != nil {
		return Checkout{}, fmt.Errorf("machine: create the worktree directory: %w", err)
	}
	if _, err := git(ctx, top, o.Environ, "worktree", "add", "-q", "-b", branch, dir, "HEAD"); err != nil {
		return Checkout{}, err
	}
	wtGit, err := git(ctx, dir, o.Environ, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return Checkout{}, err
	}
	record, err := json.Marshal(worktree{Checkout: top, Base: base})
	if err != nil {
		return Checkout{}, err
	}
	if err := errors.Join(writeFile(filepath.Join(wtGit, ownerFile), o.Session), writeFile(filepath.Join(wtGit, worktreeFile), string(record))); err != nil {
		return Checkout{}, err
	}
	return Checkout{Workdir: dir, Branch: branch}, nil
}

// ReleaseCheckout gives back the working directory a session claimed, at
// the session's end (spec 009). A checkout written in place loses its
// owner file. A session worktree with no uncommitted change whose branch
// is pushed (its upstream contains its head) or merged (its head is an
// ancestor of the base it was made from) is removed with git worktree
// remove, keeping the branch; any other worktree stays. It reports
// whether a worktree was removed. A directory the session does not own
// is left alone.
func ReleaseCheckout(ctx context.Context, workdir, session string, environ []string) (bool, error) {
	gitDir, ok, err := gitOutput(ctx, workdir, environ, "rev-parse", "--absolute-git-dir")
	if err != nil || !ok {
		return false, err
	}
	owner, err := readTrimmed(filepath.Join(gitDir, ownerFile))
	if err != nil || owner != session {
		return false, err
	}
	raw, err := os.ReadFile(filepath.Join(gitDir, worktreeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return false, removeFile(filepath.Join(gitDir, ownerFile))
	}
	if err != nil {
		return false, fmt.Errorf("machine: read the worktree record: %w", err)
	}
	var wt worktree
	if err := json.Unmarshal(raw, &wt); err != nil {
		return false, fmt.Errorf("machine: the worktree record of %s: %w", workdir, err)
	}
	status, _, err := gitOutput(ctx, workdir, environ, "status", "--porcelain")
	if err != nil || status != "" {
		return false, err
	}
	_, merged, err := gitOutput(ctx, workdir, environ, "merge-base", "--is-ancestor", "HEAD", wt.Base)
	if err != nil {
		return false, err
	}
	pushed := false
	if _, upstream, err := gitOutput(ctx, workdir, environ, "rev-parse", "--verify", "-q", "@{upstream}"); err != nil {
		return false, err
	} else if upstream {
		if _, pushed, err = gitOutput(ctx, workdir, environ, "merge-base", "--is-ancestor", "HEAD", "@{upstream}"); err != nil {
			return false, err
		}
	}
	if !merged && !pushed {
		return false, nil
	}
	if _, err := git(ctx, wt.Checkout, environ, "worktree", "remove", workdir); err != nil {
		return false, err
	}
	return true, nil
}

// gitOutput runs git in dir and returns its trimmed standard output, and
// whether it exited 0. A git that is not installed is no error and no
// success, since a host without git writes in place; a git that could not
// start for another reason is an error.
func gitOutput(ctx context.Context, dir string, environ []string, args ...string) (string, bool, error) {
	out, _, ok, err := runGit(ctx, dir, environ, args...)
	return out, ok, err
}

// git runs a git command that must succeed and returns its trimmed
// standard output; a failure carries git's own message.
func git(ctx context.Context, dir string, environ []string, args ...string) (string, error) {
	out, errOut, ok, err := runGit(ctx, dir, environ, args...)
	if err == nil && !ok {
		err = fmt.Errorf("machine: git %s: %s", strings.Join(args, " "), cmp.Or(errOut, "failed"))
	}
	return out, err
}

func runGit(ctx context.Context, dir string, environ []string, args ...string) (out, errOut string, ok bool, err error) {
	path, err := exec.LookPath("git")
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("machine: find git: %w", err)
	}
	cmd := exec.CommandContext(ctx, path, append([]string{"-C", dir}, args...)...)
	cmd.Env = environ
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	out, errOut = strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
	if _, exited := errors.AsType[*exec.ExitError](err); exited {
		return out, errOut, false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("machine: git %s: %w", strings.Join(args, " "), err)
	}
	return out, errOut, true, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("machine: read %s: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("machine: create %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		return fmt.Errorf("machine: write %s: %w", path, err)
	}
	return nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("machine: remove %s: %w", path, err)
	}
	return nil
}
