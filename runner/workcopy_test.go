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
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness"
	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// liveFolder is a session's folder of a files store in memory, which a
// test can make refuse writes.
type liveFolder struct {
	mu    sync.Mutex
	files map[string][]byte
	// refuse fails every write, and away every read.
	refuse   error
	away     error
	deletes  []string
	writings []string
}

func newLiveFolder() *liveFolder { return &liveFolder{files: map[string][]byte{}} }

func (f *liveFolder) write(ref string, size int64, r io.Reader, create bool) (session.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return "", f.refuse
	}
	if _, taken := f.files[ref]; taken && create {
		return "", harness.ErrFileExists
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if int64(len(b)) != size {
		return "", harness.ErrFileSizeChanged
	}
	f.files[ref] = b
	f.writings = append(f.writings, ref)
	return session.DigestOf(b), nil
}

func (f *liveFolder) Put(_ context.Context, ref, _ string, size int64, r io.Reader) (session.Digest, error) {
	return f.write(ref, size, r, true)
}

func (f *liveFolder) Overwrite(_ context.Context, ref, _ string, size int64, r io.Reader) (session.Digest, error) {
	return f.write(ref, size, r, false)
}

func (f *liveFolder) Open(_ context.Context, ref string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.away != nil {
		return nil, 0, f.away
	}
	b, ok := f.files[ref]
	if !ok {
		return nil, 0, harness.ErrFileNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *liveFolder) Delete(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, ref)
	delete(f.files, ref)
	return nil
}

func (f *liveFolder) CopyFolder(context.Context, session.Folder, string) error {
	return errors.New("liveFolder copies nothing")
}

func (f *liveFolder) has(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[ref]
	return ok
}

// walkingImporter is a recovering machine that takes an archive and walks
// its working directory, as a sandbox does.
type walkingImporter struct {
	*importing
	dir string
}

func (w *walkingImporter) Walk(ctx context.Context, q machine.WalkRequest) (machine.WalkResult, error) {
	return machine.WalkFS(ctx, os.DirFS(w.dir), q)
}

// TestANewMachineIsRestoredFromTheCopy: a session with a folder keeps its
// working directory there at each turn end, a command's files and their
// modes included and a dependency directory left out; a turn whose copy
// the files store refused leaves its written file to the log; and a
// machine made new in the place of the lost one is restored from the copy
// before its first call, then from the log, and recorded restored from the
// folder, a file the folder no longer holds omitted; the model reads it,
// and the next turn end deletes what the copy holds and the directory
// does not (spec 073).
func TestANewMachineIsRestoredFromTheCopy(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	folder := newLiveFolder()
	var opens atomic.Int32
	machineAs := func(id string, created bool) {
		f.onDemandWith(f.work, &opens, demand{files: true, folder: folder, wrap: func(m machine.Machine) machine.Machine {
			return &walkingImporter{importing: &importing{recovering: &recovering{Machine: m, id: id, created: created}}, dir: f.work}
		}})
	}
	machineAs("sbx_1", true)
	f.stub.Script(model,
		reply(call(t, "toolu_1", tools.NameWrite, map[string]any{"path": "src/app.js", "content": "app()\n"})),
		reply(call(t, "toolu_2", tools.NameBash, map[string]any{"command": "mkdir -p dist node_modules/dep && echo '<h1>Tides</h1>' > dist/index.html && echo dep > node_modules/dep/x.js && printf '#!/bin/sh\\necho hi\\n' > run.sh && chmod 755 run.sh"})),
		reply(ir.Block{Type: ir.BlockText, Text: "Built."}),
	)
	f.message(ctx, "Build the site.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"work/src/app.js", "work/dist/index.html", "work/run.sh", "work/AGENTS.md"} {
		if !folder.has(ref) {
			t.Fatalf("the folder holds no %s: %v", ref, folder.writings)
		}
	}
	if folder.has("work/node_modules/dep/x.js") {
		t.Fatal("a dependency directory was kept")
	}

	machineAs("sbx_1", false)
	folder.refuse = &harness.FileError{Op: "put", Status: 503, Code: "storage_unavailable"}
	f.stub.Script(model,
		reply(call(t, "toolu_3", tools.NameWrite, map[string]any{"path": "notes.md", "content": "notes\n"})),
		reply(ir.Block{Type: ir.BlockText, Text: "Noted."}),
	)
	f.message(ctx, "Take notes.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	folder.refuse = nil
	if err := folder.Delete(ctx, "work/dist/index.html"); err != nil {
		t.Fatal(err)
	}

	// The machine is lost; its successor's working directory is empty.
	entries, err := os.ReadDir(f.work)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(f.work, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	machineAs("sbx_2", true)
	f.stub.Script(model,
		reply(call(t, "toolu_4", tools.NameBash, map[string]any{"command": "ls"})),
		reply(ir.Block{Type: ir.BlockText, Text: "Back."}),
	)
	f.message(ctx, "Go on.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	got := on(t, f.work)
	if got["src/app.js"] != "app()\n" || got["run.sh"] != "#!/bin/sh\necho hi\n" || got["notes.md"] != "notes\n" || got["AGENTS.md"] == "" {
		t.Fatalf("the new machine holds %q", got)
	}
	if _, ok := got["dist/index.html"]; ok {
		t.Fatal("a file the folder no longer held is back")
	}
	if _, ok := got["node_modules/dep/x.js"]; ok {
		t.Fatal("a dependency directory is back")
	}
	if fi, err := os.Stat(filepath.Join(f.work, "run.sh")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh came back %v, %v", fi.Mode(), err)
	}
	ms := f.machines(ctx)
	if len(ms) != 2 || ms[1].Reason != session.MachineRestored || ms[1].Machine.ID != "sbx_2" || ms[1].Restore == nil {
		t.Fatalf("session.machine records %+v", ms)
	}
	r := *ms[1].Restore
	if r.Source != session.RestoreFromFolder || r.Copy == nil || r.Copy.Turn != 2 || r.Copy.Files != len(got)-1 || r.Files != 1 || !slices.Equal(r.Paths, []string{"notes.md"}) ||
		!slices.Equal(r.OmittedFiles, []session.OmittedFile{{Path: "dist/index.html", Reason: session.OmittedCopy}}) {
		t.Fatalf("the restore record %+v, copy %+v", r, r.Copy)
	}
	if n := f.count(ctx, session.TypeSessionError); n != 0 {
		t.Fatalf("%d session.error beside a restored machine", n)
	}
	if n := len(f.stub.Requests()) - 1; !f.said(n, "was restored from the copy this conversation keeps of it, as it was saved at the end of turn 2") {
		t.Fatal("the model does not read that the working directory came back from the copy")
	}
	saves := f.events(ctx, session.TypeWorkSaved)
	var last session.WorkSaved
	if err := saves[len(saves)-1].Decode(&last); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(last.Written, func(w session.WorkFile) bool { return w.Path == "notes.md" }) || !slices.Equal(last.Removed, []string{"dist/index.html"}) {
		t.Fatalf("the save after the restore %+v", last)
	}
}

// TestACopyThatCannotBeReadLeavesTheLog: a machine made new for a session
// whose folder does not answer a read of its copy is rebuilt from the log
// alone, recorded with source log and why the copy could not be written
// (spec 073).
func TestACopyThatCannotBeReadLeavesTheLog(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	folder := newLiveFolder()
	var opens atomic.Int32
	machineAs := func(id string) {
		f.onDemandWith(f.work, &opens, demand{files: true, folder: folder, wrap: func(m machine.Machine) machine.Machine {
			return &walkingImporter{importing: &importing{recovering: &recovering{Machine: m, id: id, created: true}}, dir: f.work}
		}})
	}
	machineAs("sbx_1")
	f.stub.Script(model,
		reply(call(t, "toolu_1", tools.NameWrite, map[string]any{"path": "src/app.js", "content": "app()\n"})),
		reply(ir.Block{Type: ir.BlockText, Text: "Written."}),
	)
	f.message(ctx, "Write it.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.work, "src")); err != nil {
		t.Fatal(err)
	}
	folder.away = errors.New("the files store is away")
	machineAs("sbx_2")
	f.stub.Script(model,
		reply(call(t, "toolu_2", tools.NameBash, map[string]any{"command": "ls"})),
		reply(ir.Block{Type: ir.BlockText, Text: "Back."}),
	)
	f.message(ctx, "Go on.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	if got := on(t, f.work); got["src/app.js"] != "app()\n" {
		t.Fatalf("the new machine holds %q", got)
	}
	ms := f.machines(ctx)
	r := ms[len(ms)-1].Restore
	if r == nil || r.Source != session.RestoreFromLog || r.Copy == nil || r.Copy.Failed == "" || r.Copy.Files != 0 {
		t.Fatalf("the restore record %+v", r)
	}
}
