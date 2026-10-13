// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strings"
	"testing"
)

// saved is a work.saved of turn at seq.
func saved(t *testing.T, seq uint64, turn int, w WorkSaved) Event {
	t.Helper()
	e := mustEvent(t, TypeWorkSaved, w)
	e.Seq, e.Turn = seq, turn
	return e
}

// TestTheCopyIsTheFoldOfItsSaves: the copy a log records is its
// work.saved events folded in order: a written file replaces its path's,
// a removed path leaves the copy with the save's covers, an unsaved one
// leaves it and is stale, a whole save starts again, holding its parent's
// stale paths, and a save whose walk failed changes nothing of the copy
// but that a save is pending. The event is known, no content a redaction
// removes, names no blob, and renders nothing (spec 073).
func TestTheCopyIsTheFoldOfItsSaves(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	file := func(p string, size int64) WorkFile { return WorkFile{Path: p, Size: size, Mode: 0o644, SHA256: sum} }
	log := []Event{
		saved(t, 10, 1, WorkSaved{Covers: 9, Written: []WorkFile{file("a.txt", 1), file("b.txt", 2), file("c.txt", 3)}}),
		saved(t, 20, 2, WorkSaved{Covers: 19, Written: []WorkFile{file("a.txt", 4)}, Removed: []string{"b.txt"}, Unsaved: []string{"c.txt"}, Deferred: 2,
			Skipped: 1, SkippedFiles: []OmittedFile{{Path: "big.bin", Reason: WorkTooLarge}}}),
	}
	c := CopyOf(log)
	if !c.Saved || c.Covers != 19 || c.Turn != 2 || c.Inherited || !c.Pending || c.Skipped != 1 || len(c.SkippedFiles) != 1 {
		t.Fatalf("the copy %+v", c)
	}
	if len(c.Files) != 1 || c.Files["a.txt"].Size != 4 || c.Bytes() != 4 || c.Removed["b.txt"] != 19 || !c.Stale["c.txt"] {
		t.Fatalf("the files %+v, removed %v, stale %v", c.Files, c.Removed, c.Stale)
	}
	failed := saved(t, 30, 3, WorkSaved{Covers: 29, Failed: &WorkFailure{Code: WorkMachineUnavailable, Detail: "gone"}})
	if c := CopyOf(append(log, failed)); c.Covers != 19 || c.Turn != 2 || !c.Pending || c.Skipped != 1 {
		t.Fatalf("a failed walk changed the copy: %+v", c)
	}
	written := saved(t, 40, 4, WorkSaved{Covers: 39, Written: []WorkFile{file("c.txt", 5), file("b.txt", 6)}})
	if c := CopyOf(append(log, written)); c.Pending || c.Stale["c.txt"] || c.Removed["b.txt"] != 0 || len(c.Files) != 3 || c.Skipped != 0 {
		t.Fatalf("a later save: %+v", c)
	}
	whole := saved(t, 50, 0, WorkSaved{Covers: 15, Whole: true, From: "ses_parent", Written: []WorkFile{file("x.txt", 7)}, Unsaved: []string{"y.txt"}})
	c = CopyOf(append(log, whole))
	if len(c.Files) != 1 || c.Files["x.txt"].Size != 7 || !c.Inherited || c.Covers != 15 || c.Turn != 0 || len(c.Removed) != 0 || !c.Stale["a.txt"] || !c.Stale["y.txt"] {
		t.Fatalf("a whole save: %+v", c)
	}
	if got := c.Sorted(); len(got) != 1 || got[0].Path != "x.txt" {
		t.Fatalf("sorted %v", got)
	}
	if c := CopyOf(nil); c.Saved || len(c.Files) != 0 {
		t.Fatalf("a log with no save: %+v", c)
	}

	if !Known[TypeWorkSaved] || Redactable(TypeWorkSaved) {
		t.Fatal("work.saved is unknown or redactable")
	}
	if got := log[0].Blobs(); len(got) != 0 {
		t.Fatalf("a work.saved names blobs %v", got)
	}
	if _, ok := SearchText(log[0]); ok {
		t.Fatal("a work.saved is searched")
	}
	tr, err := Fold(log, "")
	if err != nil || len(tr.Messages) != 0 || len(tr.Unknown) != 0 {
		t.Fatalf("the fold of work.saved: %+v, %v", tr, err)
	}
}

// TestTheCopyLeavesOutDependencies: a path inside a directory WorkLeftOut
// names, at any depth, is left out; a file of that name, and a build's
// output, are kept (spec 073).
func TestTheCopyLeavesOutDependencies(t *testing.T) {
	for _, p := range []string{"node_modules/react/index.js", "app/node_modules/x.js", ".git/config", "sub/repo/.git/HEAD", "src/__pycache__/a.pyc", ".venv/bin/python", "target/debug/app"} {
		if !LeftOutOfWork(p) {
			t.Errorf("%q is kept", p)
		}
	}
	for _, p := range []string{"node_modules", ".git", "dist/index.html", "build/app.js", ".next/server/page.js", "out/site.html", "src/target.go", "notes/venv.md"} {
		if LeftOutOfWork(p) {
			t.Errorf("%q is left out", p)
		}
	}
}
