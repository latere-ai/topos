// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"slices"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/runner/checkpoint"
	"latere.ai/x/topos/session"
)

// forkCheckpoint is the checkpoint a fork's copied log ends at: the one
// the fork point's session.status of the session's own thread names, nil
// when that turn kept none.
func forkCheckpoint(s session.Session, evs []session.Event) (*session.CheckpointRef, int) {
	if s.Parent == nil {
		return nil, 0
	}
	for _, e := range slices.Backward(evs) {
		if !s.Copied(e) || e.Type != session.TypeSessionStatus || e.Thread != "" || e.Redacted() {
			continue
		}
		var p session.SessionStatus
		if e.Decode(&p) == nil && p.Checkpoint != nil {
			return p.Checkpoint, e.Turn
		}
	}
	return nil, 0
}

// restoreFork puts the files of the turn a fork forked at into its first
// machine (spec 017): the fork point's checkpoint, taken from the working
// directory's own repository, or fetched from the parent's session
// repository under CheckpointDir, recorded as the fork's checkpoint of
// that turn and checked out into the working directory. A checkpoint
// neither holds, a machine without git and a directory with no
// repository restore nothing and answer nil, so the fork starts on the
// files its repositories give it; a hosted sandbox's checkpoints left
// with its sandbox until the runner pushes them to the git host
// (spec 034).
func (r *Runner) restoreFork(ctx context.Context, s session.Session, m machine.Machine) (*session.CheckpointRef, error) {
	evs, err := r.o.Store.Events(ctx, s.ID, 1, int(s.Parent.Seq))
	if err != nil {
		return nil, err
	}
	target, turn := forkCheckpoint(s, evs)
	if target == nil {
		return nil, nil
	}
	cp := r.checkpointerOn(m, s)
	from := ""
	if r.o.CheckpointDir != "" {
		from = r.sessionRepo(s.Parent.SessionID)
	}
	ref, ok, err := cp.Adopt(ctx, turn, target.Commit, from, target.Ref)
	switch {
	case errors.Is(err, checkpoint.ErrNoGit), errors.Is(err, checkpoint.ErrNoRepository), err == nil && !ok:
		return nil, nil
	case err != nil:
		return nil, &machine.OpenError{Code: checkpoint.CodeMissing, Err: err}
	}
	if err := cp.Restore(ctx, ref.Commit, ""); err != nil {
		return nil, &machine.OpenError{Code: checkpoint.CodeMissing, Err: err}
	}
	return &ref, nil
}

// chainable is the previous checkpoint a fork's turn chains its own to:
// the fork point's checkpoint while it is still the last one, and only
// when the working directory's repository holds it, which a restore
// made so; otherwise the fork's first checkpoint starts a chain of its
// own. Any other session's previous is its own.
func (r *Runner) chainable(ctx context.Context, cp *checkpoint.Checkpointer, s session.Session, evs []session.Event, previous string) (string, error) {
	base, _ := forkCheckpoint(s, evs)
	if previous == "" || base == nil || previous != base.Commit {
		return previous, nil
	}
	have, err := cp.Has(ctx, previous)
	switch {
	case errors.Is(err, checkpoint.ErrNoGit), errors.Is(err, checkpoint.ErrNoRepository):
		return "", nil
	case err != nil || !have:
		return "", err
	}
	return previous, nil
}
