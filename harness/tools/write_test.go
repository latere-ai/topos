// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
)

func TestWriteRequiresCurrentContent(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read, write := builtinTool(t, NameRead), builtinTool(t, NameWrite)
	th := &thread{id: "thr_main"}
	p := filepath.Join(f.work, "new", "dir", "a.txt")

	res := th.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": "new/dir/a.txt", "content": "one\ntwo"}))
	if res.Outcome != OutcomeOK || text(res) != "Created "+p+" (7 bytes, 2 lines)." || get(t, p) != "one\ntwo" {
		t.Fatalf("create %s %q", res.Outcome, text(res))
	}
	if res.Meta == nil || res.Meta.Path != p || res.Meta.SHA256 != sum("one\ntwo") {
		t.Fatalf("create meta %+v", res.Meta)
	}

	// The thread wrote the file, so it may write it again.
	res = th.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": p, "content": "three\n"}))
	if res.Outcome != OutcomeOK || text(res) != "Wrote "+p+" (6 bytes, 1 line)." {
		t.Fatalf("rewrite %s %q", res.Outcome, text(res))
	}

	// Changed behind the thread's back: refused until read again.
	put(t, p, "someone else\n")
	res = th.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": p, "content": "mine\n"}))
	if res.Outcome != OutcomeError || text(res) != changedText(p) || get(t, p) != "someone else\n" || res.Meta != nil {
		t.Fatalf("a changed file %s %q", res.Outcome, text(res))
	}
	th.call(ctx, t, read, f.h, mustInput(t, map[string]string{"path": p}))

	// A fresh harness folds the same log and enforces the same rule.
	fresh := &thread{id: th.id, events: append(th.events[:0:0], th.events...)}
	res = fresh.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": p, "content": "mine\n"}))
	if res.Outcome != OutcomeOK || get(t, p) != "mine\n" {
		t.Fatalf("after a read %s %q", res.Outcome, text(res))
	}

	// Another thread never read the file.
	other := &thread{id: "thr_other", events: fresh.events}
	res = other.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": p, "content": "other\n"}))
	if res.Outcome != OutcomeError || text(res) != changedText(p) {
		t.Fatalf("another thread %s %q", res.Outcome, text(res))
	}

	// An existing file this thread never read.
	q := filepath.Join(f.work, "b.txt")
	put(t, q, "existing\n")
	res = th.call(ctx, t, write, f.h, mustInput(t, map[string]string{"path": "b.txt", "content": "x"}))
	if res.Outcome != OutcomeError || text(res) != changedText(q) || get(t, q) != "existing\n" {
		t.Fatalf("an unread file %s %q", res.Outcome, text(res))
	}

	// A path last seen absent that now exists.
	res = run(ctx, t, write, f.h, State{Hashes: map[string]string{q: ""}}, mustInput(t, map[string]string{"path": q, "content": "x"}))
	if res.Outcome != OutcomeError || text(res) != changedText(q) {
		t.Fatalf("a file created since %s %q", res.Outcome, text(res))
	}
}

func TestWriteRefuses(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	write := builtinTool(t, NameWrite)
	if err := os.MkdirAll(filepath.Join(f.work, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, want string }{
		{"dir", filepath.Join(f.work, "dir") + " is a directory; write and edit change files."},
		{filepath.Join(f.outside, "x.txt"), filepath.Join(f.outside, "x.txt") + " is outside the working directory."},
		{".env", filepath.Join(f.work, ".env") + " is on the credential deny-list; the tools do not open it."},
		{"key.pem", filepath.Join(f.work, "key.pem") + " is on the credential deny-list; the tools do not open it."},
	} {
		res := run(ctx, t, write, f.h, State{}, mustInput(t, map[string]string{"path": c.path, "content": "x"}))
		if res.Outcome != OutcomeError || text(res) != c.want {
			t.Fatalf("%s: %s %q", c.path, res.Outcome, text(res))
		}
	}
	p := filepath.Join(f.work, "a.txt")
	put(t, p, "a")
	st := State{Hashes: map[string]string{p: sum("a")}}
	for name, c := range map[string]struct {
		m    machine.Machine
		want string
	}{
		"write": {faulty{Machine: f.h, writeErr: errors.New("no space left")}, p + ": no space left."},
		"read":  {faulty{Machine: f.h, readErr: errors.New("the disk went away")}, p + ": the disk went away."},
		"copy":  {faulty{Machine: f.h, body: failingReader{}}, p + ": the disk went away."},
		"stat":  {faulty{Machine: f.h, statErr: errors.New("stat failed")}, p + ": stat failed."},
	} {
		res := run(ctx, t, write, c.m, st, `{"path":"a.txt","content":"b"}`)
		if res.Outcome != OutcomeError || text(res) != c.want {
			t.Fatalf("%s: %s %q", name, res.Outcome, text(res))
		}
	}
	if got := plural(1, "byte") + ", " + plural(0, "line"); got != "1 byte, 0 lines" {
		t.Fatalf("plural %q", got)
	}
	if got := lineCount(""); got != 0 {
		t.Fatalf("lines of nothing %d", got)
	}
	if got := lineCount("a\nb\n"); got != 2 {
		t.Fatalf("lines %d", got)
	}
}

func TestEditRequiresUniqueMatch(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	read, edit := builtinTool(t, NameRead), builtinTool(t, NameEdit)
	th := &thread{id: "thr_main"}
	p := filepath.Join(f.work, "a.go")
	put(t, p, "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 1 }\n")

	// Not read yet.
	res := th.call(ctx, t, edit, f.h, `{"path":"a.go","old_string":"func A","new_string":"func C"}`)
	if res.Outcome != OutcomeError || text(res) != changedText(p) {
		t.Fatalf("an unread file %s %q", res.Outcome, text(res))
	}
	th.call(ctx, t, read, f.h, `{"path":"a.go"}`)

	for _, c := range []struct{ input, want string }{
		{`{"path":"a.go","old_string":"return 2","new_string":"return 3"}`, "old_string does not occur in " + p + ". Read the file again and copy the text exactly, whitespace and indentation included."},
		{`{"path":"a.go","old_string":"return 1","new_string":"return 2"}`, "old_string occurs 2 times in " + p + ". Add surrounding lines to make it unique, or set replace_all to replace every occurrence."},
		{`{"path":"a.go","old_string":"x","new_string":"x"}`, "old_string and new_string are the same; there is nothing to change."},
		{`{"path":"a.go","old_string":"","new_string":"x"}`, "old_string is empty; give the exact text to replace, or use write to create a file."},
		{`{"path":"absent.go","old_string":"x","new_string":"y"}`, filepath.Join(f.work, "absent.go") + " does not exist; use write to create it."},
		{mustInput(t, map[string]string{"path": f.outside, "old_string": "x", "new_string": "y"}), f.outside + " is outside the working directory."},
	} {
		res := th.call(ctx, t, edit, f.h, c.input)
		if res.Outcome != OutcomeError || text(res) != c.want {
			t.Fatalf("%s: %s %q", c.input, res.Outcome, text(res))
		}
	}

	res = th.call(ctx, t, edit, f.h, `{"path":"a.go","old_string":"func B() int { return 1 }","new_string":"func B() int { return 2 }"}`)
	if res.Outcome != OutcomeOK || text(res) != "Edited "+p+": replaced 1 occurrence at line 5." {
		t.Fatalf("a unique edit %s %q", res.Outcome, text(res))
	}
	want := "package a\n\nfunc A() int { return 1 }\n\nfunc B() int { return 2 }\n"
	if get(t, p) != want || res.Meta == nil || res.Meta.SHA256 != sum(want) || res.Meta.Path != p {
		t.Fatalf("after the edit %q, meta %+v", get(t, p), res.Meta)
	}

	// The edit recorded the new hash, so the next edit needs no read.
	res = th.call(ctx, t, edit, f.h, `{"path":"a.go","old_string":"int","new_string":"int64","replace_all":true}`)
	if res.Outcome != OutcomeOK || text(res) != "Edited "+p+": replaced 2 occurrences, the first at line 3." || strings.Count(get(t, p), "int64") != 2 {
		t.Fatalf("replace_all %s %q", res.Outcome, text(res))
	}

	// Changed behind the thread's back.
	put(t, p, "package b\n")
	res = th.call(ctx, t, edit, f.h, `{"path":"a.go","old_string":"package b","new_string":"package c"}`)
	if res.Outcome != OutcomeError || text(res) != changedText(p) || get(t, p) != "package b\n" {
		t.Fatalf("a changed file %s %q", res.Outcome, text(res))
	}

	bin := filepath.Join(f.work, "bin")
	put(t, bin, "a\x00b")
	res = run(ctx, t, edit, f.h, State{Hashes: map[string]string{bin: sum("a\x00b")}}, `{"path":"bin","old_string":"a","new_string":"c"}`)
	if res.Outcome != OutcomeError || text(res) != bin+" is a binary file; edit changes text files." {
		t.Fatalf("a binary file %s %q", res.Outcome, text(res))
	}

	q := filepath.Join(f.work, "q.txt")
	put(t, q, "q")
	res = run(ctx, t, edit, faulty{Machine: f.h, writeErr: errors.New("no space left")}, State{Hashes: map[string]string{q: sum("q")}}, `{"path":"q.txt","old_string":"q","new_string":"r"}`)
	if res.Outcome != OutcomeError || text(res) != q+": no space left." {
		t.Fatalf("a failed write %s %q", res.Outcome, text(res))
	}
	res = run(ctx, t, edit, faulty{Machine: f.h, statErr: errors.New("stat failed")}, State{}, `{"path":"q.txt","old_string":"q","new_string":"r"}`)
	if res.Outcome != OutcomeError || text(res) != q+": stat failed." {
		t.Fatalf("a failed stat %s %q", res.Outcome, text(res))
	}
}

func TestFileToolsUseMachinePaths(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	write, read := builtinTool(t, NameWrite), builtinTool(t, NameRead)
	abs := filepath.Join(f.work, "src", "main.go")
	res := run(ctx, t, write, f.h, State{}, mustInput(t, map[string]string{"path": abs, "content": "package main\n"}))
	if res.Outcome != OutcomeOK || res.Meta.Path != abs || get(t, abs) != "package main\n" {
		t.Fatalf("an absolute write %s %q", res.Outcome, text(res))
	}
	res = run(ctx, t, read, f.h, State{}, `{"path":"src/../src/main.go"}`)
	if res.Outcome != OutcomeOK || res.Meta.Path != abs {
		t.Fatalf("a relative read %+v", res.Meta)
	}
}
