// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"time"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// workRate is the slowest a turn end's copy of the working directory is
// planned to read from the machine and write to the files store, in
// bytes a second.
const workRate = 1 << 20

// workTimeout bounds one turn end's copy of the working directory (spec
// 073): the time the most bytes a turn end writes take at workRate, and a
// minute for the walk and the requests of small files. What it has not
// written by then is left for a later turn end.
const workTimeout = session.MaxWorkTurnBytes/workRate*time.Second + time.Minute

// maxWalked is the most files a walk lists: four times the files the copy
// holds, so the copy's bounds are read past and a removed file is known
// as removed in all but a directory far larger than the copy.
const maxWalked = 4 * session.MaxWorkFiles

// SaveWork writes the working directory of m, a machine that walks it
// (machine.Walker), into files, the session's folder of a files store, under
// session.WorkDir (spec 073), against the copy log records, and answers
// the work.saved that records what it did, false when it did nothing:
// the directory's files equal the copy's, the machine was lost, or it
// does not walk.
// covers is the last sequence of log. Each new or changed file is
// written at its path in walk order within the turn's bounds, the rest
// deferred; each file the directory no longer holds, or that grew past
// the copy's bounds, is deleted. A walk the machine does not answer, and
// a write or a delete the files store refuses for anything but the
// file's size, stop the save and are recorded in its Failed; a save
// never fails otherwise. typeOf names a file's media type from its base
// name and its leading bytes; nil types every file
// application/octet-stream.
func SaveWork(ctx context.Context, m machine.Machine, files Files, log []session.Event, covers uint64, typeOf func(name string, head []byte) string) (session.WorkSaved, bool) {
	w, ok := m.(machine.Walker)
	if !ok {
		return session.WorkSaved{}, false
	}
	if typeOf == nil {
		typeOf = func(string, []byte) string { return "application/octet-stream" }
	}
	prior := session.CopyOf(log)
	out := session.WorkSaved{Covers: covers}
	walked, err := w.Walk(ctx, machine.WalkRequest{
		LeftOut: session.WorkLeftOut, MaxFiles: session.MaxWorkFiles, MaxBytes: session.MaxWorkBytes,
		MaxFileBytes: session.MaxWorkFileBytes, MaxListed: maxWalked,
	})
	switch {
	case errors.Is(err, machine.ErrLost):
		return session.WorkSaved{}, false
	case err != nil:
		out.Failed = &session.WorkFailure{Code: session.WorkMachineUnavailable, Detail: err.Error()}
		return out, true
	}
	s := saver{m: m, files: files, typeOf: typeOf, out: &out, held: maps.Clone(prior.Files)}
	listed := map[string]bool{}
	var changed []machine.WalkedFile
	for _, f := range walked.Files {
		listed[f.Path] = true
		old, kept := prior.Files[f.Path]
		switch {
		case f.Reason != "":
			s.skip(f)
			if kept || prior.Stale[f.Path] {
				s.gone = append(s.gone, f.Path)
			}
		case kept && old.SHA256 == f.SHA256 && old.Size == f.Size:
			if old.Mode != f.Mode {
				// The bytes are the copy's; the mode alone is recorded anew.
				s.record(session.WorkFile{Path: f.Path, Size: f.Size, Mode: f.Mode, SHA256: f.SHA256})
			}
		default:
			changed = append(changed, f)
		}
	}
	// A path the walk did not list is gone from the directory, unless the
	// walk stopped before the place it would have been listed at.
	unseen := func(p string) bool {
		if listed[p] {
			return false
		}
		return !walked.Cut || len(walked.Files) == 0 || machine.WalkOrder(p, walked.Files[len(walked.Files)-1].Path) < 0
	}
	for p := range prior.Files {
		if unseen(p) {
			s.gone = append(s.gone, p)
		}
	}
	for p := range prior.Stale {
		if unseen(p) {
			s.gone = append(s.gone, p)
		}
	}
	slices.SortFunc(s.gone, machine.WalkOrder)
	if s.remove(ctx) {
		s.write(ctx, changed)
	} else {
		s.unwritten(changed)
	}
	for _, f := range s.held {
		out.Held.Files++
		out.Held.Bytes += f.Size
	}
	if len(out.Written)+len(out.Removed)+len(out.Unsaved) == 0 && out.Deferred == 0 && out.Failed == nil &&
		out.Skipped == prior.Skipped && slices.Equal(out.SkippedFiles, prior.SkippedFiles) {
		return session.WorkSaved{}, false
	}
	return out, true
}

// saver is one turn end's save of the copy: the machine it reads, the
// files store it writes, the record it builds, the copy as it stands, and
// the paths whose files are deleted.
type saver struct {
	m      machine.Machine
	files  Files
	typeOf func(name string, head []byte) string
	out    *session.WorkSaved
	held   map[string]session.WorkFile
	gone   []string
	// files and bytes are what this turn end wrote.
	n     int
	bytes int64
}

// skip records a file the copy leaves out.
func (s *saver) skip(f machine.WalkedFile) {
	s.out.Skipped++
	if len(s.out.SkippedFiles) < session.MaxRestoreNames {
		reason := session.WorkLimit
		if f.Reason == machine.WalkTooLarge {
			reason = session.WorkTooLarge
		}
		s.out.SkippedFiles = append(s.out.SkippedFiles, session.OmittedFile{Path: f.Path, Reason: reason})
	}
}

// record records a file written into the copy.
func (s *saver) record(f session.WorkFile) {
	s.out.Written = append(s.out.Written, f)
	s.held[f.Path] = f
}

// stale records a path the copy no longer vouches for, whose older file
// the folder may hold.
func (s *saver) stale(p string) {
	if _, kept := s.held[p]; kept {
		s.out.Unsaved = append(s.out.Unsaved, p)
		delete(s.held, p)
	}
}

// fail records why the save stopped.
func (s *saver) fail(err error) {
	s.out.Failed = &session.WorkFailure{Code: session.WorkFilesUnavailable, Detail: err.Error()}
}

// remove deletes the files of the paths gone from the directory, and
// reports whether every delete was taken.
func (s *saver) remove(ctx context.Context) bool {
	for i, p := range s.gone {
		if err := s.files.Delete(ctx, session.WorkRef(p)); err != nil {
			s.fail(fmt.Errorf("delete %s: %w", p, err))
			for _, q := range s.gone[i:] {
				s.stale(q)
			}
			return false
		}
		delete(s.held, p)
		s.out.Removed = append(s.out.Removed, p)
	}
	return true
}

// unwritten records changed files no write was tried for.
func (s *saver) unwritten(changed []machine.WalkedFile) {
	for _, f := range changed {
		s.out.Deferred++
		s.stale(f.Path)
	}
}

// write writes the changed files in walk order within the turn's bounds,
// and leaves the rest for a later turn end. A file that changed under the
// write, or that the machine did not read, is left too; a write refused
// for its size skips the file; any other refusal stops the save.
func (s *saver) write(ctx context.Context, changed []machine.WalkedFile) {
	for i, f := range changed {
		if ctx.Err() != nil || s.n >= session.MaxWorkTurnFiles || s.bytes+f.Size > session.MaxWorkTurnBytes {
			s.unwritten(changed[i:])
			return
		}
		d, err := s.put(ctx, f)
		switch {
		case err == nil:
			s.n++
			s.bytes += f.Size
			s.record(session.WorkFile{Path: f.Path, Size: f.Size, Mode: f.Mode, SHA256: d.Hex()})
		case FileTooLarge(err):
			s.skip(machine.WalkedFile{Path: f.Path, Reason: machine.WalkTooLarge})
			s.stale(f.Path)
		case errors.Is(err, machine.ErrLost):
			s.unwritten(changed[i:])
			return
		case errors.Is(err, errUnread), errors.Is(err, ErrFileSizeChanged), ctx.Err() != nil:
			s.out.Deferred++
			s.stale(f.Path)
		default:
			s.fail(fmt.Errorf("write %s: %w", f.Path, err))
			s.stale(f.Path)
			s.unwritten(changed[i+1:])
			return
		}
	}
}

// errUnread is a file of the machine a save could not read.
var errUnread = errors.New("harness: the machine's file could not be read")

// put writes one file of the machine at its place in the copy, as it is
// read, with the media type its name and leading bytes give.
func (s *saver) put(ctx context.Context, f machine.WalkedFile) (session.Digest, error) {
	rc, err := s.m.ReadFile(ctx, f.Path)
	if err != nil {
		if errors.Is(err, machine.ErrLost) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", errUnread, err)
	}
	head := make([]byte, min(int64(tools.SniffBytes), f.Size))
	n, err := io.ReadFull(rc, head)
	if err != nil {
		return "", errors.Join(fmt.Errorf("%w: %w", errUnread, err), rc.Close())
	}
	media := s.typeOf(path.Base(f.Path), head[:n])
	d, err := s.files.Overwrite(ctx, session.WorkRef(f.Path), media, f.Size, io.MultiReader(bytes.NewReader(head[:n]), rc))
	cerr := rc.Close()
	switch {
	case err != nil:
		return "", errors.Join(err, cerr)
	case cerr != nil:
		// The file was written; a read that did not close cleanly may
		// not have been the file's, so a later turn end writes it again.
		return "", fmt.Errorf("%w: %w", errUnread, cerr)
	}
	return d, nil
}

// saveWork answers the work.saved of the turn's end (spec 073), which the
// turn's closing batch appends, false when the turn keeps no copy: the session's runner has
// no files store; the turn's machine is not open, says nothing of what
// its open made of it, or does not walk its working directory; or no call
// ran since the copy was last saved and the last save left nothing for
// later.
func (t *turn) saveWork(ctx context.Context) (session.Event, bool, error) {
	if t.h.c.Files == nil || t.thread != "" {
		return session.Event{}, false, nil
	}
	m := t.openMachine()
	if m == nil {
		return session.Event{}, false, nil
	}
	_, walks := m.(machine.Walker)
	_, recovering := m.(machine.Recovering)
	if !walks || !recovering {
		return session.Event{}, false, nil
	}
	evs := t.events()
	prior := session.CopyOf(evs)
	var last uint64
	called := false
	for _, e := range evs {
		last = max(last, e.Seq)
		called = called || (e.Type == session.TypeAgentToolUse && e.Seq > prior.Covers)
	}
	if !called && !prior.Pending {
		return session.Event{}, false, nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workTimeout)
	defer cancel()
	p, ok := SaveWork(sctx, m, t.h.c.Files, evs, last, t.h.c.MediaType)
	if !ok {
		return session.Event{}, false, nil
	}
	e, err := t.event(session.TypeWorkSaved, p)
	return e, err == nil, err
}
