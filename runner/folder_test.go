// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/session"
)

// folderFiles is a session's folder of a files store that answers reads
// from a map.
type folderFiles map[string][]byte

func (f folderFiles) Put(context.Context, string, string, int64, io.Reader) (session.Digest, error) {
	return "", errors.New("folderFiles writes nothing")
}

func (f folderFiles) Open(_ context.Context, ref string) (io.ReadCloser, int64, error) {
	b, ok := f[ref]
	if !ok {
		return nil, 0, harness.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f folderFiles) CopyFolder(context.Context, session.Folder, string) error {
	return errors.New("folderFiles copies nothing")
}

// TestARebuildReadsKeptFilesFromTheFolder: a working directory rebuilt
// from the log reads a kept file whose entry names a ref from the
// session's folder of a files store; one the folder no longer holds, and
// every one on a runner that reaches no files store, is omitted as a
// file whose bytes are gone (spec 068).
func TestARebuildReadsKeptFilesFromTheFolder(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	work := fresh(t)
	m, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(t.TempDir(), "spill")})
	if err != nil {
		t.Fatal(err)
	}
	report, gone := []byte("%PDF-1.7 report"), []byte("%PDF gone")
	attached, err := session.NewEvent(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: "host", ID: "m1", Workdir: "/workspace"}, Reason: session.MachineAttached}, t0)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := session.NewEvent(session.TypeFilesKept, session.FilesKept{Message: "evt_x", Files: []session.KeptFile{
		{Path: "out/report.pdf", Resolved: "/workspace/out/report.pdf", Blob: session.DigestOf(report), Size: int64(len(report)), Ref: "results/1/out/report.pdf"},
		{Path: "gone.pdf", Resolved: "/workspace/gone.pdf", Blob: session.DigestOf(gone), Size: int64(len(gone)), Ref: "results/1/gone.pdf"},
	}, Skipped: []session.SkippedFile{}}, t0)
	if err != nil {
		t.Fatal(err)
	}
	evs := []session.Event{attached, kept}
	files := folderFiles{"results/1/out/report.pdf": report}
	r, err := f.r.rebuild(ctx, f.s, m, evs, files)
	if err != nil {
		t.Fatal(err)
	}
	if r == nil || r.Files != 1 || r.Omitted != 1 || r.OmittedFiles[0] != (session.OmittedFile{Path: "gone.pdf", Reason: session.OmittedBlob}) {
		t.Fatalf("the rebuild %+v", r)
	}
	if got, err := os.ReadFile(filepath.Join(work, "out", "report.pdf")); err != nil || !bytes.Equal(got, report) {
		t.Fatalf("the rebuilt file %q, %v", got, err)
	}
	none, err := f.r.rebuild(ctx, f.s, m, evs, nil)
	if err != nil || none != nil {
		t.Fatalf("a rebuild on a runner with no files store: %+v, %v", none, err)
	}
}
