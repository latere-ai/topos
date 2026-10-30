// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package arca

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"latere.ai/x/cella/shell"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/memory"
	"latere.ai/x/topos/session"
	"latere.ai/x/topos/test/stubs/arcastub"
)

const (
	userPath    = "files/memory/you"
	projectPath = "files/memory/projects/p1"
)

// shellMachine is a memory machine over the stub with a few files: a
// note in the read-only user folder, and a plan and a nested note in the
// writable project folder.
func shellMachine(t *testing.T) (*Machine, *arcastub.Server, *fallback) {
	t.Helper()
	arca := arcastub.New(t)
	arca.KeepTrash()
	arca.RequireToken(func(tok string) bool { return tok == "session-token" })
	arca.Put(owner, userPath+"/nikita-heumann-your-latere-ai-cofounder.md", arcastub.Object{Data: []byte("# Nikita\n\nCofounder.\n")})
	arca.Put(owner, projectPath+"/plan.md", arcastub.Object{Data: []byte("ship the shell\n")})
	arca.Put(owner, projectPath+"/notes/today.md", arcastub.Object{Data: []byte("todo: tests\n")})
	fb := &fallback{}
	return newMachine(t, arca, fb), arca, fb
}

// run runs line on m as the bash tool asks a machine to, started in dir.
func run(t *testing.T, m *Machine, dir, line string) machine.ExecResult {
	t.Helper()
	res, err := m.Exec(t.Context(), machine.ExecRequest{Command: line, Dir: dir, Timeout: time.Minute, ReportDir: true, ServerGrace: time.Second})
	if err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	return res
}

// sent counts the stub's requests of op since from.
func sent(arca *arcastub.Server, from int, op string) int {
	n := 0
	for _, r := range arca.Requests()[from:] {
		if r.Op == op {
			n++
		}
	}
	return n
}

// TestRuns: the memory machine runs a line that reaches the memory
// folders alone, from the working directory or a folder, and says why it
// does not run any other, before anything runs (spec 097).
func TestRuns(t *testing.T) {
	m, arca, _ := shellMachine(t)
	for _, c := range []struct {
		line, dir       string
		reason, program string
	}{
		{"cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md", "/work", "", ""},
		{"cd /memory/user && ls", "", "", ""},
		{"ls && cat plan.md", "/memory/project", "", ""},
		{"grep -rn todo . | sort", "/memory/project", "", ""},
		{"echo hi > /memory/user/a.md", "/work", "", ""},
		{"cat /memory/project/plan.md | python3 -c 'print(1)'", "/work", "program", "python3"},
		{"cat $(ls /memory/project)", "/work", "expansion", ""},
		{"cat /etc/passwd", "/work", "path", ""},
		{"ls", "/work", "path", ""},
		{"cd /memory/user && ls ../../work", "/work", "path", ""},
		{"ls /memory/user &", "/work", "construct", ""},
		{"ls --sort=time /memory", "/work", "option", "ls"},
		{"cat 'unclosed", "/work", "syntax", ""},
	} {
		reason, program := m.Runs(c.line, c.dir)
		if reason != c.reason || program != c.program {
			t.Errorf("Runs(%q, %q) = %q %q, want %q %q", c.line, c.dir, reason, program, c.reason, c.program)
		}
	}
	if n := len(arca.Requests()); n != 0 {
		t.Fatalf("deciding sent %d requests", n)
	}
}

// TestALineOverTheMemoryFolders: the production line lists a folder and
// reads a note with a listing and a read, opens no machine, and ends in
// the folder; a later line starts there (spec 097).
func TestALineOverTheMemoryFolders(t *testing.T) {
	m, arca, fb := shellMachine(t)
	res := run(t, m, "/work", "cd /memory/user && ls && cat nikita-heumann-your-latere-ai-cofounder.md")
	if want := "nikita-heumann-your-latere-ai-cofounder.md\n# Nikita\n\nCofounder.\n"; string(res.Output) != want || res.ExitCode != 0 || res.Dir != "/memory/user" {
		t.Fatalf("the line answered %q %d in %q", res.Output, res.ExitCode, res.Dir)
	}
	if fb.opened != 0 {
		t.Fatalf("the line opened the session's machine %d times", fb.opened)
	}
	// A read follows the files API's redirect to the bucket, which the
	// stub counts apart.
	if lists, reads, heads := sent(arca, 0, arcastub.OpList), sent(arca, 0, arcastub.OpRead), sent(arca, 0, arcastub.OpHead); lists != 1 || reads != 1 || heads != 0 {
		t.Fatalf("the line sent %d listings, %d reads and %d stats: %+v", lists, reads, heads, arca.Requests())
	}

	// A file the session read is read again from its copy while the
	// folder's listing names the same version.
	before := sent(arca, 0, arcastub.OpRead)
	if res := run(t, m, "/memory/user", "ls > /dev/null; cat nikita-heumann-your-latere-ai-cofounder.md"); string(res.Output) != "# Nikita\n\nCofounder.\n" {
		t.Fatalf("a second read: %q", res.Output)
	}
	if n := sent(arca, 0, arcastub.OpRead) - before; n != 0 {
		t.Fatalf("a file read once was read %d more times", n)
	}
	if res := run(t, m, "/memory/project", "ls -F; ls /memory; pwd"); string(res.Output) != "notes/\nplan.md\nproject\nuser\n/memory/project\n" || res.Dir != "/memory/project" {
		t.Fatalf("a line from a folder: %q in %q", res.Output, res.Dir)
	}
	if res := run(t, m, "/work", "echo hi"); string(res.Output) != "hi\n" || res.Dir != "/work" {
		t.Fatalf("a line that stays in the working directory: %q in %q", res.Output, res.Dir)
	}
	res, err := m.Exec(t.Context(), machine.ExecRequest{Command: "cat /memory/project/notes/today.md", Dir: "/work"})
	if err != nil || string(res.Output) != "todo: tests\n" || res.Dir != "" {
		t.Fatalf("a line that reports no directory: %+v %v", res, err)
	}
	if res := run(t, m, "/memory/project", "grep -rn todo . ; cat notes ; ls -l plan.md"); !strings.Contains(string(res.Output), "./notes/today.md:1:todo: tests\n") ||
		!strings.Contains(string(res.Output), "cat: notes: Is a directory\n") || !strings.Contains(string(res.Output), "-rw-r--r-- 1 root root 15 ") {
		t.Fatalf("a search, a directory read and a long listing: %q", res.Output)
	}
	if res := run(t, m, "/memory/user", "ls -l"); !strings.Contains(string(res.Output), "-r--r--r-- 1 root root 21 ") {
		t.Fatalf("a read-only folder's files: %q", res.Output)
	}
	if res := run(t, m, "/memory/project", "cd plan.md; ls plan.md/x; cat missing.md"); res.ExitCode != 1 ||
		!strings.Contains(string(res.Output), "cd: can't cd to plan.md") || !strings.Contains(string(res.Output), "cat: missing.md: No such file or directory") {
		t.Fatalf("paths that are no directory or not there: %q %d", res.Output, res.ExitCode)
	}
}

// TestALineChangesTheMemoryFolders: a write sends If-Match on the version
// the session read or, for a file it never read, the version it is at
// now; a write of nothing forgets a file and makes none; a move writes
// then forgets; a directory a line makes lasts for the line; a read-only
// folder refuses every change before any request; and a file another
// session changed since the read is refused in the file tools' words
// (spec 097).
func TestALineChangesTheMemoryFolders(t *testing.T) {
	m, arca, _ := shellMachine(t)
	from := len(arca.Requests())
	res := run(t, m, "/memory/project", "echo 'ship it' > plan.md && cat plan.md")
	if string(res.Output) != "ship it\n" || res.ExitCode != 0 {
		t.Fatalf("an overwrite: %q %d", res.Output, res.ExitCode)
	}
	if put := last(t, arca, arcastub.OpPut); put.Header.Get("If-Match") == "" || put.Header.Get("If-None-Match") != "" {
		t.Fatalf("an overwrite of a file never read was put with %v", put.Header)
	}
	if o, _ := arca.Object(owner, projectPath+"/plan.md"); string(o.Data) != "ship it\n" || o.Token != "session-token" {
		t.Fatalf("the store holds %q", o.Data)
	}
	if n := sent(arca, from, arcastub.OpHead); n != 0 {
		t.Fatalf("a folder the line listed was stat %d times", n)
	}

	run(t, m, "/memory/project", "echo one >> new.md; echo two >> new.md")
	if put := last(t, arca, arcastub.OpPut); put.Header.Get("If-Match") == "" {
		t.Fatalf("an append to a file the line wrote was put with %v", put.Header)
	}
	if o, _ := arca.Object(owner, projectPath+"/new.md"); string(o.Data) != "one\ntwo\n" {
		t.Fatalf("the appends left %q", o.Data)
	}

	if res := run(t, m, "/memory/project", "touch made.md plan.md && ls"); string(res.Output) != "new.md\nnotes\nplan.md\n" {
		t.Fatalf("touch: %q", res.Output)
	}
	if _, ok := arca.Object(owner, projectPath+"/made.md"); ok {
		t.Fatal("touch made an empty memory file")
	}
	if o, _ := arca.Object(owner, projectPath+"/plan.md"); string(o.Data) != "ship it\n" {
		t.Fatalf("touch changed a file: %q", o.Data)
	}

	run(t, m, "/memory/project", ": > new.md")
	if _, ok := arca.Object(owner, projectPath+"/new.md"); ok {
		t.Fatal("a line that emptied a file left it")
	}
	if _, ok := arca.Trashed(owner, projectPath+"/new.md"); !ok {
		t.Fatal("an emptied file is not in the trash")
	}

	if res := run(t, m, "/memory/project", "mv notes/today.md done.md && cp plan.md plan-copy.md && mkdir -p a/b && ls -F && rm plan-copy.md && rm -r a"); res.ExitCode != 0 || string(res.Output) != "a/\ndone.md\nplan-copy.md\nplan.md\n" {
		t.Fatalf("mv, cp, mkdir and rm: %q %d", res.Output, res.ExitCode)
	}
	if got := arca.Paths(owner, projectPath+"/"); strings.Join(got, " ") != projectPath+"/done.md "+projectPath+"/plan.md" {
		t.Fatalf("the folder holds %v", got)
	}

	from = len(arca.Requests())
	res = run(t, m, "/memory/user", "echo x > a.md; rm nikita-heumann-your-latere-ai-cofounder.md; mkdir d; touch nikita-heumann-your-latere-ai-cofounder.md")
	for _, want := range []string{
		"/bin/sh: 2: cannot create a.md: Read-only file system\n",
		"rm: cannot remove 'nikita-heumann-your-latere-ai-cofounder.md': Read-only file system\n",
		"mkdir: cannot create directory ‘d’: Read-only file system\n",
		"touch: cannot touch 'nikita-heumann-your-latere-ai-cofounder.md': Read-only file system\n",
	} {
		if !strings.Contains(string(res.Output), want) {
			t.Errorf("a change to a read-only folder: %q lacks %q", res.Output, want)
		}
	}
	for _, op := range []string{arcastub.OpPut, arcastub.OpDelete} {
		if n := sent(arca, from, op); n != 0 {
			t.Fatalf("a read-only folder was sent %d %s requests", n, op)
		}
	}
	if res := run(t, m, "/work", "mkdir /memory/new; rm -r /memory/project"); !strings.Contains(string(res.Output), "mkdir: cannot create directory ‘/memory/new’: Read-only file system") ||
		!strings.Contains(string(res.Output), "rm: cannot remove '/memory/project': Device or resource busy") {
		t.Fatalf("a change to /memory: %q", res.Output)
	}

	// A file another session changed since this one read it.
	if res := run(t, m, "/memory/project", "cat done.md"); res.ExitCode != 0 {
		t.Fatalf("a read: %q", res.Output)
	}
	arca.Put(owner, projectPath+"/done.md", arcastub.Object{Data: []byte("theirs\n")})
	res = run(t, m, "/memory/project", "echo mine > done.md")
	if res.ExitCode == 0 || !strings.Contains(string(res.Output), "done.md: changed since it was last read; read it again before writing") {
		t.Fatalf("a write over a change: %q %d", res.Output, res.ExitCode)
	}
	if o, _ := arca.Object(owner, projectPath+"/done.md"); string(o.Data) != "theirs\n" {
		t.Fatalf("a refused write overwrote %q", o.Data)
	}
	if res := run(t, m, "/memory/project", "cat done.md && echo mine > done.md && cat done.md"); res.ExitCode != 0 || string(res.Output) != "theirs\nmine\n" {
		t.Fatalf("a write after a read again: %q %d", res.Output, res.ExitCode)
	}
	arca.Put(owner, projectPath+"/done.md", arcastub.Object{Data: []byte("theirs again\n")})
	res = run(t, m, "/memory/project", "rm done.md")
	if res.ExitCode != 1 || string(res.Output) != "rm: cannot remove 'done.md': changed since it was last read; read it again before writing\n" {
		t.Fatalf("a remove over a change: %q %d", res.Output, res.ExitCode)
	}
	if _, ok := arca.Object(owner, projectPath+"/done.md"); !ok {
		t.Fatal("a refused remove removed the file")
	}
}

// TestTheStoreRefusesALine: a refusal of the files store is printed with
// its message and words, a store not reached as an input/output error,
// and a file past the bound as too large (spec 097).
func TestTheStoreRefusesALine(t *testing.T) {
	m, arca, _ := shellMachine(t)
	arca.Fail(arcastub.OpPut, arcastub.Failure{Status: 403, Code: "forbidden", Message: "You do not have permission to do this.", Detail: "memory_name: a memory file is named with lowercase letters, digits and dashes, ending in .md."})
	res := run(t, m, "/memory/project", "echo x > Notes.txt")
	if want := "/bin/sh: 2: cannot create Notes.txt: You do not have permission to do this. a memory file is named with lowercase letters, digits and dashes, ending in .md.\n"; string(res.Output) != want || res.ExitCode == 0 {
		t.Fatalf("a refusal: %q %d", res.Output, res.ExitCode)
	}
	arca.Fail(arcastub.OpRead, arcastub.Failure{Status: 503, Code: "unavailable"})
	if res := run(t, m, "/memory/project", "cat plan.md"); string(res.Output) != "cat: plan.md: Input/output error\n" || res.ExitCode != 1 {
		t.Fatalf("a store not reached: %q %d", res.Output, res.ExitCode)
	}
	arca.Fail(arcastub.OpList, arcastub.Failure{Status: 503, Code: "unavailable"})
	if res := run(t, m, "/memory/user", "ls"); string(res.Output) != "ls: cannot open directory '.': Input/output error\n" || res.ExitCode != 2 {
		t.Fatalf("a listing not reached: %q %d", res.Output, res.ExitCode)
	}
	arca.Put(owner, projectPath+"/half.md", arcastub.Object{Data: []byte(strings.Repeat("x", 600<<10))})
	if res := run(t, m, "/memory/project", "cat half.md half.md > whole.md"); !strings.Contains(string(res.Output), "whole.md: File too large") || res.ExitCode == 0 {
		t.Fatalf("a file past the bound: %q %d", res.Output, res.ExitCode)
	}
}

// TestAFolderPastTheListing: a file a folder's listing does not reach is
// stat and written on the version a stat answers (spec 097).
func TestAFolderPastTheListing(t *testing.T) {
	m, arca, _ := shellMachine(t)
	for i := range 2000 {
		arca.Put(owner, fmt.Sprintf("%s/a%04d.md", projectPath, i), arcastub.Object{Data: []byte("x\n")})
	}
	res := run(t, m, "/memory/project", "ls -l plan.md && echo y > plan.md && cat plan.md && ls missing.md")
	if !strings.Contains(string(res.Output), " 15 ") || !strings.Contains(string(res.Output), "y\n") || !strings.Contains(string(res.Output), "ls: cannot access 'missing.md': No such file or directory") {
		t.Fatalf("a file past the listing: %q", res.Output)
	}
	if put := last(t, arca, arcastub.OpPut); put.Header.Get("If-Match") == "" {
		t.Fatalf("a file past the listing was put with %v", put.Header)
	}
	if res := run(t, m, "/memory/project", "rm a0001.md plan.md"); res.ExitCode != 0 {
		t.Fatalf("a remove past the listing: %q", res.Output)
	}
}

// TestAFolderOfConversationsOnALine: a line lists and reads a folder of
// conversations through the machine's reach, and changes nothing there
// (spec 097).
func TestAFolderOfConversationsOnALine(t *testing.T) {
	chats := &chatsStub{
		files:   map[string]string{"2026-10-28-ses_2.md": "# Tide tables\n\nWhen is high tide at Kiel?\n"},
		version: map[string]string{"2026-10-28-ses_2.md": "v2"},
	}
	m, err := New([]session.MemoryFolder{chatsFolder}, nil, nil, nil, WithConversations(chats))
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, m, "/work", "cd /memory/chats && ls && grep -l Kiel * && cat 2026-10-28-ses_2.md | head -1 && rm 2026-10-28-ses_2.md")
	want := "2026-10-28-ses_2.md\n2026-10-28-ses_2.md\n# Tide tables\nrm: cannot remove '2026-10-28-ses_2.md': Read-only file system\n"
	if string(res.Output) != want || res.ExitCode != 1 {
		t.Fatalf("a line over conversations: %q %d", res.Output, res.ExitCode)
	}
	if chats.reads != 1 {
		t.Fatalf("a conversation was read %d times for a line", chats.reads)
	}
}

// TestWhatALineDoesNotRun: a command in the background, with input or
// variables of its own, or that the shell refuses runs on the session's
// machine; a line past its timeout is stopped, and one whose context
// ends is canceled (spec 097).
func TestWhatALineDoesNotRun(t *testing.T) {
	m, _, _ := shellMachine(t)
	ctx := t.Context()
	for _, r := range []machine.ExecRequest{
		{Command: "ls /memory", Background: true},
		{Command: "ls /memory", Stdin: strings.NewReader("x")},
		{Command: "ls /memory", Env: map[string]string{"A": "b"}},
		{Command: "python3 /memory/project/plan.md"},
	} {
		if _, err := m.Exec(ctx, r); !errors.Is(err, errCommands) {
			t.Errorf("%+v: %v", r, err)
		}
	}
	if _, err := m.ExecStream(ctx, machine.ExecRequest{Command: "ls /memory"}); !errors.Is(err, errCommands) {
		t.Fatalf("exec stream: %v", err)
	}
}

// hangingMachine is a memory machine whose store answers nothing until
// the test ends.
func hangingMachine(t *testing.T) *Machine {
	t.Helper()
	arca := arcastub.New(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	arca.RequireToken(func(string) bool { <-release; return true })
	return newMachine(t, arca, &fallback{})
}

// TestALineThatRunsPastItsTimeout: a line still running when its timeout
// passes is timed out, and one whose caller ends is canceled, whether it
// is waiting on the store or for a place to run (spec 097).
func TestALineThatRunsPastItsTimeout(t *testing.T) {
	m := hangingMachine(t)
	ctx := t.Context()
	res, err := m.Exec(ctx, machine.ExecRequest{Command: "ls", Dir: "/memory/project", Timeout: 50 * time.Millisecond, ReportDir: true})
	if err != nil || !res.TimedOut || res.Canceled || res.Dir != "" || res.ExitCode != -1 {
		t.Fatalf("a line past its timeout: %+v %v", res, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	res, err = m.Exec(cctx, machine.ExecRequest{Command: "ls", Dir: "/memory/project", Timeout: time.Minute, ReportDir: true})
	if err != nil || !res.Canceled || res.TimedOut {
		t.Fatalf("a canceled line: %+v %v", res, err)
	}

	// Every place taken: a line waits, under its timeout.
	free, _, _ := shellMachine(t)
	for range cap(lines) {
		lines <- struct{}{}
	}
	// One place is given back below, so the line given it runs.
	defer func() {
		for range cap(lines) - 1 {
			<-lines
		}
	}()
	res, err = free.Exec(ctx, machine.ExecRequest{Command: "ls", Dir: "/memory/project", Timeout: 50 * time.Millisecond})
	if err != nil || !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("a line with no place, past its timeout: %+v %v", res, err)
	}
	cctx, cancel = context.WithCancel(ctx)
	cancel()
	res, err = free.Exec(cctx, machine.ExecRequest{Command: "ls", Dir: "/memory/project", Timeout: time.Minute})
	if err != nil || !res.Canceled {
		t.Fatalf("a canceled line with no place: %+v %v", res, err)
	}
	time.AfterFunc(50*time.Millisecond, func() { <-lines })
	res = run(t, free, "/memory/project", "ls")
	if string(res.Output) != "notes\nplan.md\n" {
		t.Fatalf("a line given a place: %+v", res)
	}
}

// TestALineIsBounded: a line runs within the shell's bounds, which stop
// an endless loop with the shell's message and status 2, and keeps at
// most a memory file of output (spec 097).
func TestALineIsBounded(t *testing.T) {
	if want := max(1, runtime.GOMAXPROCS(0)/2); cap(lines) != want {
		t.Errorf("%d lines run at once, want %d", cap(lines), want)
	}
	m, arca, fb := shellMachine(t)
	res := run(t, m, "/memory/project", "echo start; while :; do :; done")
	if res.ExitCode != 2 || res.TimedOut || string(res.Output) != fmt.Sprintf("start\n/bin/sh: 2: the line ran more than %d steps\n", shell.DefaultSteps) {
		t.Errorf("an endless loop: %+v", res)
	}
	arca.Put(owner, projectPath+"/half.md", arcastub.Object{Data: []byte(strings.Repeat("x", 600<<10))})
	res = run(t, m, "/memory/project", "cat half.md half.md half.md")
	if res.ExitCode != 0 || len(res.Output) != memory.MaxFileBytes {
		t.Errorf("a long output kept %d bytes, exit %d, want %d", len(res.Output), res.ExitCode, memory.MaxFileBytes)
	}
	if fb.opened != 0 {
		t.Error("a bounded line opened the session's machine")
	}
}
