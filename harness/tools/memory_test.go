// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/memory/arca"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

const memoryOwner = "https://issuer.example|ada"

// memoryMachine is a memory machine over the files store stub with a
// read-only user folder and a writable project folder, the fixture's host
// its session's machine.
func memoryMachine(t *testing.T) (*arca.Machine, *arcastub.Server, fixture) {
	t.Helper()
	stub := arcastub.New(t)
	stub.KeepTrash()
	f := open(t)
	m, err := arca.New([]session.MemoryFolder{
		{Name: "user", URL: stub.URL(), Owner: memoryOwner, Path: "files/memory/you", Access: session.MemoryReadOnly},
		{Name: "project", URL: stub.URL(), Owner: memoryOwner, Path: "files/memory/projects/p1", Access: session.MemoryReadWrite},
	}, func(context.Context) (string, error) { return "session-token", nil }, stub.Client(), f.h)
	if err != nil {
		t.Fatal(err)
	}
	return m, stub, f
}

// TestTheFileToolsSaveAndForgetAMemory: a write under /memory/ records
// the change in the result's meta as saved, with its folder and its path
// in the folder; a read, a glob and a grep reach the folder; an empty
// write, and an edit that leaves the file empty, forget it, the file in
// the store's trash and the thread's next write of the path a new file;
// and a write to a read-only folder is refused before any request (spec
// 093).
func TestTheFileToolsSaveAndForgetAMemory(t *testing.T) {
	ctx := t.Context()
	m, stub, f := memoryMachine(t)
	th := &thread{id: "main"}
	const p, stored = "/memory/project/color.md", "files/memory/projects/p1/color.md"

	saved := th.call(ctx, t, builtinTool(t, NameWrite), m, mustInput(t, map[string]string{"path": p, "content": "The club's color is navy.\n"}))
	if saved.IsError() || saved.Meta == nil || saved.Meta.Path != p || saved.Meta.SHA256 == "" ||
		*saved.Meta.Memory != (MemoryMeta{Folder: "project", Path: "color.md", Action: MemorySaved}) {
		t.Fatalf("the write: %q %+v", text(saved), saved.Meta)
	}
	if o, ok := stub.Object(memoryOwner, stored); !ok || string(o.Data) != "The club's color is navy.\n" {
		t.Fatalf("the store holds %+v %t", o, ok)
	}
	if r := th.call(ctx, t, builtinTool(t, NameRead), m, `{"path":"`+p+`"}`); r.IsError() || !strings.Contains(text(r), "navy") {
		t.Fatalf("the read: %q", text(r))
	}
	if r := th.call(ctx, t, builtinTool(t, NameGlob), m, `{"pattern":"*.md","path":"/memory/project"}`); r.IsError() || !strings.Contains(text(r), p) {
		t.Fatalf("the glob: %q", text(r))
	}
	if r := th.call(ctx, t, builtinTool(t, NameGrep), m, `{"pattern":"navy","path":"/memory"}`); r.IsError() || !strings.Contains(text(r), p) {
		t.Fatalf("the grep: %q", text(r))
	}

	forgot := th.call(ctx, t, builtinTool(t, NameWrite), m, mustInput(t, map[string]string{"path": p, "content": ""}))
	if text(forgot) != "Forgot "+p+"." || forgot.IsError() || forgot.Meta.SHA256 != "" ||
		*forgot.Meta.Memory != (MemoryMeta{Folder: "project", Path: "color.md", Action: MemoryForgotten}) {
		t.Fatalf("the forget: %q %+v", text(forgot), forgot.Meta)
	}
	if _, ok := stub.Object(memoryOwner, stored); ok {
		t.Fatal("a forgotten file is still in the folder")
	}
	if _, ok := stub.Trashed(memoryOwner, stored); !ok {
		t.Fatal("a forgotten file is not in the trash")
	}

	again := th.call(ctx, t, builtinTool(t, NameWrite), m, mustInput(t, map[string]string{"path": p, "content": "Navy and white.\n"}))
	if again.IsError() || again.Meta.Memory.Action != MemorySaved {
		t.Fatalf("the write after the forget: %q", text(again))
	}
	emptied := th.call(ctx, t, builtinTool(t, NameEdit), m, mustInput(t, map[string]string{"path": p, "old_string": "Navy and white.\n", "new_string": ""}))
	if text(emptied) != "Forgot "+p+"." || emptied.Meta.Memory.Action != MemoryForgotten {
		t.Fatalf("an edit that empties the file: %q %+v", text(emptied), emptied.Meta)
	}

	before := len(stub.Requests())
	ro := th.call(ctx, t, builtinTool(t, NameWrite), m, mustInput(t, map[string]string{"path": "/memory/user/name.md", "content": "Ada.\n"}))
	if !ro.IsError() || text(ro) != "/memory/user/name.md is in /memory/user/, which this session reads and does not change." {
		t.Fatalf("a write to a read-only folder: %q", text(ro))
	}
	if n := len(stub.Requests()) - before; n != 1 {
		// The tool's own current-content check reads the path; the write
		// itself sends nothing.
		t.Fatalf("a write to a read-only folder sent %d requests, want the one read", n)
	}

	// The same write on the session's own machine records no memory.
	host := th.call(ctx, t, builtinTool(t, NameWrite), f.h, mustInput(t, map[string]string{"path": "notes.md", "content": "x"}))
	if host.Meta == nil || host.Meta.Memory != nil {
		t.Fatalf("a write outside /memory recorded %+v", host.Meta)
	}
}

// TestAMemoryRefusalSaysWhatHappened: a files store's refusal is its
// sentence and the authorizer's words, with the reason in the meta as
// refusal, or the store's code when no reason was given; an answer that
// never came is "Memory could not be reached"; a write the store changed
// under is the changed text; and a file past the bound names it (spec
// 093).
func TestAMemoryRefusalSaysWhatHappened(t *testing.T) {
	ctx := t.Context()
	m, stub, _ := memoryMachine(t)
	write := func(p, content string) Result {
		t.Helper()
		return run(ctx, t, builtinTool(t, NameWrite), m, State{Hashes: map[string]string{}}, mustInput(t, map[string]string{"path": p, "content": content}))
	}

	stub.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Message: "You do not have permission to do this.", Detail: "memory_name: A memory file is named with lowercase letters, digits and dashes, ending in .md."})
	r := write("/memory/project/Notes.txt", "x")
	if !r.IsError() || r.Meta == nil || r.Meta.Refusal != "memory_name" ||
		text(r) != "/memory/project/Notes.txt: You do not have permission to do this. A memory file is named with lowercase letters, digits and dashes, ending in .md." {
		t.Fatalf("a refusal with a reason: %q %+v", text(r), r.Meta)
	}
	stub.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Message: "You do not have permission to do this.", Detail: "no reason here"})
	if r := write("/memory/project/a.md", "x"); r.Meta == nil || r.Meta.Refusal != "forbidden" || text(r) != "/memory/project/a.md: You do not have permission to do this." {
		t.Fatalf("a refusal with no reason: %q %+v", text(r), r.Meta)
	}
	stub.Fail(arcastub.OpPut, arcastub.Failure{Status: http.StatusServiceUnavailable, Code: "storage_unavailable"})
	if r := write("/memory/project/a.md", "x"); !r.IsError() || text(r) != "Memory could not be reached: the files store answered the write with 503." {
		t.Fatalf("an unreachable store: %q", text(r))
	}
	if r := write("/memory/project/big.md", strings.Repeat("x", memory.MaxFileBytes+1)); !r.IsError() || !strings.Contains(text(r), "a memory file holds at most 1048576 bytes") {
		t.Fatalf("a file past the bound: %q", text(r))
	}
	changed := faulty{Machine: m, writeErr: memory.ErrChanged}
	if r := run(ctx, t, builtinTool(t, NameWrite), changed, State{Hashes: map[string]string{}}, `{"path":"/memory/project/b.md","content":"x"}`); text(r) != changedText("/memory/project/b.md") {
		t.Fatalf("a write the store refused as changed: %q", text(r))
	}
	stub.Fail(arcastub.OpList, arcastub.Failure{Status: http.StatusForbidden, Code: "forbidden", Message: "You do not have permission to do this.", Detail: "memory_off: Remember new things is off."})
	if r := run(ctx, t, builtinTool(t, NameGrep), m, State{}, `{"pattern":"x","path":"/memory/project"}`); r.Meta == nil || r.Meta.Refusal != "memory_off" || !strings.Contains(text(r), "Remember new things is off.") {
		t.Fatalf("a search the store refused: %q %+v", text(r), r.Meta)
	}
}

// TestASearchPastTheListingSaysSo: a glob or a grep that read a memory
// folder's first memory.MaxListed files alone says so after its lines,
// and one that read every file says nothing more.
func TestASearchPastTheListingSaysSo(t *testing.T) {
	line := prompts.Render(prompts.MemoryUnlisted, prompts.Data{"Limit": memory.MaxListed}) + "\n"
	for _, tc := range []struct {
		text string
		res  machine.SearchResult
		want string
	}{
		{"a\n", machine.SearchResult{Unlisted: true}, "a\n" + line},
		{"No files found.", machine.SearchResult{Unlisted: true}, "No files found.\n" + line},
		{"a\n", machine.SearchResult{}, "a\n"},
	} {
		if got := unlisted(tc.text, tc.res); got != tc.want {
			t.Errorf("unlisted(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	ctx := t.Context()
	searched := pastListing{res: machine.SearchResult{Lines: []string{"/memory/project/a.md:1:x"}, Unlisted: true}}
	for _, tool := range []string{NameGlob, NameGrep} {
		if r := run(ctx, t, builtinTool(t, tool), searched, State{}, `{"pattern":"x","path":"/memory/project"}`); !strings.HasSuffix(text(r), line) {
			t.Errorf("%s past the listing: %q", tool, text(r))
		}
	}
}

// pastListing is a memory machine whose every search answers res.
type pastListing struct {
	machine.Machine
	res machine.SearchResult
}

func (s pastListing) Info() machine.Info {
	return machine.Info{Kind: memory.Kind, Workdir: memory.Root}
}

func (s pastListing) Search(context.Context, machine.SearchRequest) (machine.SearchResult, error) {
	return s.res, nil
}

// TestTheFileToolsNameTheirPaths: read, write, edit, glob and grep name
// the one path their input writes, "" for none or an input that does not
// decode; bash names none (spec 093).
func TestTheFileToolsNameTheirPaths(t *testing.T) {
	for _, name := range []string{NameRead, NameWrite, NameEdit, NameGlob, NameGrep} {
		p, ok := builtinTool(t, name).(Pather)
		if !ok {
			t.Fatalf("%s names no paths", name)
		}
		for in, want := range map[string]string{`{"path":"/memory/user/a.md"}`: "/memory/user/a.md", `{"pattern":"x"}`: "", `[`: ""} {
			if got := p.Paths(json.RawMessage(in)); len(got) != 1 || got[0] != want {
				t.Errorf("%s %s: %q", name, in, got)
			}
		}
	}
	if _, ok := builtinTool(t, NameBash).(Pather); ok {
		t.Error("bash names paths")
	}
	if memoryFailure(errors.New("other")) {
		t.Error("an error of another machine is a memory failure")
	}
}
