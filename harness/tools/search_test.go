// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree lays out a working directory for the search tools.
func tree(t *testing.T, f fixture) {
	t.Helper()
	files := map[string]string{
		".gitignore":   "ignored/\n*.log\n",
		"a.go":         "package a\n\nfunc Main() {}\n",
		"b.go":         "package b\n// main here\nfunc main() {}\n",
		"c.txt":        "nothing\n",
		"ignored/x.go": "func main() {}\n",
		"debug.log":    "func main\n",
		".git/config":  "func main\n",
		"bin.dat":      "func main\x00\n",
		"sub/d.go":     "func main() {\n\treturn\n}\n",
		".env":         "func main\n",
	}
	for name, body := range files {
		put(t, filepath.Join(f.work, name), body)
	}
	base := time.Now().Add(-time.Hour)
	for i, name := range []string{"a.go", "b.go", "sub/d.go"} {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(f.work, name), at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGrepNeedsNoBinary(t *testing.T) {
	f := open(t)
	tree(t, f)
	ctx := t.Context()
	grep := builtinTool(t, NameGrep)
	w := func(name string) string { return filepath.Join(f.work, name) }
	for _, c := range []struct{ input, want string }{
		{`{"pattern":"func main"}`, w("b.go") + "\n" + w("sub/d.go") + "\n"},
		{`{"pattern":"func main","case_insensitive":true}`, w("a.go") + "\n" + w("b.go") + "\n" + w("sub/d.go") + "\n"},
		{`{"pattern":"func main","output_mode":"content"}`, w("b.go") + ":3:func main() {}\n" + w("sub/d.go") + ":1:func main() {\n"},
		{`{"pattern":"func main","output_mode":"content","context":1,"glob":"b.go"}`, w("b.go") + "-2-// main here\n" + w("b.go") + ":3:func main() {}\n"},
		{`{"pattern":"main","output_mode":"count"}`, w("b.go") + ":2\n" + w("sub/d.go") + ":1\n"},
		{`{"pattern":"main\\(\\) \\{\\n\\treturn","multiline":true}`, w("sub/d.go") + "\n"},
		{`{"pattern":".","glob":"*.go"}`, w("a.go") + "\n" + w("b.go") + "\n" + w("sub/d.go") + "\n"},
		{`{"pattern":"main","path":"sub"}`, w("sub/d.go") + "\n"},
		{`{"pattern":"absent text"}`, "No matches."},
		{`{"pattern":"main","head_limit":1}`, w("b.go") + "\n[... more results past head_limit 1; narrow the pattern, the path or the glob, or raise head_limit]\n"},
	} {
		res := run(ctx, t, grep, f.h, State{}, c.input)
		if res.Outcome != OutcomeOK || text(res) != c.want {
			t.Fatalf("%s:\n%q\nwant\n%q", c.input, text(res), c.want)
		}
	}
	for _, c := range []struct{ input, want string }{
		{`{"pattern":"("}`, "The search failed: the pattern is not a valid RE2 expression: error parsing regexp: missing closing ): `(`."},
		{`{"pattern":"a","output_mode":"lines"}`, `The search failed: output mode "lines" is not files_with_matches, content or count.`},
		{mustInput(t, map[string]string{"pattern": "a", "path": f.outside}), f.outside + " is outside the working directory."},
		{`{"pattern":"a","path":"absent"}`, w("absent") + " does not exist."},
		{`{"pattern":"a","path":".env"}`, w(".env") + " is on the credential deny-list; the tools do not open it."},
	} {
		res := run(ctx, t, grep, f.h, State{}, c.input)
		if res.Outcome != OutcomeError || text(res) != c.want {
			t.Fatalf("%s:\n%q\nwant\n%q", c.input, text(res), c.want)
		}
	}
}

func TestGlobNeedsNoBinary(t *testing.T) {
	f := open(t)
	tree(t, f)
	ctx := t.Context()
	glob := builtinTool(t, NameGlob)
	w := func(name string) string { return filepath.Join(f.work, name) }
	for _, c := range []struct{ input, want string }{
		{`{"pattern":"**/*.go"}`, w("sub/d.go") + "\n" + w("b.go") + "\n" + w("a.go") + "\n"},
		{`{"pattern":"*.go"}`, w("b.go") + "\n" + w("a.go") + "\n"},
		{`{"pattern":"*.go","path":"sub"}`, w("sub/d.go") + "\n"},
		{`{"pattern":"*.rs"}`, "No files match."},
	} {
		res := run(ctx, t, glob, f.h, State{}, c.input)
		if res.Outcome != OutcomeOK || text(res) != c.want {
			t.Fatalf("%s:\n%q\nwant\n%q", c.input, text(res), c.want)
		}
	}
	res := run(ctx, t, glob, f.h, State{}, `{"pattern":"["}`)
	if res.Outcome != OutcomeError || text(res) != "The search failed: the glob pattern: syntax error in pattern." {
		t.Fatalf("a bad pattern %q", text(res))
	}
	res = run(ctx, t, glob, f.h, State{}, mustInput(t, map[string]string{"pattern": "*", "path": f.outside}))
	if res.Outcome != OutcomeError || text(res) != f.outside+" is outside the working directory." {
		t.Fatalf("outside %q", text(res))
	}
	for i := range 1001 {
		put(t, filepath.Join(f.work, "many", fmt.Sprintf("f%04d.txt", i)), "")
	}
	res = run(ctx, t, glob, f.h, State{}, `{"pattern":"*.txt","path":"many"}`)
	if res.Spill == nil {
		t.Fatalf("1000 paths fit in %d bytes", len(text(res)))
	}
	lines := strings.Split(strings.TrimSuffix(get(t, res.Spill.Path), "\n"), "\n")
	if len(lines) != 1001 || lines[1000] != "[... more than 1000 paths match; narrow the pattern or the path]" {
		t.Fatalf("the limit: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
	res = run(ctx, t, glob, faulty{Machine: f.h, searchErr: fmt.Errorf("walk: %w", os.ErrPermission)}, State{}, `{"pattern":"*"}`)
	if res.Outcome != OutcomeError || text(res) != f.work+" is not accessible: permission denied." {
		t.Fatalf("a refused walk %q", text(res))
	}
	res = run(ctx, t, glob, faulty{Machine: f.h, searchErr: errors.New("machine: the helper crashed")}, State{}, `{"pattern":"*"}`)
	if res.Outcome != OutcomeError || text(res) != "The search failed: the helper crashed." {
		t.Fatalf("a failed search %q", text(res))
	}
}
