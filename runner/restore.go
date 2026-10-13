// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// restoreRate is the slowest a rebuilt working directory's bytes are
// planned to reach a machine at, in bytes a second.
const restoreRate = 1 << 20

// restoreTimeout bounds the writing of a rebuilt working directory into
// a machine: the time the most bytes a rebuild holds take at
// restoreRate, so a machine that stops answering holds its open no
// longer.
const restoreTimeout = tools.MaxRebuiltBytes / restoreRate * time.Second

// copyTimeout bounds the writing of a working directory from the copy its
// session keeps in its folder, and then from its log (spec 073): the time
// the most bytes the copy and a rebuild hold take at restoreRate.
const copyTimeout = (session.MaxWorkBytes + tools.MaxRebuiltBytes) / restoreRate * time.Second

// rebuildNew rebuilds the working directory of a machine its open made
// new, when the session's log holds one (spec 065): a machine in the
// place of one the session lost, and a fork's first machine, whose
// files are the ones its copied log holds. first is whether the session
// records no machine of its own yet. A machine that does not say it is
// new is left as it is, since its directory may hold the session's own
// files, newer than the log's; and a session's own first machine has
// nothing before it.
func (r *Runner) rebuildNew(ctx context.Context, s session.Session, m machine.Machine, first bool, files harness.Files) (*session.Restore, error) {
	rec, ok := m.(machine.Recovering)
	if !ok || !rec.Created() {
		return nil, nil
	}
	if first && (s.Parent == nil || s.Parent.Seq == 0) {
		return nil, nil
	}
	evs, err := r.o.Store.Events(ctx, s.ID, 1, 0)
	if err != nil {
		return nil, err
	}
	return r.rebuild(ctx, s, m, evs, files)
}

// rebuild puts back into m, a machine made new for a session whose log
// holds files, what the log holds of its working directory (spec 065),
// and returns the record of it: the files written, the ones that may be
// out of date, the ones that could not be rebuilt, and how many commands
// ran since. It returns nil when the log holds no file to write, and an
// error when the files could not be written, in which case the machine
// holds none or some of them.
//
// A session whose log records a copy of its working directory in its
// folder of a files store is restored from the copy first, and from the
// log for what the copy does not hold (spec 073). A copy that cannot be
// written at all leaves the machine to the log alone, and the record says
// why.
func (r *Runner) rebuild(ctx context.Context, s session.Session, m machine.Machine, evs []session.Event, files harness.Files) (*session.Restore, error) {
	var failed error
	if c := session.CopyOf(evs); files != nil && c.Saved {
		out, err := r.restoreCopy(ctx, s, m, evs, files, c)
		if err == nil {
			return out, nil
		}
		failed = err
	}
	out, err := r.rebuildLog(ctx, s, m, evs, files)
	if failed != nil && err == nil {
		if out == nil {
			return nil, fmt.Errorf("runner: the copy of the working directory: %w", failed)
		}
		out.Copy = &session.RestoreCopy{Failed: failed.Error()}
	}
	return out, err
}

// rebuildLog is rebuild from the log alone (spec 065).
func (r *Runner) rebuildLog(ctx context.Context, s session.Session, m machine.Machine, evs []session.Event, files harness.Files) (*session.Restore, error) {
	blob := func(d session.Digest) ([]byte, error) { return r.blob(ctx, s.ID, evs, files, d) }
	rb := tools.Rebuild(evs, blob)
	if len(rb.Files) == 0 {
		return nil, nil
	}
	wctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	// A kept file's bytes are read from the store as it is written, one
	// at a time; one whose blob is gone is omitted and the rest go on.
	var missing []string
	open := r.opener(wctx, s.ID, evs, files, nil, &missing)
	if err := r.writeAll(wctx, m, rb.Files, open); err != nil {
		return nil, fmt.Errorf("runner: write the working directory the log holds: %w", err)
	}
	out := &session.Restore{Source: session.RestoreFromLog, Commands: rb.Commands}
	omitted := rb.Omitted
	for _, f := range rb.Files {
		if slices.Contains(missing, f.Path) {
			omitted = append(omitted, session.OmittedFile{Path: f.Path, Reason: session.OmittedBlob})
			continue
		}
		out.Files++
		out.Bytes += f.Size
		if len(out.Paths) < session.MaxRestoreNames {
			out.Paths = append(out.Paths, f.Path)
		}
		if f.Unverified {
			out.Unverified++
			if len(out.UnverifiedPaths) < session.MaxRestoreNames {
				out.UnverifiedPaths = append(out.UnverifiedPaths, f.Path)
			}
		}
	}
	slices.SortFunc(omitted, func(a, b session.OmittedFile) int { return strings.Compare(a.Path, b.Path) })
	out.Omitted = len(omitted)
	out.OmittedFiles = omitted[:min(len(omitted), session.MaxRestoreNames)]
	if out.Files == 0 {
		return nil, nil
	}
	return out, nil
}

// blob reads a blob of the session whole, at most the bytes a rebuild
// holds: a file its log, evs, names by ref from files, the session's
// folder of a files store (spec 068), and any other from its store. A
// file of a folder this runner does not reach is not found.
func (r *Runner) blob(ctx context.Context, id string, evs []session.Event, files harness.Files, d session.Digest) ([]byte, error) {
	var rc io.ReadCloser
	var err error
	ref, kept := session.FindFile(evs, d)
	switch {
	case kept && files == nil:
		return nil, fmt.Errorf("%w: %s is in the session's folder of a files store, which this runner does not reach", session.ErrNotFound, ref)
	case kept:
		if rc, _, err = files.Open(ctx, ref); errors.Is(err, harness.ErrFileNotFound) {
			err = fmt.Errorf("%w: %w", session.ErrNotFound, err)
		}
	default:
		rc, err = r.o.Store.Blob(ctx, id, d)
	}
	if err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(rc, tools.MaxRebuiltBytes+1))
	if err := errors.Join(rerr, rc.Close()); err != nil {
		return nil, err
	}
	if len(b) > tools.MaxRebuiltBytes {
		return nil, fmt.Errorf("runner: the blob %s is past the %d bytes a rebuild holds", d, tools.MaxRebuiltBytes)
	}
	return b, nil
}

// opener is how a rebuilt file's bytes are read as it is written: its
// content; a kept file's blob, from the store or the session's folder,
// one that is gone recorded in missing and skipped; and, with copied set,
// a copied file of the session's folder, read from work/<path> (spec
// 073), one the folder no longer holds recorded in copied and skipped.
// Each answers the bytes as a stream and their length.
func (r *Runner) opener(ctx context.Context, id string, evs []session.Event, files harness.Files, copied *[]string, missing *[]string) func(tools.RebuiltFile) (io.ReadCloser, int64, bool, error) {
	return func(f tools.RebuiltFile) (io.ReadCloser, int64, bool, error) {
		switch {
		case f.Copied:
			rc, n, err := openCopied(ctx, files, f)
			if errors.Is(err, harness.ErrFileNotFound) {
				*copied = append(*copied, f.Path)
				return nil, 0, false, nil
			}
			return rc, n, err == nil, err
		case f.Blob == "":
			return io.NopCloser(bytes.NewReader(f.Content)), int64(len(f.Content)), true, nil
		}
		b, err := r.blob(ctx, id, evs, files, f.Blob)
		if errors.Is(err, session.ErrNotFound) {
			*missing = append(*missing, f.Path)
			return nil, 0, false, nil
		}
		if err != nil {
			return nil, 0, false, err
		}
		return io.NopCloser(bytes.NewReader(b)), int64(len(b)), true, nil
	}
}

// openCopied opens the file of the copy f names in the session's folder,
// with its length: the one the files store answers, or, when it answers
// none, the file read whole within the bytes one file of the copy holds.
func openCopied(ctx context.Context, files harness.Files, f tools.RebuiltFile) (io.ReadCloser, int64, error) {
	rc, n, err := files.Open(ctx, session.WorkRef(f.Path))
	if err != nil || n >= 0 {
		return rc, n, err
	}
	b, rerr := io.ReadAll(io.LimitReader(rc, session.MaxWorkFileBytes+1))
	if err := errors.Join(rerr, rc.Close()); err != nil {
		return nil, 0, err
	}
	if len(b) > session.MaxWorkFileBytes {
		return nil, 0, fmt.Errorf("runner: %s of the copy is past the %d bytes one file of it holds", f.Path, session.MaxWorkFileBytes)
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

// writeAll writes files into m: in one archive where m takes one, and
// file by file otherwise.
func (r *Runner) writeAll(ctx context.Context, m machine.Machine, files []tools.RebuiltFile, open func(tools.RebuiltFile) (io.ReadCloser, int64, bool, error)) error {
	if im, ok := m.(machine.Importer); ok {
		return importFiles(ctx, im, m.Info().Workdir, files, open, r.o.Clock())
	}
	return writeFiles(ctx, m, files, open)
}

// importFiles writes the files below dir in one archive, which the
// machine extracts as it arrives: each file's directories first, then
// the files, so the archive is never held whole. Every entry is dated
// now.
func importFiles(ctx context.Context, im machine.Importer, dir string, files []tools.RebuiltFile, open func(tools.RebuiltFile) (io.ReadCloser, int64, bool, error), now time.Time) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := writeTar(pw, files, open, now)
		done <- err
		// The reader ends with the archive, or with why it could not be
		// written; CloseWithError never fails.
		_ = pw.CloseWithError(err)
	}()
	ierr := im.ImportTar(ctx, dir, pr)
	// A machine that stopped reading leaves the writer blocked; closing
	// the read side releases it. Close never fails on a pipe.
	_ = pr.Close()
	werr := <-done
	if ierr != nil {
		return ierr
	}
	if werr != nil && !errors.Is(werr, io.ErrClosedPipe) {
		return werr
	}
	return nil
}

// modeOf is the permission bits a rebuilt file is written with: its own,
// the copy's, or 0644.
func modeOf(f tools.RebuiltFile) int64 {
	if f.Mode != 0 {
		return int64(f.Mode)
	}
	return 0o644
}

// writeTar writes the directories of files and then the files to w as a
// tar archive of paths under the working directory, each dated now, the
// time it is written back: a header with no time extracts as 1970, a date
// tools refuse, as Python's zipfile does one before 1980.
func writeTar(w io.Writer, files []tools.RebuiltFile, open func(tools.RebuiltFile) (io.ReadCloser, int64, bool, error), now time.Time) error {
	tw := tar.NewWriter(w)
	made := map[string]bool{}
	var dirs []string
	for _, f := range files {
		for d := path.Dir(f.Path); d != "." && !made[d]; d = path.Dir(d) {
			made[d] = true
			dirs = append(dirs, d)
		}
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755, ModTime: now}); err != nil {
			return err
		}
	}
	for _, f := range files {
		rc, n, ok, err := open(f)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		err = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: modeOf(f), Size: n, ModTime: now})
		if err == nil {
			_, err = io.CopyN(tw, rc, n)
		}
		if err := errors.Join(err, rc.Close()); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return tw.Close()
}

// writeFiles writes the files one by one, for a machine that takes no
// archive.
func writeFiles(ctx context.Context, m machine.Machine, files []tools.RebuiltFile, open func(tools.RebuiltFile) (io.ReadCloser, int64, bool, error)) error {
	for _, f := range files {
		rc, n, ok, err := open(f)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		err = m.WriteFile(ctx, f.Path, io.LimitReader(rc, n), fs.FileMode(modeOf(f)))
		if err := errors.Join(err, rc.Close()); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}
