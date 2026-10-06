// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
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
// directory's own repository, from the parent's session repository under
// CheckpointDir, or fetched by its id from the repository that kept it
// past the parent's machine when that repository is the fork's own
// (spec 035), recorded as the fork's checkpoint of that turn and checked
// out into the working directory. A fork point that kept no checkpoint
// restores nothing and answers nil; one whose checkpoint the runner cannot
// have or cannot check out answers why, and the fork starts on the files
// its repositories give it. A fork before its parent's opening message
// copied nothing, so it has no fork point to restore and starts fresh
// (spec 054).
func (r *Runner) restoreFork(ctx context.Context, s session.Session, m machine.Machine) (*session.CheckpointRef, error) {
	if s.Parent.Seq == 0 {
		return nil, nil
	}
	evs, err := r.o.Store.Events(ctx, s.ID, 1, int(s.Parent.Seq))
	if err != nil {
		return nil, err
	}
	target, turn := forkCheckpoint(s, evs)
	if target == nil {
		return nil, nil
	}
	missing := func(err error) error {
		return fmt.Errorf("the checkpoint %s of turn %d of %s: %w", target.Commit, turn, s.Parent.SessionID, err)
	}
	cp := r.checkpointerOn(m, s)
	from := ""
	if r.o.CheckpointDir != "" {
		from = r.sessionRepo(s.Parent.SessionID)
	}
	ref, ok, err := cp.Adopt(ctx, turn, target.Commit, from, target.Ref)
	if err == nil && !ok {
		// The copied log names the repository that kept the checkpoint;
		// only the fork's own repository is fetched from, so the log
		// cannot point the machine at another.
		switch {
		case target.Remote == "":
			return nil, missing(errors.New("it was kept on the machine of that session alone"))
		case target.Remote != cp.Remote:
			return nil, missing(fmt.Errorf("it was kept at %s, which is not this session's own repository", target.Remote))
		}
		if err := cp.Fetch(ctx, target.Commit, s.Parent.SessionID); err != nil {
			return nil, missing(err)
		}
		ref, ok, err = cp.Adopt(ctx, turn, target.Commit, "", "")
	}
	switch {
	case err != nil:
		return nil, missing(err)
	case !ok:
		return nil, missing(checkpoint.ErrNotKept)
	}
	if err := cp.Restore(ctx, ref.Commit, ""); err != nil {
		return nil, missing(err)
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
