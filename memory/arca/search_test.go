// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/test/stubs/arcastub"
)

// count is how many requests of op the stub received.
func count(arca *arcastub.Server, op string) int {
	n := 0
	for _, r := range arca.Requests() {
		if r.Op == op {
			n++
		}
	}
	return n
}

// TestGlobAndGrepInAFolder: glob and grep over a folder, over a directory
// of one and over /memory find the files and lines its listing holds; a
// grep reads each file once per checksum for the session, and a file
// another session changed again; a folder past memory.MaxListed is searched in
// its first memory.MaxListed files and the result says the rest were not read;
// and a search under no folder of the session is not there (spec 093).
func TestGlobAndGrepInAFolder(t *testing.T) {
	arca := arcastub.New(t)
	m := newMachine(t, arca, nil)
	ctx := t.Context()
	const project, user = "files/memory/projects/p1", "files/memory/you"
	for p, data := range map[string]string{
		project + "/MEMORY.md":        "- [Tides](tides.md)\n- [Trains](notes/trains.md)\n",
		project + "/tides.md":         "High tide at 6.\nLow tide at noon.\n",
		project + "/notes/trains.md":  "The 8:12 to the coast.\n",
		project + "/notes/.gitignore": "draft.md\n",
		project + "/notes/draft.md":   "tide draft\n",
		user + "/name.md":             "Ada, who likes the tide.\n",
	} {
		arca.Put(owner, p, arcastub.Object{Data: []byte(data)})
	}
	glob, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "**/*.md", Path: "/memory/project"})
	slices.Sort(glob.Lines)
	if err != nil || !slices.Equal(glob.Lines, []string{"/memory/project/MEMORY.md", "/memory/project/notes/trains.md", "/memory/project/tides.md"}) || glob.Unlisted {
		t.Fatalf("glob %+v %v", glob, err)
	}
	if count(arca, arcastub.OpRead) != 1 {
		t.Fatalf("a glob read %d files, want the one .gitignore", count(arca, arcastub.OpRead))
	}
	grep := func(path string) machine.SearchResult {
		t.Helper()
		res, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: `tide\b`, Path: path, OutputMode: machine.ModeContent, HeadLimit: 50})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if got := grep("/memory/project").Lines; !slices.Equal(got, []string{"/memory/project/tides.md:1:High tide at 6.", "/memory/project/tides.md:2:Low tide at noon."}) {
		t.Fatalf("grep the folder %q", got)
	}
	reads := count(arca, arcastub.OpRead)
	if got := grep("/memory").Lines; len(got) != 3 || got[2] != "/memory/user/name.md:1:Ada, who likes the tide." {
		t.Fatalf("grep /memory %q", got)
	}
	if n := count(arca, arcastub.OpRead) - reads; n != 1 {
		t.Fatalf("a second grep read %d files, want the user folder's one", n)
	}
	if got := grep("/memory/project/notes").Lines; len(got) != 0 {
		t.Fatalf("grep a directory %q", got)
	}
	arca.Put(owner, project+"/tides.md", arcastub.Object{Data: []byte("No tide today.\n")})
	reads = count(arca, arcastub.OpRead)
	if got := grep("/memory/project").Lines; !slices.Equal(got, []string{"/memory/project/tides.md:1:No tide today."}) {
		t.Fatalf("grep after a change %q", got)
	}
	if n := count(arca, arcastub.OpRead) - reads; n != 1 {
		t.Fatalf("a grep after one change read %d files", n)
	}
	if got := grep("/memory/project/tides.md").Lines; len(got) != 1 {
		t.Fatalf("grep one file %q", got)
	}
	if _, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/memory/other"}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a search of no folder: %v", err)
	}
	if _, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/memory/project/none"}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a search of no directory: %v", err)
	}
	if _, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "(", Path: "/memory/project"}); err == nil {
		t.Fatal("a bad pattern")
	}

	for i := range memory.MaxListed + 1 {
		arca.Put(owner, user+fmt.Sprintf("/many/%04d.md", i), arcastub.Object{Data: []byte("x")})
	}
	lists := count(arca, arcastub.OpList)
	res, err := m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "many/*.md", Path: "/memory/user"})
	if err != nil || !res.Unlisted || len(res.Lines) != machine.GlobLimit || !res.Truncated {
		t.Fatalf("a folder past the bound: %d lines, unlisted %t, %v", len(res.Lines), res.Unlisted, err)
	}
	if n := count(arca, arcastub.OpList) - lists; n != 3 {
		t.Fatalf("the listing took %d pages, want 3", n)
	}
	if files, more, err := m.list(ctx, m.folders[0]); err != nil || len(files) != memory.MaxListed || !more {
		t.Fatalf("the listing %d %t %v", len(files), more, err)
	}
}

// TestTheTreeIsAFileSystem: the folders a search covers are an fs.FS that
// holds to the standard library's checks, its files read from the store.
func TestTheTreeIsAFileSystem(t *testing.T) {
	arca := arcastub.New(t)
	m := newMachine(t, arca, nil)
	arca.Put(owner, "files/memory/projects/p1/a.md", arcastub.Object{Data: []byte("a")})
	arca.Put(owner, "files/memory/projects/p1/d/b.md", arcastub.Object{Data: []byte("b")})
	tr := &tree{m: m, ctx: t.Context(), files: map[string]treeFile{}, dirs: map[string][]string{".": nil}}
	for _, f := range m.folders {
		files, _, err := m.list(t.Context(), f)
		if err != nil {
			t.Fatal(err)
		}
		tr.add(f, files)
	}
	tr.sort()
	if err := fstest.TestFS(tr, "project/a.md", "project/d/b.md", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Open("../x"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("an invalid name: %v", err)
	}
	if _, err := tr.content("project/none.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a file not listed: %v", err)
	}
	tr.files["project/big.md"] = treeFile{folder: m.folders[1], rel: "big.md", size: memory.MaxFileBytes + 1}
	if _, err := tr.content("project/big.md"); !errors.As(err, new(*memory.TooLarge)) {
		t.Fatalf("a file past the bound: %v", err)
	}
}

// TestTheCacheIsBounded: the contents a session keeps by checksum are at
// most maxCached bytes, emptied before they would hold more.
func TestTheCacheIsBounded(t *testing.T) {
	m := &Machine{cache: map[string][]byte{}}
	m.remember("", []byte("no checksum"))
	m.remember("a", make([]byte, maxCached-1))
	m.remember("a", make([]byte, maxCached-1))
	m.remember("b", []byte("bb"))
	if _, ok := m.recalled("a"); ok || m.cached != 2 || len(m.cache) != 1 {
		t.Fatalf("the cache holds %d bytes in %d files", m.cached, len(m.cache))
	}
}
