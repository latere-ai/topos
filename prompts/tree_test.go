// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"maps"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"text/template/parse"
)

// names are the names of every embedded text, sorted.
func names(c catalog) []string {
	var out []string
	for _, t := range c.set.Templates() {
		if t.Name() != "" {
			out = append(out, t.Name())
		}
	}
	slices.Sort(out)
	return out
}

// raws are the files of fsys by name, as they are on disk.
func raws(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fields are the keys text n reads from its data: the fields named with
// the data as dot, through the templates it includes with dot, and every
// $.Key. A range or with body reads its element, not the data.
func fields(t *testing.T, c catalog, n string) map[string]bool {
	t.Helper()
	tmpl := c.set.Lookup(n)
	if tmpl == nil {
		t.Fatalf("no text %s", n)
	}
	out := map[string]bool{}
	var node func(parse.Node, bool)
	var pipe func(*parse.PipeNode, bool)
	arg := func(a parse.Node, root bool) {
		switch a := a.(type) {
		case *parse.FieldNode:
			if root {
				out[a.Ident[0]] = true
			}
		case *parse.VariableNode:
			if a.Ident[0] == "$" && len(a.Ident) > 1 {
				out[a.Ident[1]] = true
			}
		case *parse.PipeNode:
			pipe(a, root)
		}
	}
	pipe = func(p *parse.PipeNode, root bool) {
		if p == nil {
			return
		}
		for _, cmd := range p.Cmds {
			for _, a := range cmd.Args {
				arg(a, root)
			}
		}
	}
	node = func(x parse.Node, root bool) {
		switch x := x.(type) {
		case *parse.ListNode:
			if x == nil {
				return
			}
			for _, y := range x.Nodes {
				node(y, root)
			}
		case *parse.ActionNode:
			pipe(x.Pipe, root)
		case *parse.IfNode:
			pipe(x.Pipe, root)
			node(x.List, root)
			node(x.ElseList, root)
		case *parse.RangeNode:
			pipe(x.Pipe, root)
			node(x.List, false)
			node(x.ElseList, root)
		case *parse.WithNode:
			pipe(x.Pipe, root)
			node(x.List, false)
			node(x.ElseList, root)
		case *parse.TemplateNode:
			pipe(x.Pipe, root)
			if inc := c.set.Lookup(x.Name); inc == nil {
				t.Errorf("%s includes %s, which no file holds", n, x.Name)
			} else if x.Pipe != nil && len(x.Pipe.Cmds) == 1 && x.Pipe.Cmds[0].Args[0].Type() == parse.NodeDot {
				node(inc.Root, root)
			}
		}
	}
	node(tmpl.Root, true)
	return out
}

// TestReleasedPromptsAreImmutable pins every released file by its
// SHA-256. A change of wording is a new file with the next version and a
// new line here; a line that changes means a released version was edited,
// and a replay of a request that recorded it would rebuild different
// bytes.
func TestReleasedPromptsAreImmutable(t *testing.T) {
	got := map[string]string{}
	for p, raw := range raws(t, files) {
		sum := sha256.Sum256([]byte(raw))
		got[p] = hex.EncodeToString(sum[:])
	}
	for _, p := range slices.Sorted(maps.Keys(released)) {
		switch sum, ok := got[p]; {
		case !ok:
			t.Errorf("%s was released and is gone", p)
		case sum != released[p]:
			t.Errorf("%s was released and changed: sha256 %s", p, sum)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(got)) {
		if _, ok := released[p]; !ok {
			t.Errorf("%s is not pinned: add %q: %q", p, p, got[p])
		}
	}
}

// TestEveryFileIsAVersionedText holds the tree to its naming rule: each
// file is <dir>/<name>-v<N>.md, and a file that holds no template action
// is a static text.
func TestEveryFileIsAVersionedText(t *testing.T) {
	for _, n := range names(texts) {
		_, v, ok := strings.Cut(n[strings.LastIndex(n, "/")+1:], "-v")
		if !strings.Contains(n, "/") || !ok || v == "" || strings.Trim(v, "0123456789") != "" || v[0] == '0' {
			t.Errorf("%s is not named <dir>/<name>-v<N>", n)
		}
	}
	for n, body := range texts.static {
		if strings.Contains(body, "{{") {
			t.Errorf("static text %s holds a template action", n)
		}
	}
}

// TestLoadRefusesABrokenTree loads trees that must fail: a file that is
// not Markdown, a template that does not parse, a tree that cannot be
// listed, and a file that cannot be read.
func TestLoadRefusesABrokenTree(t *testing.T) {
	for name, fsys := range map[string]fs.FS{
		"not markdown":   fstest.MapFS{"results/a-v1.txt": {Data: []byte("x")}},
		"bad template":   fstest.MapFS{"results/a-v1.md": {Data: []byte("{{.Open")}},
		"unlistable":     failing{fstest.MapFS{"results/a-v1.md": {Data: []byte("x")}}, "."},
		"unreadable":     failing{fstest.MapFS{"results/a-v1.md": {Data: []byte("x")}}, "results/a-v1.md"},
		"a stray action": fstest.MapFS{"results/a-v1.md": {Data: []byte("{{end}}")}},
	} {
		if _, err := load(fsys); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if !panics(func() { mustLoad(fstest.MapFS{"a.txt": {Data: []byte("x")}}) }) {
		t.Fatal("mustLoad accepted a broken tree")
	}
	c, err := load(fstest.MapFS{"results/a-v1.md": {Data: []byte("  plain text\n\n")}, "results/b-v1.md": {Data: []byte("{{.X}}\n")}})
	if err != nil || c.static["results/a-v1"] != "plain text" || len(c.static) != 1 {
		t.Fatalf("a static text and a template: %+v, %v", c.static, err)
	}
}

// failing is a tree whose file at path fails to open. It offers only
// Open, so listing and reading go through it.
type failing struct {
	tree fstest.MapFS
	path string
}

func (f failing) Open(name string) (fs.File, error) {
	if name == f.path {
		return nil, errors.New("the disk went away")
	}
	return f.tree.Open(name)
}
