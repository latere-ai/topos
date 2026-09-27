// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package prompts

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template/parse"
)

const importPath = "latere.ai/x/topos/prompts"

// constants are the Name constants this package declares, by identifier.
func constants(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range pkgs["prompts"].Files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				if typ, ok := v.Type.(*ast.Ident); !ok || typ.Name != "Name" {
					continue
				}
				for i, id := range v.Names {
					lit, ok := v.Values[i].(*ast.BasicLit)
					if !ok {
						t.Fatalf("%s is not a string literal", id.Name)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[id.Name] = s
				}
			}
		}
	}
	return out
}

// TestEveryCallNamesAText reads every Go file of the module outside this
// package and its tests, and checks each call of Text, Render and Execute:
// the name is a constant of this package whose file exists, a static text
// is asked for with Text, and a Data literal holds exactly the keys the
// template reads. It then checks that every constant is asked for and
// every file is named, included, or superseded by a later version.
func TestEveryCallNamesAText(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller information")
	}
	here := filepath.Dir(file)
	consts := constants(t, here)
	used := map[string]bool{}
	calls := 0
	err := filepath.WalkDir(filepath.Dir(here), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == here || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, src, 0)
		if err != nil {
			return err
		}
		local := ""
		for _, imp := range f.Imports {
			if imp.Path.Value == strconv.Quote(importPath) {
				local = "prompts"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isIdent(fn.X, local) || (fn.Sel.Name != "Text" && fn.Sel.Name != "Render" && fn.Sel.Name != "Execute") {
				return true
			}
			calls++
			at := fset.Position(call.Pos()).String()
			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if !ok || !isIdent(arg.X, local) {
				t.Errorf("%s: %s names its text with something other than a constant of package prompts", at, fn.Sel.Name)
				return true
			}
			name, ok := consts[arg.Sel.Name]
			if !ok {
				t.Errorf("%s: prompts.%s is not a Name constant", at, arg.Sel.Name)
				return true
			}
			used[arg.Sel.Name] = true
			if fn.Sel.Name == "Text" {
				if _, ok := texts.static[name]; !ok {
					t.Errorf("%s: %s is not a static text", at, name)
				}
				return true
			}
			if texts.set.Lookup(name) == nil {
				t.Errorf("%s: no file holds %s", at, name)
				return true
			}
			keys, checked := dataKeys(call.Args[1], local)
			if !checked {
				return true
			}
			if uses := slices.Sorted(maps.Keys(fields(t, texts, name))); !slices.Equal(keys, uses) {
				t.Errorf("%s: %s is rendered with %v and reads %v", at, name, keys, uses)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("no call of the package was found")
	}
	for _, id := range slices.Sorted(maps.Keys(consts)) {
		if !used[id] {
			t.Errorf("prompts.%s (%s) is asked for by no caller", id, consts[id])
		}
	}
	reachable := map[string]bool{}
	var reach func(string)
	reach = func(n string) {
		if reachable[n] {
			return
		}
		reachable[n] = true
		if tmpl := texts.set.Lookup(n); tmpl != nil {
			includes(tmpl.Root, reach)
		}
	}
	for _, n := range consts {
		reach(n)
	}
	for _, n := range names(texts) {
		if strings.HasPrefix(n, "harness/harness-v") {
			reach(n)
		}
	}
	for _, n := range names(texts) {
		if !reachable[n] && !superseded(n) {
			t.Errorf("%s is named by no constant, included by no text, and superseded by no later version", n)
		}
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// dataKeys are the keys of a Data literal, sorted, or none for nil; false
// for data built elsewhere, which the call's own tests render.
func dataKeys(e ast.Expr, local string) ([]string, bool) {
	if isIdent(e, "nil") {
		return []string{}, true
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	if typ, ok := lit.Type.(*ast.SelectorExpr); !ok || !isIdent(typ.X, local) || typ.Sel.Name != "Data" {
		return nil, false
	}
	keys := []string{}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return nil, false
		}
		k, ok := kv.Key.(*ast.BasicLit)
		if !ok || k.Kind != token.STRING {
			return nil, false
		}
		s, err := strconv.Unquote(k.Value)
		if err != nil {
			return nil, false
		}
		keys = append(keys, s)
	}
	slices.Sort(keys)
	return keys, true
}

// includes calls f with the name of every template a tree includes.
func includes(n parse.Node, f func(string)) {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return
		}
		for _, m := range n.Nodes {
			includes(m, f)
		}
	case *parse.IfNode:
		includes(n.List, f)
		includes(n.ElseList, f)
	case *parse.RangeNode:
		includes(n.List, f)
		includes(n.ElseList, f)
	case *parse.WithNode:
		includes(n.List, f)
		includes(n.ElseList, f)
	case *parse.TemplateNode:
		f(n.Name)
	}
}

// superseded reports whether a later version of the text n exists.
func superseded(n string) bool {
	i := strings.LastIndex(n, "-v")
	cur, err := strconv.Atoi(n[i+2:])
	if i < 0 || err != nil {
		return false
	}
	for _, m := range names(texts) {
		v, ok := strings.CutPrefix(m, n[:i+2])
		if later, err := strconv.Atoi(v); ok && err == nil && later > cur {
			return true
		}
	}
	return false
}
