// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/host"
	"latere.ai/x/topos/test/stubs/cellastub"
)

// tree writes the same files under root: a .gitignore, an ignored
// directory, a credential file, and files of distinct ages, main.go the
// oldest.
func tree(t *testing.T, root string) {
	t.Helper()
	files := []struct{ name, body string }{
		{"main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"},
		{"lib/lib.go", "package lib\n\n// Hello says hello.\nfunc Hello() string { return \"hello\" }\n"},
		{"lib/lib_test.go", "package lib\n"},
		{"build/out.go", "package build // hello\n"},
		{".gitignore", "build/\n"},
		{".env", "HELLO=secret\n"},
		{"notes.txt", "Hello, notes\n"},
	}
	base := time.Now().Add(-time.Hour)
	for i, file := range files {
		p := filepath.Join(root, filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(file.body), 0o644); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

var searches = map[string]machine.SearchRequest{
	"files":            {Kind: machine.SearchGrep, Pattern: "hello"},
	"content":          {Kind: machine.SearchGrep, Pattern: "hello", OutputMode: machine.ModeContent, Context: 1},
	"count":            {Kind: machine.SearchGrep, Pattern: "(?i)hello", OutputMode: machine.ModeCount},
	"case insensitive": {Kind: machine.SearchGrep, Pattern: "HELLO", CaseInsensitive: true},
	"a glob filter":    {Kind: machine.SearchGrep, Pattern: "package", Glob: "*_test.go"},
	"glob":             {Kind: machine.SearchGlob, Pattern: "**/*.go"},
	"below the root":   {Kind: machine.SearchGlob, Pattern: "*", Path: "lib"},
	"a head limit":     {Kind: machine.SearchGrep, Pattern: "package", HeadLimit: 1},
}

func TestSearch(t *testing.T) {
	f := open(t)
	tree(t, f.ws())
	res, err := f.m.Search(t.Context(), searches["files"])
	want := f.ws() + "/lib/lib.go\n" + f.ws() + "/main.go"
	if err != nil || strings.Join(res.Lines, "\n") != want {
		t.Errorf("grep = %q %v, want %q: the ignored directory and .env are skipped", res.Lines, err, want)
	}
	res, err = f.m.Search(t.Context(), searches["glob"])
	if err != nil || len(res.Lines) != 3 || res.Lines[2] != f.ws()+"/main.go" {
		t.Errorf("glob = %q %v: newest first", res.Lines, err)
	}
	ctx := t.Context()
	for name, c := range map[string]struct {
		q    machine.SearchRequest
		want error
	}{
		"missing": {machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "gone"}, fs.ErrNotExist},
		"outside": {machine.SearchRequest{Kind: machine.SearchGlob, Pattern: "*", Path: "/"}, machine.ErrOutside},
		"denied":  {machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "x", Path: ".env"}, machine.ErrDenied},
	} {
		if _, err := f.m.Search(ctx, c.q); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	if _, err := f.m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "("}); err == nil || !strings.HasPrefix(err.Error(), "machine: the pattern is not a valid RE2 expression") {
		t.Errorf("a bad pattern: %v", err)
	}
	if err := f.m.WriteFile(ctx, f.m.SpillDir()+"/tool-1.txt", strings.NewReader("spilled hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = f.m.Search(ctx, machine.SearchRequest{Kind: machine.SearchGrep, Pattern: "hello", Path: f.m.SpillDir()})
	if err != nil || len(res.Lines) != 1 || res.Lines[0] != f.m.SpillDir()+"/tool-1.txt" {
		t.Errorf("a search of the spill directory = %q %v", res.Lines, err)
	}
	f.stub.Fail(cellastub.OpSession, cellastub.Failure{Status: 503, Code: "driver_unavailable"})
	if _, err := f.m.Search(ctx, searches["files"]); err == nil {
		t.Error("a search Cella could not start was answered")
	}
}

// TestParityWithTheHost runs the same searches, commands and file
// operations on the host machine and on a Cella machine over the same
// files, and compares the answers with each machine's root taken out.
func TestParityWithTheHost(t *testing.T) {
	f := open(t)
	tree(t, f.ws())
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(base, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	tree(t, work)
	h, err := host.Open(host.Options{Workdir: work, SpillDir: filepath.Join(base, "spill"), Environ: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Release(context.Background(), true); err != nil {
			t.Error(err)
		}
	})
	machines := map[string]machine.Machine{"host": h, "cella": f.m}
	roots := map[string]string{"host": work, "cella": f.ws()}
	ctx := t.Context()
	answer := func(name string, run func(machine.Machine) string) {
		t.Helper()
		got := map[string]string{}
		for kind, m := range machines {
			got[kind] = strings.ReplaceAll(run(m), roots[kind], "<root>")
		}
		if got["host"] != got["cella"] {
			t.Errorf("%s:\nhost  %q\ncella %q", name, got["host"], got["cella"])
		}
	}
	for name, q := range searches {
		answer("search "+name, func(m machine.Machine) string {
			res, err := m.Search(ctx, q)
			if err != nil {
				return "error " + err.Error()
			}
			return strings.Join(res.Lines, "\n") + "|" + map[bool]string{true: "truncated"}[res.Truncated]
		})
	}
	for _, r := range []machine.ExecRequest{
		{Command: "ls; echo err >&2; exit 3"},
		{Command: "cd lib && pwd", ReportDir: true},
		{Command: "wc -c", Stdin: strings.NewReader("12345")},
		{Command: "sleep 30", Timeout: 200 * time.Millisecond},
	} {
		answer("exec "+r.Command, func(m machine.Machine) string {
			if r.Stdin != nil {
				r.Stdin = strings.NewReader("12345")
			}
			res, err := m.Exec(ctx, r)
			if err != nil {
				return "error " + err.Error()
			}
			return strings.Join([]string{strings.TrimSpace(string(res.Output)), res.Dir,
				map[bool]string{true: "timed out"}[res.TimedOut], strings.Repeat("!", res.ExitCode+1)}, "|")
		})
	}
	answer("missing dir", func(m machine.Machine) string {
		_, err := m.Exec(ctx, machine.ExecRequest{Command: "true", Dir: "gone"})
		return map[bool]string{true: "not exist"}[errors.Is(err, fs.ErrNotExist)]
	})
	for name, p := range map[string]string{"a file": "main.go", "a directory": "lib", "missing": "gone", "denied": ".env", "outside": "/elsewhere"} {
		answer("stat "+name, func(m machine.Machine) string {
			fi, err := m.Stat(ctx, p)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return "not exist"
			case errors.Is(err, machine.ErrDenied):
				return "denied"
			case errors.Is(err, machine.ErrOutside):
				return "outside"
			case err != nil:
				return err.Error()
			}
			return strings.Join([]string{fi.Path, fi.Mode.Perm().String(), map[bool]string{true: "dir"}[fi.IsDir]}, "|")
		})
	}
	answer("list", func(m machine.Machine) string {
		infos, err := m.List(ctx, "lib")
		if err != nil {
			return err.Error()
		}
		// The host lists in directory order.
		slices.SortFunc(infos, func(a, b machine.FileInfo) int { return strings.Compare(a.Path, b.Path) })
		return names(infos)
	})
}
