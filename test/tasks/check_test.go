// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"latere.ai/x/pkg/llmdialect/ir"
	"latere.ai/x/pkg/llmdialect/lux"

	"latere.ai/x/topos/session"
)

func TestParseCheckRefuses(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"", "no assertion"},
		{"file: {path: a}\n", "check.yaml"},
		{"- {}\n", "sets 0 kinds"},
		{"- {absent: [a], unchanged: [b]}\n", "sets 2 kinds"},
		{"- shade: [a]\n", "shade"},
		{"- go: {args: []}\n", "no args"},
		{"- file: {equals: a}\n", "no path"},
		{"- file: {path: a, matches: '('}\n", "missing closing"},
		{"- no_match: {glob: '*.go'}\n", "glob and a pattern"},
		{"- no_match: {glob: '*.go', pattern: '['}\n", "missing closing"},
		{"- todo: {final: done}\n", "final"},
		{"- call: {outcome: ok}\n", "no tool"},
		{"- no_call: {tool: bash, thread: child}\n", "thread"},
		{"- call: {tool: bash, input: {command: {matches: '*'}}}\n", "missing argument"},
	} {
		_, err := ParseCheck([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err %v, want one naming %q", c.src, err, c.want)
		}
	}
}

// fixture is a task directory, its fixture and a working directory
// changed from it, for the assertions to judge.
type fixture struct {
	t    *testing.T
	task Task
	in   Input
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	taskDir := filepath.Join(root, "task")
	work := filepath.Join(root, "work")
	writeFiles(t, taskDir, map[string]string{
		"fixture/keep.txt":               "kept\n",
		"fixture/change.txt":             "before\n",
		"fixture/gone.txt":               "removed later\n",
		"testdata/want.txt":              "hello world",
		"testdata/want.json":             `{"a": [1, 2], "b": "c"}`,
		"testdata/overlay/extra_test.go": "package m\n\nimport \"testing\"\n\nfunc TestExtra(t *testing.T) {}\n",
	})
	writeFiles(t, work, map[string]string{
		"keep.txt":      "kept\n",
		"change.txt":    "after\n",
		"hello.txt":     "hello world\n\n",
		"data.json":     `{"b":"c","a":[1,2]}`,
		"bad.json":      "{",
		"src/a.go":      "package a\n\nfunc Fetch() {}\n",
		"src/b.go":      "package a\n",
		"src/deep/c.go": "package deep\n\n// Lookup is here.\n",
		".git/HEAD":     "ref: refs/heads/main\n",
	})
	return &fixture{t: t, task: Task{ID: "coding/x", Dir: taskDir}, in: Input{Workdir: work, Scratch: filepath.Join(root, "scratch")}}
}

// judge evaluates one check.yaml against the fixture.
func (f *fixture) judge(src string) Verdict {
	f.t.Helper()
	in := f.in
	in.Task = f.task
	in.Task.Check = []byte(src)
	if err := os.MkdirAll(in.Scratch, 0o755); err != nil {
		f.t.Fatal(err)
	}
	v, err := Evaluate(f.t.Context(), in)
	if err != nil {
		f.t.Fatalf("%s: %v", src, err)
	}
	return v
}

func TestFileAssertions(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		src  string
		pass bool
		want string
	}{
		{"- file: {path: hello.txt, equals: \"hello world\\n\"}\n", true, ""},
		{"- file: {path: hello.txt, equals: hello}\n", false, `is "hello world", want "hello"`},
		{"- file: {path: missing.txt, contains: [x]}\n", false, "missing.txt does not exist"},
		{"- file: {path: hello.txt, equals_file: testdata/want.txt}\n", true, ""},
		{"- file: {path: keep.txt, equals_file: testdata/want.txt}\n", false, "differs from testdata/want.txt"},
		{"- file: {path: data.json, json_equals_file: testdata/want.json}\n", true, ""},
		{"- file: {path: hello.txt, json_equals_file: testdata/want.json}\n", false, "is not JSON"},
		{"- file: {path: keep.txt, json_equals_file: testdata/want.json}\n", false, "is not JSON"},
		{"- file: {path: change.txt, json_equals_file: testdata/want.txt}\n", false, ""},
		{"- file: {path: hello.txt, contains: [hello, world]}\n", true, ""},
		{"- file: {path: hello.txt, contains: [planet]}\n", false, `does not contain "planet"`},
		{"- file: {path: hello.txt, not_contains: [world]}\n", false, `contains "world"`},
		{"- file: {path: hello.txt, not_contains: [planet], matches: '^hel+o'}\n", true, ""},
		{"- file: {path: hello.txt, matches: '^world'}\n", false, "does not match"},
		{"- file: {path: \"${WORKDIR}/hello.txt\", contains: [hello]}\n", true, ""},
	} {
		if c.src == "- file: {path: change.txt, json_equals_file: testdata/want.txt}\n" {
			// The expected JSON itself is not JSON: the checker cannot run.
			in := f.in
			in.Task = f.task
			in.Task.Check = []byte(c.src)
			if _, err := Evaluate(t.Context(), in); err == nil {
				t.Errorf("an expected file that is not JSON evaluated")
			}
			continue
		}
		v := f.judge(c.src)
		if v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s: verdict %+v, want pass %v and a reason naming %q", c.src, v, c.pass, c.want)
		}
		if !v.Pass && (v.Assertion != 1 || v.Kind != "file") {
			t.Errorf("%s: the verdict names assertion %d %q", c.src, v.Assertion, v.Kind)
		}
	}
}

func TestFileAssertionsThatCannotRun(t *testing.T) {
	f := newFixture(t)
	for _, src := range []string{
		"- file: {path: keep.txt, equals_file: testdata/none.txt}\n",
		"- file: {path: keep.txt, json_equals_file: testdata/none.json}\n",
		"- file: {path: src, contains: [x]}\n",
		"- unchanged: [not-in-the-fixture.txt]\n",
		"- unchanged: [src]\n",
	} {
		in := f.in
		in.Task = f.task
		in.Task.Check = []byte(src)
		if _, err := Evaluate(t.Context(), in); err == nil {
			t.Errorf("%s evaluated", src)
		}
	}
	in := f.in
	in.Task = f.task
	in.Task.Check = []byte("- [")
	if _, err := Evaluate(t.Context(), in); err == nil {
		t.Error("a check.yaml that does not parse evaluated")
	}
}

func TestTreeAssertions(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		src  string
		pass bool
		want string
	}{
		{"- unchanged: [keep.txt]\n", true, ""},
		{"- unchanged: [keep.txt, change.txt]\n", false, "change.txt changed"},
		{"- unchanged: [gone.txt]\n", false, "gone.txt was removed"},
		{"- absent: [gone.txt, nothing/here]\n", true, ""},
		{"- absent: [keep.txt]\n", false, "keep.txt exists"},
		{"- only_files: [keep.txt, change.txt, hello.txt, data.json, bad.json, src/a.go, src/b.go, src/deep/c.go]\n", true, ""},
		{"- only_files: [keep.txt, gone.txt]\n", false, "lacks [gone.txt]"},
		{"- no_match: {glob: '**/*.go', pattern: '\\bLookup\\b'}\n", false, "src/deep/c.go:3 matches"},
		{"- no_match: {glob: 'src/*.go', pattern: '\\bLookup\\b'}\n", true, ""},
		{"- no_match: {glob: '*.txt', pattern: before}\n", true, ""},
	} {
		v := f.judge(c.src)
		if v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s: verdict %+v, want pass %v and a reason naming %q", c.src, v, c.pass, c.want)
		}
	}
	bundled := f.task
	bundled.Bundle = true
	in := f.in
	in.Task = bundled
	in.Task.Check = []byte("- unchanged: [keep.txt]\n")
	if _, err := Evaluate(t.Context(), in); err == nil {
		t.Error("unchanged evaluated against a task that starts from a bundle")
	}
}

func TestGoAssertions(t *testing.T) {
	f := newFixture(t)
	writeFiles(t, f.in.Workdir, map[string]string{"go.mod": "module m\n\ngo 1.24\n", "m.go": "package m\n"})
	for _, c := range []struct {
		src  string
		pass bool
		want string
	}{
		{"- go: {args: [vet, .]}\n", true, ""},
		{"- go: {args: [test, -run, TestExtra, .], overlay: testdata/overlay}\n", true, ""},
		{"- go: {args: [test, -run, TestExtra, .]}\n", true, ""},
		{"- go: {args: [run, ./nothing]}\n", false, "go run ./nothing exited 1"},
	} {
		v := f.judge(c.src)
		if v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s: verdict %+v, want pass %v and a reason naming %q", c.src, v, c.pass, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(f.in.Workdir, "extra_test.go")); err == nil {
		t.Fatal("the overlay was copied into the working directory itself")
	}
	in := f.in
	in.Task = f.task
	in.Task.Check = []byte("- go: {args: [vet], overlay: testdata/none}\n")
	if _, err := Evaluate(t.Context(), in); err == nil {
		t.Error("a missing overlay evaluated")
	}
	in.Scratch = filepath.Join(f.in.Workdir, "keep.txt")
	in.Task.Check = []byte("- go: {args: [vet]}\n")
	if _, err := Evaluate(t.Context(), in); err == nil {
		t.Error("a scratch directory that is a file evaluated")
	}
}

func TestGoOutputIsScrubbedOfTimes(t *testing.T) {
	out := []byte("--- FAIL: TestX (0.01s)\nFAIL\texample.com/m\t1.234s\nok  \texample.com/n\t(cached)\n")
	if got := tail(out, 10); got != "--- FAIL: TestX\nFAIL\texample.com/m\nok  \texample.com/n" {
		t.Fatalf("tail = %q", got)
	}
	if got := tail([]byte("a\nb\nc\n"), 2); got != "b\nc" {
		t.Fatalf("tail = %q", got)
	}
	long := strings.Repeat("x", 300)
	if got := clip(long); len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Fatalf("clip = %q", got)
	}
}

// event builds one event of a thread.
func event(t *testing.T, typ session.Type, thread string, payload any) session.Event {
	t.Helper()
	e, err := session.NewEvent(typ, payload, time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	e.Thread = thread
	return e
}

// call is a tool_use and its result in a thread.
func call(t *testing.T, thread, id, name, input, outcome string) []session.Event {
	t.Helper()
	evs := []session.Event{event(t, session.TypeAgentToolUse, thread, session.AgentToolUse{ToolUseID: id, Name: name, Input: json.RawMessage(input), Verdict: "allow"})}
	if outcome != "" {
		evs = append(evs, event(t, session.TypeToolResult, thread, session.ToolResult{
			ToolUseID: id, Outcome: outcome, Content: []lux.Block{{Type: ir.BlockText, Text: name + " said " + outcome}, {Type: ir.BlockImage}},
		}))
	}
	return evs
}

func TestCalls(t *testing.T) {
	var evs []session.Event
	evs = append(evs, call(t, "", "c1", "read", `{"path":"a.txt"}`, "ok")...)
	evs = append(evs, call(t, "thr_1", "c1", "write", `{"path":"b.txt"}`, "error")...)
	evs = append(evs, call(t, "", "c2", "bash", `{"command":"sleep 100"}`, "")...)
	evs = append(evs, event(t, session.TypeToolResult, "", session.ToolResult{ToolUseID: "c9", Outcome: "ok"}))
	bad := event(t, session.TypeAgentToolUse, "", session.AgentToolUse{ToolUseID: "c3"})
	bad.Payload = json.RawMessage(`{"tool_use_id": 3}`)
	badResult := event(t, session.TypeToolResult, "", session.ToolResult{})
	badResult.Payload = json.RawMessage(`[]`)
	redacted := call(t, "", "c4", "read", `{}`, "ok")[0]
	redacted.Payload = json.RawMessage(`{"tombstone":true}`)
	evs = append(evs, bad, badResult, redacted)
	got := Calls(evs)
	if len(got) != 3 {
		t.Fatalf("calls %+v", got)
	}
	if got[0].Name != "read" || got[0].Outcome != "ok" || got[0].Text != "read said ok" || got[0].Thread != "" {
		t.Fatalf("first %+v", got[0])
	}
	if got[1].Thread != "thr_1" || got[1].Outcome != "error" {
		t.Fatalf("second %+v", got[1])
	}
	if got[2].Outcome != "" {
		t.Fatalf("a call with no result has the outcome %q", got[2].Outcome)
	}
}

func TestCallAssertions(t *testing.T) {
	f := newFixture(t)
	var evs []session.Event
	evs = append(evs, call(t, "", "c1", "read", `{"path":"logs/server.log","offset":10,"limit":1}`, "ok")...)
	evs = append(evs, call(t, "", "c2", "edit", `{"path":"a.go","old_string":"x","new_string":"y","replace_all":false}`, "ok")...)
	evs = append(evs, call(t, "", "c3", "edit", `{"path":"b.go","old_string":"x","new_string":"y","replace_all":true}`, "ok")...)
	evs = append(evs, call(t, "thr_1", "c4", "read", `{"path":"colors.txt"}`, "ok")...)
	evs = append(evs, call(t, "", "c5", "bash", `{"command":"go run ./server","background":true}`, "ok")...)
	evs = append(evs, call(t, "", "c6", "bash", `"not an object"`, "error")...)
	f.in.Events = evs
	for _, c := range []struct {
		src  string
		pass bool
		want string
	}{
		{"- call: {tool: read}\n", true, ""},
		{"- call: {tool: read, min: 3}\n", false, "2 read call(s) match, want at least 3"},
		{"- call: {tool: read, max: 1}\n", false, "want at most 1"},
		{"- call: {tool: read, thread: root, max: 1}\n", true, ""},
		{"- call: {tool: read, thread: sub, input: {path: {matches: 'colors\\.txt$'}}}\n", true, ""},
		{"- call: {tool: read, thread: sub, input: {offset: {present: true}}}\n", false, "0 read"},
		{"- call: {tool: read, input: {offset: {present: true, equals: 10}, limit: {equals: 1}}}\n", true, ""},
		{"- call: {tool: read, input: {offset: {equals: 11}}}\n", false, ""},
		{"- call: {tool: read, input: {offset: {matches: '1'}}}\n", false, ""},
		{"- call: {tool: read, input: {offset: {present: false}}}\n", true, ""},
		{"- call: {tool: read, input: {missing: {contains: x}}}\n", false, ""},
		{"- call: {tool: edit, min: 2, input: {path: {min_length: 4}}}\n", true, ""},
		{"- call: {tool: edit, input: {path: {min_length: 5}}}\n", false, ""},
		{"- call: {tool: edit, max: 1, input: {replace_all: {not_equals: true}}}\n", true, ""},
		{"- call: {tool: edit, outcome: error}\n", false, ""},
		{"- call: {tool: bash, input: {background: {equals: true}, command: {contains: ./server}}}\n", true, ""},
		{"- call: {tool: bash, outcome: error}\n", true, ""},
		{"- call: {tool: bash, outcome: error, input: {command: {present: true}}}\n", false, ""},
		{"- no_call: {tool: write}\n", true, ""},
		{"- no_call: {tool: edit, input: {replace_all: {equals: true}}}\n", false, "1 edit call(s) match, want at most 0"},
		{"- no_call: {tool: bash, outcome: timeout}\n", true, ""},
	} {
		v := f.judge(c.src)
		if v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
			t.Errorf("%s: verdict %+v, want pass %v and a reason naming %q", c.src, v, c.pass, c.want)
		}
	}
}

// todos is a todo call whose items have the statuses given.
func todos(t *testing.T, id, thread string, statuses ...string) []session.Event {
	t.Helper()
	var items []map[string]string
	for i, s := range statuses {
		items = append(items, map[string]string{"id": string(rune('a' + i)), "content": "step", "status": s})
	}
	b, err := json.Marshal(map[string]any{"todos": items})
	if err != nil {
		t.Fatal(err)
	}
	return call(t, thread, id, "todo", string(b), "ok")
}

func TestTodoAssertions(t *testing.T) {
	const check = "- todo: {min_items: 3, max_in_progress: 1, final: completed}\n"
	for _, c := range []struct {
		name string
		evs  [][]session.Event
		pass bool
		want string
	}{
		{"no list", nil, false, "kept no todo list"},
		{"a list in a subagent only", [][]session.Event{todos(t, "t1", "thr_1", "completed", "completed", "completed")}, false, "kept no todo list"},
		{"kept well", [][]session.Event{
			todos(t, "t1", "", "in_progress", "pending", "pending"),
			todos(t, "t2", "", "completed", "in_progress", "pending"),
			todos(t, "t3", "", "completed", "completed", "completed"),
		}, true, ""},
		{"two at once", [][]session.Event{
			todos(t, "t1", "", "in_progress", "in_progress", "pending"),
			todos(t, "t3", "", "completed", "completed", "completed"),
		}, false, "todo call 1 has 2 items in progress"},
		{"too short", [][]session.Event{todos(t, "t1", "", "completed", "completed")}, false, "the longest todo list has 2 items"},
		{"left open", [][]session.Event{todos(t, "t1", "", "completed", "completed", "pending")}, false, `leaves "c" pending`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			for _, e := range c.evs {
				f.in.Events = append(f.in.Events, e...)
			}
			v := f.judge(check)
			if v.Pass != c.pass || !strings.Contains(v.Reason, c.want) {
				t.Fatalf("verdict %+v, want pass %v and a reason naming %q", v, c.pass, c.want)
			}
		})
	}
	f := newFixture(t)
	f.in.Events = todos(t, "t1", "", "in_progress", "in_progress")
	if v := f.judge("- todo: {}\n"); !v.Pass {
		t.Fatalf("a todo assertion with no conditions: %+v", v)
	}
}

func TestTheFirstFailingAssertionIsNamed(t *testing.T) {
	f := newFixture(t)
	v := f.judge("- absent: [nothing]\n- file: {path: hello.txt, contains: [hello]}\n- absent: [keep.txt]\n- absent: [change.txt]\n")
	if v.Pass || v.Assertion != 3 || v.Kind != "absent" {
		t.Fatalf("verdict %+v", v)
	}
	if k := (Assertion{}).Kind(); k != "" {
		t.Fatalf("an empty assertion is a %q", k)
	}
}

func TestGlobRegexp(t *testing.T) {
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/a.go", true},
		{"*.go", "x/a.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/deep/a.go", false},
		{"docs/**", "docs/a/b.md", true},
		{"a?.txt", "ab.txt", true},
		{"a?.txt", "a/.txt", false},
		{"a.txt", "abtxt", false},
	} {
		if got := globRegexp(c.glob).MatchString(c.path); got != c.want {
			t.Errorf("%s against %s: %v", c.glob, c.path, got)
		}
	}
}

func TestCheckScripts(t *testing.T) {
	f := newFixture(t)
	writeFiles(t, f.task.Dir, map[string]string{"check.sh": "test -f \"$1/hello.txt\" || exit 4\ntest \"$2\" = the-log || exit 5\nexit \"${CODE:-0}\"\n"})
	in := f.in
	in.Task, in.Log = f.task, "the-log"
	v, err := Evaluate(t.Context(), in)
	if err != nil || !v.Pass {
		t.Fatalf("verdict %+v, %v", v, err)
	}
	in.Log = "another"
	v, err = Evaluate(t.Context(), in)
	if err != nil || v.Pass || v.Kind != "check.sh" || !strings.Contains(v.Reason, "exited 5") {
		t.Fatalf("verdict %+v, %v", v, err)
	}
	in.Workdir = filepath.Join(t.TempDir(), "gone")
	if _, err := Evaluate(t.Context(), in); err == nil {
		t.Fatal("check.sh ran in a working directory that does not exist")
	}
}

func TestCopyTreeRefusesALink(t *testing.T) {
	src := t.TempDir()
	if err := os.Symlink("/etc/hosts", filepath.Join(src, "hosts")); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(src, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("copyTree: %v", err)
	}
	if err := copyTree(filepath.Join(src, "none"), t.TempDir()); err == nil {
		t.Fatal("copyTree of a missing tree")
	}
}
