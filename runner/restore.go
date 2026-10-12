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
	"path"
	"slices"
	"strings"
	"time"

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

// rebuildNew rebuilds the working directory of a machine its open made
// new, when the session's log holds one (spec 065): a machine in the
// place of one the session lost, and a fork's first machine, whose
// files are the ones its copied log holds. first is whether the session
// records no machine of its own yet. A machine that does not say it is
// new is left as it is, since its directory may hold the session's own
// files, newer than the log's; and a session's own first machine has
// nothing before it.
func (r *Runner) rebuildNew(ctx context.Context, s session.Session, m machine.Machine, first bool) (*session.Restore, error) {
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
	return r.rebuild(ctx, s, m, evs)
}

// rebuild puts back into m, a machine made new for a session whose log
// holds files, what the log holds of its working directory (spec 065),
// and returns the record of it: the files written, the ones that may be
// out of date, the ones that could not be rebuilt, and how many commands
// ran since. It returns nil when the log holds no file to write, and an
// error when the files could not be written, in which case the machine
// holds none or some of them.
func (r *Runner) rebuild(ctx context.Context, s session.Session, m machine.Machine, evs []session.Event) (*session.Restore, error) {
	blob := func(d session.Digest) ([]byte, error) { return r.blob(ctx, s.ID, d) }
	rb := tools.Rebuild(evs, blob)
	if len(rb.Files) == 0 {
		return nil, nil
	}
	wctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	// A kept file's bytes are read from the store as it is written, one
	// at a time; one whose blob is gone is omitted and the rest go on.
	var missing []string
	open := func(f tools.RebuiltFile) ([]byte, bool, error) {
		if f.Blob == "" {
			return f.Content, true, nil
		}
		b, err := r.blob(wctx, s.ID, f.Blob)
		if errors.Is(err, session.ErrNotFound) {
			missing = append(missing, f.Path)
			return nil, false, nil
		}
		return b, err == nil, err
	}
	var err error
	if im, ok := m.(machine.Importer); ok {
		err = importFiles(wctx, im, m.Info().Workdir, rb.Files, open, r.o.Clock())
	} else {
		err = writeFiles(wctx, m, rb.Files, open)
	}
	if err != nil {
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
// holds.
func (r *Runner) blob(ctx context.Context, id string, d session.Digest) ([]byte, error) {
	rc, err := r.o.Store.Blob(ctx, id, d)
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

// importFiles writes the files below dir in one archive, which the
// machine extracts as it arrives: each file's directories first, then
// the files, so the archive is never held whole. Every entry is dated
// now.
func importFiles(ctx context.Context, im machine.Importer, dir string, files []tools.RebuiltFile, open func(tools.RebuiltFile) ([]byte, bool, error), now time.Time) error {
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

// writeTar writes the directories of files and then the files to w as a
// tar archive of paths under the working directory, each dated now, the
// time it is written back: a header with no time extracts as 1970, a date
// tools refuse, as Python's zipfile does one before 1980.
func writeTar(w io.Writer, files []tools.RebuiltFile, open func(tools.RebuiltFile) ([]byte, bool, error), now time.Time) error {
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
		b, ok, err := open(f)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Path, Mode: 0o644, Size: int64(len(b)), ModTime: now}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	return tw.Close()
}

// writeFiles writes the files one by one, for a machine that takes no
// archive.
func writeFiles(ctx context.Context, m machine.Machine, files []tools.RebuiltFile, open func(tools.RebuiltFile) ([]byte, bool, error)) error {
	for _, f := range files {
		b, ok, err := open(f)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := m.WriteFile(ctx, f.Path, bytes.NewReader(b), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}
