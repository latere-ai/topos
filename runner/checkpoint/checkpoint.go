// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package checkpoint takes and restores the per-turn checkpoints of spec
// 034: a git commit of the working directory's tree, written through a
// temporary index so the person's index, HEAD and branches are never
// touched. Every git command runs on the session's machine.
package checkpoint

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"strconv"
	"strings"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// maxFileBytes is the size past which a file is left out of a checkpoint
// and named in its Topos-Excluded trailer: 100 MiB.
var maxFileBytes = 100 << 20

// excluded are the credential patterns of spec 009, as pathspecs no
// checkpoint takes.
var excluded = []string{
	":(exclude,glob)**/.env", ":(exclude,glob)**/.env.*",
	":(exclude,glob)**/*.pem", ":(exclude,glob)**/*.key", ":(exclude,glob)**/*.p12", ":(exclude,glob)**/*.pfx",
	":(exclude,glob)**/id_rsa*", ":(exclude,glob)**/id_ecdsa*", ":(exclude,glob)**/id_ed25519*",
}

// identity is the author and committer of every checkpoint: the runner's
// bookkeeping, never a person's or the agent's commit.
var identity = map[string]string{
	"GIT_AUTHOR_NAME": "Topos", "GIT_AUTHOR_EMAIL": "checkpoint@topos.invalid",
	"GIT_COMMITTER_NAME": "Topos", "GIT_COMMITTER_EMAIL": "checkpoint@topos.invalid",
}

// ErrNoGit is a machine without git, which keeps no checkpoints.
var ErrNoGit = errors.New("checkpoint: git is not available on the machine")

// ErrNoRepository is a working directory that is not a checkout, with no
// session repository to keep its checkpoints in.
var ErrNoRepository = errors.New("checkpoint: the working directory is not a checkout and no session repository is set")

// The error codes of spec 034.
const (
	CodeRewindNotIdle = "rewind_not_idle"
	CodeMissing       = "checkpoint_missing"
)

// Checkpointer takes the checkpoints of one working directory.
type Checkpointer struct {
	Machine   machine.Machine
	SessionID string
	AgentID   string
	// Thread is the thread whose worktree this is; empty is the session's
	// own working directory.
	Thread string
	// SessionRepo is the bare repository that holds the objects when the
	// working directory is not a checkout.
	SessionRepo string
}

// Ref is the ref of a turn's checkpoint.
func (c *Checkpointer) Ref(turn int) string {
	if c.Thread != "" {
		return fmt.Sprintf("refs/topos/checkpoints/%s/threads/%s/%d", c.SessionID, c.Thread, turn)
	}
	return fmt.Sprintf("refs/topos/checkpoints/%s/%d", c.SessionID, turn)
}

// git runs a git command in the working directory with env added and
// returns its trimmed output.
func (c *Checkpointer) git(ctx context.Context, env map[string]string, args string) (string, error) {
	res, err := c.Machine.Exec(ctx, machine.ExecRequest{Command: "git " + args, Env: env, Timeout: 2 * time.Minute})
	if err != nil {
		return "", fmt.Errorf("checkpoint: git %s: %w", firstWord(args), err)
	}
	if res.ExitCode == 127 {
		return "", ErrNoGit
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("checkpoint: git %s exited %d: %s", firstWord(args), res.ExitCode, strings.TrimSpace(string(res.Output)))
	}
	return strings.TrimSpace(string(res.Output)), nil
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}

// quote renders an argument for the machine's shell.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// repo returns the environment that points git at the objects: nothing
// in a checkout, the session repository otherwise, created on first use.
func (c *Checkpointer) repo(ctx context.Context) (map[string]string, error) {
	env := maps.Clone(identity)
	if _, err := c.git(ctx, nil, "rev-parse --is-inside-work-tree"); err == nil {
		return env, nil
	} else if errors.Is(err, ErrNoGit) {
		return nil, err
	}
	if c.SessionRepo == "" {
		return nil, ErrNoRepository
	}
	if _, err := c.git(ctx, nil, "init --quiet --bare "+quote(c.SessionRepo)); err != nil {
		return nil, err
	}
	env["GIT_DIR"] = c.SessionRepo
	env["GIT_WORK_TREE"] = c.Machine.Info().Workdir
	return env, nil
}

// index returns a fresh temporary index file in the spill directory.
func (c *Checkpointer) index() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("checkpoint: temporary index name: %w", err)
	}
	return path.Join(c.Machine.SpillDir(), "checkpoint-"+hex.EncodeToString(b[:])+".index"), nil
}

// Take commits the working directory's tree as the checkpoint of turn,
// chained to previous (empty for the first), and points the turn's ref
// at it. A turn that changed nothing reuses the previous tree.
func (c *Checkpointer) Take(ctx context.Context, turn int, previous string) (session.CheckpointRef, error) {
	ref := c.Ref(turn)
	commit, _, err := c.commit(ctx, ref, fmt.Sprintf("topos checkpoint %s turn %d", c.SessionID, turn), previous, false)
	if err != nil {
		return session.CheckpointRef{}, err
	}
	return session.CheckpointRef{Ref: ref, Commit: commit}, nil
}

// Save checkpoints the working directory before a rewind replaces it,
// under refs/topos/checkpoints/<session>/saved/<n>. When the tree is the
// previous checkpoint's, it records nothing and reports false: the state
// is already kept.
func (c *Checkpointer) Save(ctx context.Context, n int, previous string) (session.CheckpointRef, bool, error) {
	ref := fmt.Sprintf("refs/topos/checkpoints/%s/saved/%d", c.SessionID, n)
	commit, made, err := c.commit(ctx, ref, fmt.Sprintf("topos checkpoint %s before rewind %d", c.SessionID, n), previous, true)
	if err != nil || !made {
		return session.CheckpointRef{}, false, err
	}
	return session.CheckpointRef{Ref: ref, Commit: commit}, true, nil
}

// commit writes the working directory's tree through a temporary index
// and commits it under ref. With skipSame, a tree equal to previous's is
// not committed.
func (c *Checkpointer) commit(ctx context.Context, ref, subject, previous string, skipSame bool) (_ string, _ bool, err error) {
	env, err := c.repo(ctx)
	if err != nil {
		return "", false, err
	}
	idx, err := c.index()
	if err != nil {
		return "", false, err
	}
	env["GIT_INDEX_FILE"] = idx
	defer func() {
		if rerr := c.Machine.Remove(context.WithoutCancel(ctx), idx); rerr != nil && err == nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
	}()
	big, err := c.large(ctx)
	if err != nil {
		return "", false, err
	}
	specs := append([]string{"."}, excluded...)
	for _, b := range big {
		specs = append(specs, ":(exclude,literal)"+b)
	}
	quoted := make([]string, len(specs))
	for i, s := range specs {
		quoted[i] = quote(s)
	}
	if _, err := c.git(ctx, env, "add -A -- "+strings.Join(quoted, " ")); err != nil {
		return "", false, err
	}
	tree, err := c.git(ctx, env, "write-tree")
	if err != nil {
		return "", false, err
	}
	if skipSame && previous != "" {
		if prev, err := c.git(ctx, env, "rev-parse "+previous+"^{tree}"); err == nil && prev == tree {
			return previous, false, nil
		}
	}
	var msg strings.Builder
	fmt.Fprintf(&msg, "%s\n\nTopos-Session: %s\nTopos-Agent: %s", subject, c.SessionID, c.AgentID)
	for _, b := range big {
		msg.WriteString("\nTopos-Excluded: " + b)
	}
	args := "commit-tree " + tree + " -m " + quote(msg.String())
	if previous != "" {
		args += " -p " + previous
	}
	commit, err := c.git(ctx, env, args)
	if err != nil {
		return "", false, err
	}
	if _, err := c.git(ctx, env, "update-ref "+ref+" "+commit); err != nil {
		return "", false, err
	}
	return commit, true, nil
}

// large lists the files git would take that pass maxFileBytes.
func (c *Checkpointer) large(ctx context.Context) ([]string, error) {
	res, err := c.Machine.Exec(ctx, machine.ExecRequest{
		Command: "find . -type f -size +" + strconv.Itoa(maxFileBytes/1024) + "k -not -path './.git/*' 2>/dev/null",
		Timeout: time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("checkpoint: find large files: %w", err)
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(res.Output)), "\n") {
		if line = strings.TrimPrefix(line, "./"); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// Has reports whether the checkpoints of this working directory hold
// commit. A machine without git, or a directory with no repository to
// hold checkpoints, holds none and answers that error.
func (c *Checkpointer) Has(ctx context.Context, commit string) (bool, error) {
	env, err := c.repo(ctx)
	if err != nil {
		return false, err
	}
	return c.exists(ctx, env, commit)
}

// exists reports whether the repository env points at holds commit.
func (c *Checkpointer) exists(ctx context.Context, env map[string]string, commit string) (bool, error) {
	res, err := c.Machine.Exec(ctx, machine.ExecRequest{Command: "git cat-file -e " + quote(commit+"^{commit}"), Env: env, Timeout: time.Minute})
	if err != nil {
		return false, fmt.Errorf("checkpoint: git cat-file: %w", err)
	}
	return res.ExitCode == 0, nil
}

// Adopt records commit, a checkpoint another session took, as this
// working directory's checkpoint of turn, as a fork does with the turn it
// forks at (spec 017). The commit is taken from this directory's own
// repository, or else fetched from the bare repository from, the other
// session's, by its ref there. It reports false, recording nothing, when
// neither holds the commit.
func (c *Checkpointer) Adopt(ctx context.Context, turn int, commit, from, ref string) (session.CheckpointRef, bool, error) {
	env, err := c.repo(ctx)
	if err != nil {
		return session.CheckpointRef{}, false, err
	}
	have, err := c.exists(ctx, env, commit)
	if err != nil {
		return session.CheckpointRef{}, false, err
	}
	if !have && from != "" {
		if there, err := c.exists(ctx, map[string]string{"GIT_DIR": from}, commit); err != nil || !there {
			return session.CheckpointRef{}, false, err
		}
		if _, err := c.git(ctx, env, "fetch --no-tags --quiet "+quote(from)+" "+quote(ref)); err != nil {
			return session.CheckpointRef{}, false, err
		}
		// The ref may have moved past the commit since; then the commit
		// is not had.
		if have, err = c.exists(ctx, env, commit); err != nil {
			return session.CheckpointRef{}, false, err
		}
	}
	if !have {
		return session.CheckpointRef{}, false, nil
	}
	own := c.Ref(turn)
	if _, err := c.git(ctx, env, "update-ref "+own+" "+commit); err != nil {
		return session.CheckpointRef{}, false, err
	}
	return session.CheckpointRef{Ref: own, Commit: commit}, true, nil
}

// Restore makes the working directory hold the files of commit: files
// the commit lacks that the saved state had are removed, the commit's
// files are written, and ignored and deny-listed files are untouched
// because neither tree holds them.
func (c *Checkpointer) Restore(ctx context.Context, commit, saved string) (err error) {
	env, err := c.repo(ctx)
	if err != nil {
		return err
	}
	if _, err := c.git(ctx, env, "cat-file -e "+commit+"^{commit}"); err != nil {
		return fmt.Errorf("%s: %w", CodeMissing, err)
	}
	if saved != "" {
		gone, err := c.git(ctx, env, "diff --name-only --no-renames --diff-filter=D "+saved+" "+commit)
		if err != nil {
			return err
		}
		for p := range strings.SplitSeq(gone, "\n") {
			if p == "" {
				continue
			}
			if err := c.Machine.Remove(ctx, path.Join(c.Machine.Info().Workdir, p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("checkpoint: remove %s: %w", p, err)
			}
		}
	}
	idx, err := c.index()
	if err != nil {
		return err
	}
	env["GIT_INDEX_FILE"] = idx
	defer func() {
		if rerr := c.Machine.Remove(context.WithoutCancel(ctx), idx); rerr != nil && err == nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
	}()
	if _, err := c.git(ctx, env, "read-tree "+commit); err != nil {
		return err
	}
	_, err = c.git(ctx, env, "checkout-index -a -f")
	return err
}
