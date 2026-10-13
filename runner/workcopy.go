// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// restoreCopy writes the working directory of m, a machine made new, from
// c, the copy the session keeps of it in its folder of a files store, and
// then what its log holds that the copy does not (spec 073), and returns
// the record of both. A file the copy holds that the folder no longer has
// is the log's when the log gives one, and is omitted otherwise. It
// returns nil when neither gives a file, and an error when the files could
// not be written, in which case the machine holds none or some of them.
func (r *Runner) restoreCopy(ctx context.Context, s session.Session, m machine.Machine, evs []session.Event, files harness.Files, c session.WorkCopy) (*session.Restore, error) {
	wctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()
	cp := &tools.Copy{Covers: c.Covers, Files: c.Files, Removed: c.Removed, Read: func(p string) ([]byte, error) {
		return readCopied(wctx, files, p)
	}}
	blob := func(d session.Digest) ([]byte, error) { return r.blob(wctx, s.ID, evs, files, d) }
	rb := tools.RebuildOver(evs, blob, cp)
	var gone, missing []string
	if err := r.writeAll(wctx, m, rb.Files, r.opener(wctx, s.ID, evs, files, &gone, &missing)); err != nil {
		return nil, fmt.Errorf("runner: write the working directory from its copy: %w", err)
	}
	written := rb.Files
	omitted := slices.Clone(rb.Omitted)
	if len(gone) > 0 {
		// The log's file for each path the folder no longer held, where
		// the log has one of its own.
		without := maps.Clone(c.Files)
		for _, p := range gone {
			delete(without, p)
		}
		again := tools.RebuildOver(evs, blob, &tools.Copy{Covers: c.Covers, Files: without, Removed: c.Removed, Read: cp.Read})
		var instead []tools.RebuiltFile
		for _, p := range gone {
			i := slices.IndexFunc(again.Files, func(f tools.RebuiltFile) bool { return f.Path == p && !f.Copied })
			if i < 0 {
				omitted = append(omitted, session.OmittedFile{Path: p, Reason: session.OmittedCopy})
				continue
			}
			instead = append(instead, again.Files[i])
		}
		if err := r.writeAll(wctx, m, instead, r.opener(wctx, s.ID, evs, files, new([]string), &missing)); err != nil {
			return nil, fmt.Errorf("runner: write the working directory the log holds in place of its copy's: %w", err)
		}
		written = append(slices.DeleteFunc(slices.Clone(written), func(f tools.RebuiltFile) bool { return slices.Contains(gone, f.Path) }), instead...)
	}
	out := &session.Restore{Source: session.RestoreFromFolder, Commands: rb.Commands}
	cr := &session.RestoreCopy{Inherited: c.Inherited, Skipped: c.Skipped, SkippedFiles: c.SkippedFiles}
	if !c.Inherited {
		cr.Turn = c.Turn
	}
	slices.SortFunc(written, func(a, b tools.RebuiltFile) int { return strings.Compare(a.Path, b.Path) })
	for _, f := range written {
		switch {
		case !f.Copied && slices.Contains(missing, f.Path):
			omitted = append(omitted, session.OmittedFile{Path: f.Path, Reason: session.OmittedBlob})
			continue
		case f.Copied:
			cr.Files++
			cr.Bytes += f.Size
		default:
			out.Files++
			out.Bytes += f.Size
			if len(out.Paths) < session.MaxRestoreNames {
				out.Paths = append(out.Paths, f.Path)
			}
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
	if cr.Files == 0 && out.Files == 0 {
		return nil, nil
	}
	out.Copy = cr
	return out, nil
}

// readCopied reads the file of the copy at p whole, within the bytes one
// file of the copy holds: the base of an edit the log made after the copy.
func readCopied(ctx context.Context, files harness.Files, p string) ([]byte, error) {
	rc, _, err := openCopied(ctx, files, tools.RebuiltFile{Path: p})
	if err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(rc, session.MaxWorkFileBytes+1))
	if err := errors.Join(rerr, rc.Close()); err != nil {
		return nil, err
	}
	if len(b) > session.MaxWorkFileBytes {
		return nil, fmt.Errorf("runner: %s of the copy is past the %d bytes one file of it holds", p, session.MaxWorkFileBytes)
	}
	return b, nil
}
