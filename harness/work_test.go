// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"latere.ai/x/pkg/llmdialect/ir"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/session"
)

// walkingMachine is a host machine that walks its working directory as a
// sandbox does and says what its open made of it, with a walk failure a
// test injects.
type walkingMachine struct {
	*host.Host
	dir     string
	walkErr error
}

func (w *walkingMachine) Walk(ctx context.Context, q machine.WalkRequest) (machine.WalkResult, error) {
	if w.walkErr != nil {
		return machine.WalkResult{}, w.walkErr
	}
	return machine.WalkFS(ctx, os.DirFS(w.dir), q)
}

func (*walkingMachine) Created() bool                                          { return false }
func (*walkingMachine) Restarted() (machine.Restart, bool)                     { return machine.Restart{}, false }
func (*walkingMachine) OnRestart(func(context.Context, machine.Restart) error) {}

// walking is a walking machine over files.
func walking(t *testing.T, files map[string][]byte) *walkingMachine {
	t.Helper()
	m, dir := workMachine(t, files)
	return &walkingMachine{Host: m, dir: dir}
}

// put writes a file of the machine's working directory with mode.
func (w *walkingMachine) put(t *testing.T, rel, data string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(w.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// sized writes a sparse file of n bytes, all zero.
func (w *walkingMachine) sized(t *testing.T, rel string, n int64) {
	t.Helper()
	w.put(t, rel, "", 0o644)
	if err := os.Truncate(filepath.Join(w.dir, filepath.FromSlash(rel)), n); err != nil {
		t.Fatal(err)
	}
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sinkFiles is a files store that keeps each file's digest and size and
// none of its bytes.
type sinkFiles struct {
	mu    sync.Mutex
	files map[string]string
	*memFiles
}

func (f *sinkFiles) Overwrite(_ context.Context, ref, _ string, size int64, r io.Reader) (session.Digest, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", err
	}
	if n != size {
		return "", ErrFileSizeChanged
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[ref] = hex.EncodeToString(h.Sum(nil))
	return session.Digest("sha256:" + f.files[ref]), nil
}

func (f *sinkFiles) Delete(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, ref)
	return nil
}

// savedEvent is a work.saved of p at seq.
func savedEvent(t *testing.T, seq uint64, p session.WorkSaved) session.Event {
	t.Helper()
	e, err := session.NewEvent(session.TypeWorkSaved, p, t0)
	if err != nil {
		t.Fatal(err)
	}
	e.Seq = seq
	return e
}

// TestATurnEndKeepsTheWorkingDirectory: a turn that ran a call ends with
// a work.saved before its status, having written each file of the working
// directory at work/<path> with its mode and the media type its name
// gives, a dependency directory left out; a turn that ran no call writes
// nothing; a later turn writes only what changed and deletes what is
// gone; and a session with no files store, or a machine that does not
// walk, keeps no copy (spec 073).
func TestATurnEndKeepsTheWorkingDirectory(t *testing.T) {
	m := walking(t, map[string][]byte{"index.html": []byte("<h1>Hi</h1>"), "src/app.js": []byte("app()"), "node_modules/dep/index.js": []byte("dep")})
	m.put(t, "run.sh", "#!/bin/sh\n", 0o755)
	files := newMemFiles()
	e := setup(t, func(c *Config) {
		c.Machine = m
		c.Files = files
		c.MediaType = func(name string, _ []byte) string { return "x-test/" + name }
	})
	ctx := t.Context()
	e.echo.run = func(c tools.Call) (tools.Result, error) {
		switch c.ID {
		case "toolu_build":
			m.put(t, "dist/out.txt", "built", 0o644)
		case "toolu_change":
			m.put(t, "index.html", "<h1>Hello</h1>", 0o644)
			if err := os.Remove(filepath.Join(m.dir, "src", "app.js")); err != nil {
				return tools.Result{}, err
			}
		}
		return tools.Text(tools.OutcomeOK, "done"), nil
	}
	e.stub.Script(model,
		reply(ir.StopToolUse, call("toolu_build", "echo", `{"text":"build"}`)),
		reply(ir.StopEndTurn, text("Built.")),
		reply(ir.StopEndTurn, text("Hello.")),
		reply(ir.StopToolUse, call("toolu_change", "echo", `{"text":"change"}`)),
		reply(ir.StopEndTurn, text("Changed.")),
	)
	e.send(ctx, "Build it.")
	e.turn(ctx)
	saves := e.events(ctx, session.TypeWorkSaved)
	if len(saves) != 1 {
		t.Fatalf("%d work.saved after the first turn", len(saves))
	}
	var first session.WorkSaved
	if err := saves[0].Decode(&first); err != nil {
		t.Fatal(err)
	}
	want := []session.WorkFile{
		{Path: "dist/out.txt", Size: 5, Mode: 0o644, SHA256: hexSum([]byte("built"))},
		{Path: "index.html", Size: 11, Mode: 0o644, SHA256: hexSum([]byte("<h1>Hi</h1>"))},
		{Path: "run.sh", Size: 10, Mode: 0o755, SHA256: hexSum([]byte("#!/bin/sh\n"))},
		{Path: "src/app.js", Size: 5, Mode: 0o644, SHA256: hexSum([]byte("app()"))},
	}
	if !slices.Equal(first.Written, want) || first.Held != (session.WorkHeld{Files: 4, Bytes: 31}) || first.Failed != nil {
		t.Fatalf("the first save %+v", first)
	}
	if string(files.files["work/dist/out.txt"]) != "built" || files.media["work/index.html"] != "x-test/index.html" {
		t.Fatalf("the folder %v", files.media)
	}
	if _, ok := files.files["work/node_modules/dep/index.js"]; ok {
		t.Fatal("a dependency directory was kept")
	}
	evs, err := e.store.Events(ctx, e.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	last := evs[len(evs)-1]
	if last.Type != session.TypeSessionStatus || evs[len(evs)-2].ID != saves[0].ID || first.Covers >= saves[0].Seq {
		t.Fatalf("the save is not just before the turn's status: %s at %d, covers %d", last.Type, saves[0].Seq, first.Covers)
	}
	for _, ev := range evs[first.Covers:] {
		if ev.Type == session.TypeAgentToolUse || ev.Type == session.TypeToolResult {
			t.Fatalf("a call after the save's covers: %s at %d", ev.Type, ev.Seq)
		}
	}

	e.send(ctx, "Say hello.")
	e.turn(ctx)
	if got := len(e.events(ctx, session.TypeWorkSaved)); got != 1 {
		t.Fatalf("a turn that ran no call saved: %d", got)
	}

	files.overwrites = nil
	e.send(ctx, "Change it.")
	e.turn(ctx)
	saves = e.events(ctx, session.TypeWorkSaved)
	var second session.WorkSaved
	if err := saves[len(saves)-1].Decode(&second); err != nil {
		t.Fatal(err)
	}
	if len(second.Written) != 1 || second.Written[0].Path != "index.html" || !slices.Equal(second.Removed, []string{"src/app.js"}) || second.Held.Files != 3 {
		t.Fatalf("the second save %+v", second)
	}
	if !slices.Equal(files.overwrites, []string{"work/index.html"}) || !slices.Contains(files.deletes, "work/src/app.js") {
		t.Fatalf("overwrites %v, deletes %v", files.overwrites, files.deletes)
	}
	if _, ok := files.files["work/src/app.js"]; ok {
		t.Fatal("a removed file is still in the folder")
	}

	for name, mut := range map[string]func(*Config){
		"no files store":               func(c *Config) { c.Machine = m },
		"a machine that does not walk": func(c *Config) { c.Machine = m.Host; c.Files = newMemFiles() },
	} {
		e := setup(t, mut)
		e.stub.Script(model, reply(ir.StopToolUse, call("toolu_x", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Done.")))
		e.send(ctx, "Go.")
		e.turn(ctx)
		if got := e.events(ctx, session.TypeWorkSaved); len(got) != 0 {
			t.Errorf("%s: %d work.saved", name, len(got))
		}
	}
}

// TestTheCopyHoldsToItsBounds: a file past the per-file bound is skipped
// too_large, and deleted from the copy once it grows past it; a turn end
// writes at most its files and its bytes, in walk order, the rest
// deferred and written by the next; a mode that changed alone is recorded
// with no write; and an unchanged directory appends nothing (spec 073).
func TestTheCopyHoldsToItsBounds(t *testing.T) {
	ctx := t.Context()
	m := walking(t, nil)
	m.put(t, "grows.bin", "small", 0o644)
	m.sized(t, "huge.bin", session.MaxWorkFileBytes+1)
	files := &sinkFiles{files: map[string]string{}}
	p, ok := SaveWork(ctx, m, files, nil, 5, nil)
	if !ok || len(p.Written) != 1 || p.Skipped != 1 || p.SkippedFiles[0] != (session.OmittedFile{Path: "huge.bin", Reason: session.WorkTooLarge}) || p.Covers != 5 {
		t.Fatalf("the first save %+v", p)
	}
	log := []session.Event{savedEvent(t, 6, p)}
	if _, ok := SaveWork(ctx, m, files, log, 6, nil); ok {
		t.Fatal("an unchanged directory saved again")
	}
	m.sized(t, "grows.bin", session.MaxWorkFileBytes+1)
	p, ok = SaveWork(ctx, m, files, log, 7, nil)
	if !ok || !slices.Equal(p.Removed, []string{"grows.bin"}) || p.Skipped != 2 || p.Held.Files != 0 {
		t.Fatalf("a file that grew past the bound %+v", p)
	}
	if _, ok := files.files["work/grows.bin"]; ok {
		t.Fatal("a file past the bound is still in the copy")
	}
	if err := os.Remove(filepath.Join(m.dir, "grows.bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(m.dir, "huge.bin")); err != nil {
		t.Fatal(err)
	}

	// Three files of 30 MiB: two fit one turn end's bytes.
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		m.sized(t, "big/"+name, 30<<20)
	}
	p, ok = SaveWork(ctx, m, files, nil, 8, nil)
	if !ok || len(p.Written) != 2 || p.Written[1].Path != "big/b.bin" || p.Deferred != 1 {
		t.Fatalf("a save past the turn's bytes %+v", p)
	}
	log = []session.Event{savedEvent(t, 9, p)}
	if c := session.CopyOf(log); !c.Pending {
		t.Fatal("a save that deferred a file is not pending")
	}
	p, ok = SaveWork(ctx, m, files, log, 10, nil)
	if !ok || len(p.Written) != 1 || p.Written[0].Path != "big/c.bin" || p.Deferred != 0 || p.Held.Files != 3 {
		t.Fatalf("the next save %+v", p)
	}
	log = append(log, savedEvent(t, 11, p))
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.Remove(filepath.Join(m.dir, "big", name)); err != nil {
			t.Fatal(err)
		}
	}

	// One file more than a turn end writes.
	for i := range session.MaxWorkTurnFiles + 1 {
		m.put(t, fmt.Sprintf("many/%04d.txt", i), "x", 0o644)
	}
	p, ok = SaveWork(ctx, m, files, log, 12, nil)
	if !ok || len(p.Written) != session.MaxWorkTurnFiles || p.Deferred != 1 || len(p.Removed) != 3 {
		t.Fatalf("a save past the turn's files: %d written, %d deferred, %v removed", len(p.Written), p.Deferred, p.Removed)
	}
	log = append(log, savedEvent(t, 13, p))
	m.put(t, "many/0000.txt", "x", 0o755)
	p, ok = SaveWork(ctx, m, files, log, 14, nil)
	if !ok || len(p.Written) != 2 || p.Written[0].Path != "many/0000.txt" || p.Written[0].Mode != 0o755 || p.Written[1].Path != fmt.Sprintf("many/%04d.txt", session.MaxWorkTurnFiles) {
		t.Fatalf("a mode changed alone and the deferred file %+v", p.Written)
	}
}

// failingFiles is a files store whose writes or deletes fail.
type failingFiles struct {
	*memFiles
	writeErr, deleteErr error
}

func (f *failingFiles) Overwrite(ctx context.Context, ref, media string, size int64, r io.Reader) (session.Digest, error) {
	if f.writeErr != nil {
		return "", f.writeErr
	}
	return f.memFiles.Overwrite(ctx, ref, media, size, r)
}

func (f *failingFiles) Delete(ctx context.Context, ref string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.memFiles.Delete(ctx, ref)
}

// TestACopyThatCannotBeWrittenIsRecorded: a walk the machine does not
// answer is recorded machine_unavailable and changes nothing of the copy;
// a lost machine records nothing; a write the files store refuses for
// the file's size skips it too_large; any other refusal of a write or a
// delete stops the save files_unavailable, the files written before it
// recorded and the rest no longer vouched for; and a turn whose copy
// failed still ends (spec 073).
func TestACopyThatCannotBeWrittenIsRecorded(t *testing.T) {
	ctx := t.Context()
	m := walking(t, map[string][]byte{"a.txt": []byte("a"), "b.txt": []byte("b")})
	m.walkErr = errors.New("the helper is gone")
	p, ok := SaveWork(ctx, m, newMemFiles(), nil, 3, nil)
	if !ok || p.Failed == nil || p.Failed.Code != session.WorkMachineUnavailable || !strings.Contains(p.Failed.Detail, "the helper is gone") || len(p.Written) != 0 {
		t.Fatalf("a failed walk %+v", p)
	}
	m.walkErr = fmt.Errorf("walk: %w", machine.ErrLost)
	if _, ok := SaveWork(ctx, m, newMemFiles(), nil, 3, nil); ok {
		t.Fatal("a lost machine recorded a save")
	}
	m.walkErr = nil

	tooLarge := &FileError{Op: "put", Status: StatusObjectTooLarge, Code: CodeObjectTooLarge}
	p, ok = SaveWork(ctx, m, &failingFiles{memFiles: newMemFiles(), writeErr: tooLarge}, nil, 3, nil)
	if !ok || p.Failed != nil || p.Skipped != 2 || p.SkippedFiles[0].Reason != session.WorkTooLarge {
		t.Fatalf("a write refused for its size %+v", p)
	}
	p, ok = SaveWork(ctx, m, &failingFiles{memFiles: newMemFiles(), writeErr: &FileError{Op: "put", Status: 403, Code: "forbidden"}}, nil, 3, nil)
	if !ok || p.Failed == nil || p.Failed.Code != session.WorkFilesUnavailable || len(p.Written) != 0 || p.Deferred != 1 {
		t.Fatalf("a refused write %+v", p)
	}
	ok1, _ := SaveWork(ctx, m, newMemFiles(), nil, 3, nil)
	log := []session.Event{savedEvent(t, 4, ok1)}
	if err := os.Remove(filepath.Join(m.dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	m.put(t, "b.txt", "changed", 0o644)
	p, ok = SaveWork(ctx, m, &failingFiles{memFiles: newMemFiles(), deleteErr: errors.New("no")}, log, 5, nil)
	if !ok || p.Failed == nil || p.Failed.Code != session.WorkFilesUnavailable || !slices.Equal(p.Unsaved, []string{"a.txt", "b.txt"}) || p.Deferred != 1 || p.Held.Files != 0 {
		t.Fatalf("a refused delete %+v", p)
	}
	c := session.CopyOf(append(log, savedEvent(t, 6, p)))
	if len(c.Files) != 0 || !c.Stale["a.txt"] || !c.Stale["b.txt"] || !c.Pending {
		t.Fatalf("the copy after a refused delete %+v", c)
	}

	files := &failingFiles{memFiles: newMemFiles(), writeErr: &FileError{Op: "put", Status: 503, Code: "storage_unavailable"}}
	e := setup(t, func(c *Config) { c.Machine = m; c.Files = files })
	e.stub.Script(model, reply(ir.StopToolUse, call("toolu_x", "echo", `{"text":"x"}`)), reply(ir.StopEndTurn, text("Done.")))
	e.send(ctx, "Go.")
	if out := e.turn(ctx); out.Status != session.StatusIdle || out.StopReason != session.StopEndTurn {
		t.Fatalf("a turn whose copy failed ended %+v", out)
	}
	saves := e.events(ctx, session.TypeWorkSaved)
	var got session.WorkSaved
	if len(saves) != 1 || saves[0].Decode(&got) != nil || got.Failed == nil || got.Failed.Code != session.WorkFilesUnavailable {
		t.Fatalf("the turn's save %+v", saves)
	}
}
