// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/session"
)

// worklog is a session's log as its tools write it on a machine: every
// call is run by the built-in tool itself, so the log holds the inputs,
// the results and the hashes a real session's holds.
type worklog struct {
	t      *testing.T
	m      machine.Machine
	events []session.Event
	calls  int
}

// add appends an event at the next sequence.
func (w *worklog) add(typ session.Type, payload any) session.Event {
	w.t.Helper()
	e, err := session.NewEvent(typ, payload, time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	e.Seq = uint64(len(w.events) + 1)
	w.events = append(w.events, e)
	return e
}

// machine records the machine the log's calls run on, with a reason.
func (w *worklog) machine(reason string) {
	w.add(session.TypeSessionMachine, session.SessionMachine{Machine: session.AttachedMachine{Kind: "cella", ID: "sbx_1", Workdir: w.m.Info().Workdir}, Reason: reason})
}

// call runs a built-in tool and records its call and its result, and
// returns the two events.
func (w *worklog) call(name, input string) (session.Event, session.Event, Result) {
	w.t.Helper()
	w.calls++
	id := fmt.Sprintf("toolu_%d", w.calls)
	use := w.add(session.TypeAgentToolUse, session.AgentToolUse{ToolUseID: id, Name: name, Input: json.RawMessage(input), Verdict: "allow"})
	res, err := builtinTool(w.t, name).Run(w.t.Context(), Call{ID: id, Input: json.RawMessage(input), Machine: w.m, State: StateOf(w.events, "")})
	if err != nil {
		w.t.Fatalf("%s %s: %v", name, input, err)
	}
	var meta json.RawMessage
	if res.Meta != nil {
		if meta, err = json.Marshal(res.Meta); err != nil {
			w.t.Fatal(err)
		}
	}
	result := w.add(session.TypeToolResult, session.ToolResult{ToolUseID: id, Content: res.Content, IsError: res.IsError(), Outcome: res.Outcome, Spill: res.Spill, Meta: meta})
	return use, result, res
}

// ok is call for a call that must succeed.
func (w *worklog) ok(name, input string) {
	w.t.Helper()
	if _, _, res := w.call(name, input); res.IsError() {
		w.t.Fatalf("%s %s: %s", name, input, text(res))
	}
}

// redact replaces an event with its tombstone, as a redaction does.
func (w *worklog) redact(e session.Event) {
	w.t.Helper()
	tomb, _, err := session.Tombstone(e, uint64(len(w.events)), session.Sender{Subject: "usr_ada", Kind: session.SenderPerson}, "", time.Now())
	if err != nil {
		w.t.Fatal(err)
	}
	w.events[e.Seq-1] = tomb
}

// rebuiltFiles are a rebuild's files as "path=content", unverified ones
// marked, and its omitted files as "path: reason".
func rebuiltFiles(r Rebuilt) ([]string, []string) {
	var files, omitted []string
	for _, f := range r.Files {
		s := f.Path + "=" + string(f.Content)
		if f.Blob != "" {
			s = f.Path + "=" + string(f.Blob)
		}
		if f.Unverified {
			s += " (unverified)"
		}
		files = append(files, s)
	}
	for _, o := range r.Omitted {
		omitted = append(omitted, o.Path+": "+o.Reason)
	}
	return files, omitted
}

// TestRebuildReplaysWritesEditsAndFullReads: the log of a session whose
// tools wrote, edited and read files gives each file back byte for byte:
// a write's content, each edit applied in order, a file a command made
// from the read of the whole of it, with a last newline or without, and
// an image from its read. An edit of such a file follows from the read.
// The commands that ran since the first file are counted (spec 065).
func TestRebuildReplaysWritesEditsAndFullReads(t *testing.T) {
	f := open(t)
	w := &worklog{t: t, m: f.h}
	w.machine(session.MachineAttached)
	w.ok(NameBash, `{"command":"true"}`)
	w.ok(NameWrite, `{"path":"src/app.js","content":"const a = 1;\nconst b = 2;\n"}`)
	w.ok(NameEdit, `{"path":"src/app.js","old_string":"const b = 2;","new_string":"const b = 3;"}`)
	w.ok(NameWrite, `{"path":"notes.md","content":"one two one"}`)
	w.ok(NameEdit, `{"path":"notes.md","old_string":"one","new_string":"1","replace_all":true}`)
	// A command makes two files, one with a last newline and one without,
	// and a picture; the agent reads each in full and edits the first.
	w.ok(NameBash, `{"command":"printf '{\\n  \"name\": \"app\"\\n}\\n' > package.json; printf 'a\\tb\\nlast' > data.tsv"}`)
	png := "\x89PNG\r\n\x1a\nnot really pixels"
	if err := os.WriteFile(filepath.Join(f.work, "logo.png"), []byte(png), 0o644); err != nil {
		t.Fatal(err)
	}
	w.ok(NameRead, `{"path":"package.json"}`)
	w.ok(NameRead, `{"path":"data.tsv"}`)
	w.ok(NameRead, `{"path":"logo.png"}`)
	w.ok(NameEdit, `{"path":"package.json","old_string":"\"app\"","new_string":"\"tides\""}`)
	w.ok(NameWrite, `{"path":"empty.txt","content":""}`)

	got := Rebuild(w.events, nil)
	files, omitted := rebuiltFiles(got)
	want := []string{
		"data.tsv=a\tb\nlast",
		"empty.txt=",
		"logo.png=" + png,
		"notes.md=1 two 1",
		"package.json={\n  \"name\": \"tides\"\n}\n",
		"src/app.js=const a = 1;\nconst b = 3;\n",
	}
	if !slices.Equal(files, want) || len(omitted) != 0 || got.Commands != 1 {
		t.Fatalf("rebuilt %q, omitted %q, %d commands; want %q", files, omitted, got.Commands, want)
	}
	// Each rebuilt file is the file the machine holds.
	for _, file := range got.Files {
		on, err := os.ReadFile(filepath.Join(f.work, filepath.FromSlash(file.Path)))
		if err != nil || string(on) != string(file.Content) || file.Size != int64(len(on)) {
			t.Errorf("%s is %q of %d bytes, the machine holds %q, %v", file.Path, file.Content, file.Size, on, err)
		}
	}
}

// TestRebuildNamesWhatItCannotGiveBack: a file a command made that the
// log shows only through a read of part of it, or an edit, is omitted as
// one whose content no event holds; a file a command changed between two
// edits, so the second no longer applies as rebuilt, is omitted; a file
// a command changed after its last write is given back as rebuilt and
// marked unverified once a read records another hash, and a read of the
// whole of it gives the changed content back. A path outside the working
// directory, a call that failed and a file past the bounds are no files
// of the rebuild (spec 065).
func TestRebuildNamesWhatItCannotGiveBack(t *testing.T) {
	f := open(t)
	w := &worklog{t: t, m: f.h}
	w.machine(session.MachineAttached)
	w.ok(NameWrite, `{"path":"kept.txt","content":"kept\n"}`)
	// Made by a command and read in part: no event holds it.
	w.ok(NameBash, `{"command":"seq 1 50 > rows.txt; echo generated > gen.txt; echo first > changed.txt"}`)
	w.ok(NameRead, `{"path":"rows.txt","limit":5}`)
	// Written, then changed by a command, then read in part: its hash is
	// no longer the rebuilt one.
	w.ok(NameWrite, `{"path":"stale.txt","content":"as written\nline two\n"}`)
	w.ok(NameBash, `{"command":"echo appended >> stale.txt"}`)
	w.ok(NameRead, `{"path":"stale.txt","limit":1}`)
	// Read in full, changed by a command, read in part again and edited:
	// the edit's text is not in the file as rebuilt.
	w.ok(NameRead, `{"path":"changed.txt"}`)
	w.ok(NameBash, `{"command":"printf 'second\\nmore\\n' > changed.txt"}`)
	w.ok(NameRead, `{"path":"changed.txt","limit":1}`)
	w.ok(NameEdit, `{"path":"changed.txt","old_string":"second","new_string":"third"}`)
	// Failed calls and a path outside the working directory.
	if _, _, res := w.call(NameEdit, `{"path":"kept.txt","old_string":"absent","new_string":"x"}`); !res.IsError() {
		t.Fatal("an edit of text the file does not hold succeeded")
	}
	if _, _, res := w.call(NameRead, `{"path":"missing.txt"}`); !res.IsError() {
		t.Fatal("a read of a missing file succeeded")
	}

	got := Rebuild(w.events, nil)
	files, omitted := rebuiltFiles(got)
	wantFiles := []string{"kept.txt=kept\n", "stale.txt=as written\nline two\n (unverified)"}
	wantOmitted := []string{"changed.txt: " + session.OmittedEdit, "rows.txt: " + session.OmittedUnknown}
	if !slices.Equal(files, wantFiles) || !slices.Equal(omitted, wantOmitted) {
		t.Fatalf("rebuilt %q, omitted %q; want %q and %q", files, omitted, wantFiles, wantOmitted)
	}

	// A read of the whole of each gives it back as the machine holds it.
	w.ok(NameRead, `{"path":"stale.txt"}`)
	w.ok(NameRead, `{"path":"rows.txt"}`)
	w.ok(NameWrite, `{"path":"changed.txt","content":"written again\n"}`)
	got = Rebuild(w.events, nil)
	files, omitted = rebuiltFiles(got)
	rows, err := os.ReadFile(filepath.Join(f.work, "rows.txt"))
	if err != nil {
		t.Fatal(err)
	}
	wantFiles = []string{"changed.txt=written again\n", "kept.txt=kept\n", "rows.txt=" + string(rows), "stale.txt=as written\nline two\nappended\n"}
	if !slices.Equal(files, wantFiles) || len(omitted) != 0 {
		t.Fatalf("after the full reads: rebuilt %q, omitted %q; want %q", files, omitted, wantFiles)
	}
}

// TestRebuildTakesKeptFilesAndHonorsRedactionsAndLosses: a files.kept
// gives each file it stored the bytes of its blob, and an edit after it
// is applied to them; a redacted event gives nothing, and a file whose
// latest write was redacted, the call or its result, is not rebuilt
// from an older one; a machine
// recorded as replaced empties the rebuild, and a path outside the
// working directory is no file of it (spec 065).
func TestRebuildTakesKeptFilesAndHonorsRedactionsAndLosses(t *testing.T) {
	f := open(t)
	w := &worklog{t: t, m: f.h}
	w.machine(session.MachineAttached)
	report := []byte("# Report\n\nDraft.\n")
	if err := os.WriteFile(filepath.Join(f.work, "report.md"), report, 0o644); err != nil {
		t.Fatal(err)
	}
	blobs := map[session.Digest][]byte{session.DigestOf(report): report}
	w.add(session.TypeFilesKept, session.FilesKept{Message: "evt_1", Skipped: []session.SkippedFile{}, Files: []session.KeptFile{
		{Path: "report.md", Resolved: f.work + "/report.md", Blob: session.DigestOf(report), MediaType: "text/markdown", Size: int64(len(report))},
		{Path: "chart.png", Resolved: f.work + "/out/chart.png", Blob: session.DigestOf([]byte("png")), MediaType: "image/png", Size: 3, Width: 1, Height: 1},
		{Path: "/etc/hosts", Resolved: "/etc/hosts", Blob: session.DigestOf([]byte("hosts")), Size: 5},
	}})
	// The agent reads the report in part, then edits it.
	w.ok(NameRead, `{"path":"report.md","limit":1}`)
	w.ok(NameEdit, `{"path":"report.md","old_string":"Draft.","new_string":"Final."}`)
	w.ok(NameWrite, `{"path":"secret.txt","content":"first version\n"}`)
	use, _, _ := w.call(NameWrite, `{"path":"secret.txt","content":"token=abc\n"}`)
	w.ok(NameWrite, `{"path":"gone.txt","content":"gone\n"}`)
	kept := w.add(session.TypeFilesKept, session.FilesKept{Message: "evt_2", Skipped: []session.SkippedFile{}, Files: []session.KeptFile{
		{Path: "redacted.pdf", Resolved: f.work + "/redacted.pdf", Blob: session.DigestOf([]byte("pdf")), Size: 3},
	}})
	_, result, _ := w.call(NameWrite, `{"path":"gone.txt","content":"redacted result\n"}`)
	w.redact(use)
	w.redact(kept)
	w.redact(result)

	blob := func(d session.Digest) ([]byte, error) {
		if b, ok := blobs[d]; ok {
			return b, nil
		}
		return nil, session.ErrNotFound
	}
	files, omitted := rebuiltFiles(Rebuild(w.events, blob))
	want := []string{"out/chart.png=" + string(session.DigestOf([]byte("png"))), "report.md=# Report\n\nFinal.\n"}
	if !slices.Equal(files, want) || len(omitted) != 0 {
		t.Fatalf("rebuilt %q, omitted %q; want %q", files, omitted, want)
	}
	// With no reader of blobs, the edit after the kept file has no bytes
	// to apply to.
	files, omitted = rebuiltFiles(Rebuild(w.events, nil))
	if !slices.Equal(omitted, []string{"report.md: " + session.OmittedUnknown}) || slices.ContainsFunc(files, func(s string) bool { return strings.HasPrefix(s, "report.md") }) {
		t.Fatalf("with no blobs: rebuilt %q, omitted %q", files, omitted)
	}
	// A blob that cannot be read leaves the edit nothing to apply to.
	failing := func(session.Digest) ([]byte, error) { return nil, errors.New("the store is down") }
	if _, omitted = rebuiltFiles(Rebuild(w.events, failing)); !slices.Equal(omitted, []string{"report.md: " + session.OmittedUnknown}) {
		t.Fatalf("with a store that fails: omitted %q", omitted)
	}

	// A machine replaced with nothing restored lost what was there; what
	// is written after it is the rebuild.
	w.machine(session.MachineReplaced)
	w.ok(NameBash, `{"command":"true"}`)
	w.ok(NameWrite, `{"path":"after.txt","content":"after\n"}`)
	w.ok(NameBash, `{"command":"true"}`)
	got := Rebuild(w.events, blob)
	if files, omitted = rebuiltFiles(got); !slices.Equal(files, []string{"after.txt=after\n"}) || len(omitted) != 0 || got.Commands != 1 {
		t.Fatalf("after a replacement: rebuilt %q, omitted %q, %d commands", files, omitted, got.Commands)
	}
	// A log that records no machine names no working directory.
	if got := Rebuild(w.events[1:3], blob); len(got.Files) != 0 {
		t.Fatalf("with no machine recorded: %+v", got.Files)
	}
}

// TestRebuildHoldsToItsBounds: a rebuild holds at most MaxRebuiltFiles
// files and MaxRebuiltBytes bytes; a file past either is omitted as past
// the limit, a smaller one after it still fits, and a file that leaves
// the rebuild makes room (spec 065).
func TestRebuildHoldsToItsBounds(t *testing.T) {
	w := &worklog{t: t, m: open(t).h}
	w.machine(session.MachineAttached)
	work := w.m.Info().Workdir
	kept := func(name string, size int64) session.KeptFile {
		return session.KeptFile{Path: name, Resolved: work + "/" + name, Blob: session.DigestOf([]byte(name)), Size: size}
	}
	half := int64(MaxRebuiltBytes / 2)
	w.add(session.TypeFilesKept, session.FilesKept{Message: "evt_1", Skipped: []session.SkippedFile{}, Files: []session.KeptFile{
		kept("a.bin", half), kept("b.bin", half), kept("c.bin", 1), kept("empty.bin", 0),
	}})
	_, omitted := rebuiltFiles(Rebuild(w.events, nil))
	if !slices.Equal(omitted, []string{"c.bin: " + session.OmittedLimit}) {
		t.Fatalf("past the bytes: omitted %q", omitted)
	}
	// A smaller version of a file makes room for the one that was past.
	w.add(session.TypeFilesKept, session.FilesKept{Message: "evt_2", Skipped: []session.SkippedFile{}, Files: []session.KeptFile{kept("a.bin", 1), kept("c.bin", 1)}})
	got := Rebuild(w.events, nil)
	if len(got.Omitted) != 0 || len(got.Files) != 4 {
		t.Fatalf("after the smaller version: %d files, omitted %+v", len(got.Files), got.Omitted)
	}

	many := &worklog{t: t, m: w.m}
	many.machine(session.MachineAttached)
	files := make([]session.KeptFile, 0, MaxRebuiltFiles+2)
	for i := range MaxRebuiltFiles + 2 {
		files = append(files, kept(fmt.Sprintf("f%05d.txt", i), 1))
	}
	many.add(session.TypeFilesKept, session.FilesKept{Message: "evt_1", Skipped: []session.SkippedFile{}, Files: files})
	got = Rebuild(many.events, nil)
	if len(got.Files) != MaxRebuiltFiles || len(got.Omitted) != 2 || got.Omitted[0].Reason != session.OmittedLimit || got.Omitted[1].Path != fmt.Sprintf("f%05d.txt", MaxRebuiltFiles+1) {
		t.Fatalf("past the count: %d files, omitted %+v", len(got.Files), got.Omitted)
	}
}
