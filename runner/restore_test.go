// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/harness/tools"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// importing is a recovering machine that takes an archive, as a sandbox
// does: it extracts the archive's files below dest, counts the archives
// it was given, and fails with fail when that is set.
type importing struct {
	*recovering
	imports atomic.Int32
	fail    error
}

func (m *importing) ImportTar(ctx context.Context, dest string, r io.Reader) error {
	m.imports.Add(1)
	if m.fail != nil {
		return m.fail
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if err := m.WriteFile(ctx, filepath.ToSlash(filepath.Join(dest, h.Name)), tr, 0o644); err != nil {
			return err
		}
	}
}

// fresh is an empty directory for a machine made new.
func fresh(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// call is a reply that calls a tool with input, an object.
func call(t *testing.T, id, name string, input map[string]any) ir.Block {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return toolUse(id, name, string(b))
}

// build drives a first turn on the machine id in f.work: the agent
// writes and edits a file, a command makes three, of which the agent
// reads one in full and edits it, reads one in part, and never touches
// the third.
func (f *fixture) build(ctx context.Context, t *testing.T, id string) {
	t.Helper()
	var opens atomic.Int32
	f.onDemandWith(f.work, &opens, demand{files: true, wrap: func(m machine.Machine) machine.Machine { return &recovering{Machine: m, id: id, created: true} }})
	f.stub.Script(model,
		reply(call(t, "toolu_1", tools.NameWrite, map[string]any{"path": "src/app.js", "content": "const greeting = 'hello';\nconsole.log(greeting);\n"})),
		reply(call(t, "toolu_2", tools.NameEdit, map[string]any{"path": "src/app.js", "old_string": "'hello'", "new_string": "'tide tables'"})),
		reply(call(t, "toolu_3", tools.NameBash, map[string]any{"command": "printf '{\\n  \"name\": \"app\"\\n}\\n' > package.json; seq 1 40 > rows.txt; echo built > out.bin"})),
		reply(call(t, "toolu_4", tools.NameRead, map[string]any{"path": "package.json"})),
		reply(call(t, "toolu_5", tools.NameEdit, map[string]any{"path": "package.json", "old_string": "\"app\"", "new_string": "\"tides\""})),
		reply(call(t, "toolu_6", tools.NameRead, map[string]any{"path": "rows.txt", "limit": 3})),
		reply(ir.Block{Type: ir.BlockText, Text: "Built."}),
	)
	f.message(ctx, "Build the app.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	for _, e := range f.events(ctx, session.TypeToolResult) {
		var p session.ToolResult
		if err := e.Decode(&p); err != nil || p.IsError {
			t.Fatalf("a call of the first turn failed: %+v, %v", p, err)
		}
	}
}

// on reads the files of a directory as path to content.
func on(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAReplacedMachineIsRebuiltFromTheLog: a drive whose open made the
// session's machine new, in the place of one the log records, writes back
// what the log holds of the working directory before the call that
// opened it runs: the written and edited file byte for byte, and the
// file a command made that the agent read in full and edited. The file
// read in part is named as missing, and the one only a command made is
// not there. The machine is recorded as restored from the log with the
// counts and the paths, with no machine_lost; the model reads what is
// back and what is not; and the agent edits a restored file without
// reading it again, since it hashes to what the log recorded (spec 065).
func TestAReplacedMachineIsRebuiltFromTheLog(t *testing.T) {
	for name, archive := range map[string]bool{"file by file": false, "as one archive": true} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			ctx := t.Context()
			f.build(ctx, t, "sbx_1")
			was := on(t, f.work)
			// The new machine's working directory is at the path the lost
			// one's was, as a session's sandboxes are, and holds nothing.
			next := f.work
			for _, name := range []string{"src", "package.json", "rows.txt", "out.bin"} {
				if err := os.RemoveAll(filepath.Join(next, name)); err != nil {
					t.Fatal(err)
				}
			}
			before := len(on(t, next))
			var opens atomic.Int32
			var taken *importing
			f.onDemandWith(next, &opens, demand{files: true, wrap: func(m machine.Machine) machine.Machine {
				rec := &recovering{Machine: m, id: "sbx_2", created: true}
				if !archive {
					return rec
				}
				taken = &importing{recovering: rec}
				return taken
			}})
			f.stub.Script(model,
				reply(call(t, "toolu_7", tools.NameEdit, map[string]any{"path": "src/app.js", "old_string": "tide tables", "new_string": "harbor"})),
				reply(ir.Block{Type: ir.BlockText, Text: "The app is back; the rows are not."}),
			)
			f.message(ctx, "Go on.")
			if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
				t.Fatal(err)
			}
			got := on(t, next)
			want := map[string]string{
				"src/app.js":   strings.Replace(was["src/app.js"], "tide tables", "harbor", 1),
				"package.json": was["package.json"],
			}
			if len(got) != before+len(want) || got["src/app.js"] != want["src/app.js"] || got["package.json"] != want["package.json"] || !strings.Contains(got["package.json"], `"tides"`) {
				t.Fatalf("the new machine holds %q, want %q and nothing else of the session's", got, want)
			}
			if archive && taken.imports.Load() != 1 {
				t.Fatalf("%d archives for one rebuild", taken.imports.Load())
			}
			ms := f.machines(ctx)
			if len(ms) != 2 || ms[1].Reason != session.MachineRestored || ms[1].Machine.ID != "sbx_2" || ms[1].Checkpoint != nil || ms[1].Restore == nil {
				t.Fatalf("session.machine records %+v", ms)
			}
			wantRestore := session.Restore{
				Source: session.RestoreFromLog, Files: 2, Bytes: int64(len(was["src/app.js"]) + len(was["package.json"])), Omitted: 1, Commands: 1,
				Paths: []string{"package.json", "src/app.js"}, OmittedFiles: []session.OmittedFile{{Path: "rows.txt", Reason: session.OmittedUnknown}},
			}
			if r := *ms[1].Restore; r.Source != wantRestore.Source || r.Files != wantRestore.Files || r.Bytes != wantRestore.Bytes || r.Unverified != 0 || r.Omitted != 1 ||
				r.Commands != 1 || !slices.Equal(r.Paths, wantRestore.Paths) || !slices.Equal(r.OmittedFiles, wantRestore.OmittedFiles) || len(r.UnverifiedPaths) != 0 {
				t.Fatalf("the restore record %+v, want %+v", r, wantRestore)
			}
			if n := f.count(ctx, session.TypeSessionError); n != 0 {
				t.Fatalf("%d session.error events for a machine whose files the log gave back", n)
			}
			// The record lands before the result of the call that opened the
			// machine, and that call, an edit, found the file there.
			evs, results := f.events(ctx, session.TypeSessionMachine), f.events(ctx, session.TypeToolResult)
			var edited session.ToolResult
			last := results[len(results)-1]
			if err := last.Decode(&edited); err != nil || edited.IsError || evs[1].Seq >= last.Seq {
				t.Fatalf("the edit after the rebuild: %+v, %v; the record at %d, the result at %d", edited, err, evs[1].Seq, last.Seq)
			}
			note := prompts.Render(prompts.TranscriptMachineRebuilt, prompts.Data{
				"Files": 2, "Paths": []string{"package.json", "src/app.js"}, "More": 0, "Unverified": []string(nil), "UnverifiedMore": 0,
				"Omitted": []string{"rows.txt"}, "OmittedMore": 0, "Commands": 1,
			})
			if n := len(f.stub.Requests()) - 1; !f.said(n, note) || f.said(n, prompts.Text(prompts.TranscriptMachineReplaced)) {
				t.Fatal("the model does not read what is back and what is not, or reads that everything is gone")
			}
		})
	}
}

// TestARebuildThatCannotBeWrittenIsALoss: a rebuild the new machine does
// not take leaves the machine recorded as replaced with machine_lost,
// whose detail says why, and the turn goes on (spec 065).
func TestARebuildThatCannotBeWrittenIsALoss(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.build(ctx, t, "sbx_1")
	var opens atomic.Int32
	f.onDemandWith(fresh(t), &opens, demand{files: true, wrap: func(m machine.Machine) machine.Machine {
		return &importing{recovering: &recovering{Machine: m, id: "sbx_2", created: true}, fail: errors.New("the disk is full")}
	}})
	f.stub.Script(model, reply(toolUse("toolu_7", "bash", `{"command":"ls"}`)), reply(ir.Block{Type: ir.BlockText, Text: "The files are gone."}))
	f.message(ctx, "Go on.")
	if _, err := f.r.Drive(ctx, f.s.ID); err != nil {
		t.Fatal(err)
	}
	ms := f.machines(ctx)
	if len(ms) != 2 || ms[1].Reason != session.MachineReplaced || ms[1].Restore != nil {
		t.Fatalf("session.machine records %+v", ms)
	}
	var lost session.SessionError
	if errs := f.events(ctx, session.TypeSessionError); len(errs) != 1 || errs[0].Decode(&lost) != nil || lost.Code != machine.CodeLost || !strings.Contains(lost.Detail, "the disk is full") {
		t.Fatalf("session.error events %+v", lost)
	}
}

// TestAForksFirstMachineIsRebuiltFromTheLog: a fork whose fork point kept
// no checkpoint gets, on a first machine its open made new, the working
// directory its copied log holds, recorded as restored from the log, and
// its model reads what is back and what is not. A fork on a machine that
// does not say it is new is left as it is (spec 065).
func TestAForksFirstMachineIsRebuiltFromTheLog(t *testing.T) {
	f := setup(t)
	ctx := t.Context()
	f.build(ctx, t, "sbx_1")
	was := on(t, f.work)
	evs, err := f.store.Events(ctx, f.s.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := session.ForkPoint(evs, nil)
	if err != nil {
		t.Fatal(err)
	}
	fork := func() session.Session {
		child := session.New(f.s.Agent, f.s.Initiator, session.RunnerExternal, session.Machine{Kind: machine.KindHost}, t0)
		child, err := session.Fork(ctx, f.store, child, nil, f.s, evs[:seq])
		if err != nil {
			t.Fatal(err)
		}
		return child
	}
	drive := func(child session.Session, dir string, created bool) {
		var opens atomic.Int32
		f.onDemandWith(dir, &opens, demand{files: true, wrap: func(m machine.Machine) machine.Machine {
			return &recovering{Machine: m, id: "sbx_" + child.ID, created: created}
		}})
		f.stub.Script(model, reply(toolUse("toolu_f", "bash", `{"command":"ls"}`)), reply(ir.Block{Type: ir.BlockText, Text: "Continuing."}))
		f.sendTo(ctx, child.ID, session.TypeUserMessage, session.UserMessage{Sender: child.Initiator, Content: []lux.Block{{Type: ir.BlockText, Text: "Continue."}}})
		if _, err := f.r.Drive(ctx, child.ID); err != nil {
			t.Fatal(err)
		}
	}
	child, dir := fork(), fresh(t)
	drive(child, dir, true)
	if got := on(t, dir); len(got) != 2 || got["src/app.js"] != was["src/app.js"] || got["package.json"] != was["package.json"] {
		t.Fatalf("the fork's machine holds %q", got)
	}
	forked, err := f.store.Events(ctx, child.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	own := ownMachine(t, child, forked)
	if own.Reason != session.MachineRestored || own.Restore == nil || own.Restore.Source != session.RestoreFromLog || own.Restore.Files != 2 || own.Checkpoint != nil {
		t.Fatalf("the fork's first machine is recorded %+v", own)
	}
	if errs := f.errorsIn(ctx, child.ID); len(errs) != 0 {
		t.Fatalf("the fork recorded %+v", errs)
	}
	if n := len(f.stub.Requests()) - 1; !f.said(n, "was rebuilt from this conversation's record") {
		t.Fatal("the fork's model does not read that its working directory was rebuilt")
	}

	// A machine that does not say it is new may hold the session's own
	// files, and is not written over.
	other, kept := fork(), fresh(t)
	write(t, filepath.Join(kept, "src", "app.js"), "a newer edit\n")
	drive(other, kept, false)
	if got := on(t, kept); len(got) != 1 || got["src/app.js"] != "a newer edit\n" {
		t.Fatalf("a machine that is not new was written over: %q", got)
	}
	forked, err = f.store.Events(ctx, other.ID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if own := ownMachine(t, other, forked); own.Reason != session.MachineAttached || own.Restore != nil {
		t.Fatalf("its machine is recorded %+v", own)
	}
}

// TestRebuiltFilesCarryTheTimeTheyWereWrittenBack: the archive a rebuilt
// working directory reaches a machine in dates every directory and every
// file at the runner's clock, so no file comes back dated 1970, which
// tools refuse: Python's zipfile raises on a date before 1980.
func TestRebuiltFilesCarryTheTimeTheyWereWrittenBack(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	files := []tools.RebuiltFile{{Path: "src/app.js", Content: []byte("x\n")}, {Path: "notes.txt", Content: []byte("y\n")}}
	open := func(f tools.RebuiltFile) ([]byte, bool, error) { return f.Content, true, nil }
	var buf bytes.Buffer
	if err := writeTar(&buf, files, open, now); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		if !h.ModTime.Equal(now) {
			t.Fatalf("%s is dated %s, want the runner's clock %s", h.Name, h.ModTime, now)
		}
	}
	if !slices.Equal(names, []string{"src/", "src/app.js", "notes.txt"}) {
		t.Fatalf("the archive holds %q", names)
	}
}
