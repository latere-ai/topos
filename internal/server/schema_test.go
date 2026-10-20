// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/session"
)

// TestEveryBodyHasAType: a route that reads a JSON body names its type
// in shapes, and so does a route that answers JSON, so no body is
// documented by its example alone.
func TestEveryBodyHasAType(t *testing.T) {
	for _, rt := range table() {
		sh := shapes[rt.op]
		if rt.body > 0 && !emptyBodies[rt.op] && sh.request == nil {
			t.Errorf("%s reads a body and names no request type in shapes", rt.op)
		}
		if rt.status != http.StatusNoContent && answerMedia[rt.op] == "" && sh.response == nil {
			t.Errorf("%s answers JSON and names no response type in shapes", rt.op)
		}
	}
	for op := range shapes {
		if !slices.ContainsFunc(table(), func(rt route) bool { return rt.op == op }) {
			t.Errorf("shapes names %s, which no route is", op)
		}
	}
}

// TestExamplesFitTheirTypes decodes every example strictly into its
// route's type: a member the type does not have fails, so the examples
// and the schemas derived from the types say the same.
func TestExamplesFitTheirTypes(t *testing.T) {
	shown, err := examples()
	if err != nil {
		t.Fatal(err)
	}
	for op, sh := range shapes {
		ex := shown[op]
		if sh.request != nil {
			fitsType(t, op+" request", ex.request, sh.request)
		}
		if sh.response == nil {
			continue
		}
		answer := sh.response
		if sh.list {
			answer = reflect.StructOf([]reflect.StructField{
				{Name: "Items", Type: reflect.SliceOf(sh.response), Tag: `json:"items"`},
				{Name: "NextCursor", Type: reflect.TypeFor[string](), Tag: `json:"next_cursor"`},
			})
		}
		fitsType(t, op+" response", ex.response, answer)
		if ex.created != "" {
			fitsType(t, op+" created", ex.created, answer)
		}
	}
}

func fitsType(t *testing.T, what, body string, typ reflect.Type) {
	t.Helper()
	if body == "" {
		t.Errorf("%s: no example", what)
		return
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(reflect.New(typ).Interface()); err != nil {
		t.Errorf("%s does not fit %s: %v", what, typ, err)
	}
}

// TestEveryEventTypeHasAPayload: each type schema v1 defines has its
// payload's type, so every variant of Event is described.
func TestEveryEventTypeHasAPayload(t *testing.T) {
	for typ := range session.Known {
		if payloadTypes[typ] == nil {
			t.Errorf("%s has no payload type in payloadTypes", typ)
		}
	}
	for typ := range payloadTypes {
		if !session.Known[typ] {
			t.Errorf("payloadTypes names %s, which schema v1 does not define", typ)
		}
	}
}

// TestSentTypesAreTheHandlers: SendEvent's variants are the types
// sendEvent takes.
func TestSentTypesAreTheHandlers(t *testing.T) {
	var got []string
	for typ := range sentPayloads {
		got = append(got, string(typ))
	}
	slices.Sort(got)
	want := slices.Clone(sentTypes)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("SendEvent's variants are %v, and sendEvent takes %v", got, want)
	}
}

// TestEnumsAreTheConstants holds each enum the document lists to the
// constants of its type the session package declares.
func TestEnumsAreTheConstants(t *testing.T) {
	declared := constantsOf(t, filepath.Join("..", "..", "session"))
	for _, typ := range []reflect.Type{typeOf[session.Status](), typeOf[session.StopReason]()} {
		want := declared[typ.Name()]
		got := enums[typ]
		if !slices.Equal(got, want) {
			t.Errorf("the document lists %s as %v, and the package declares %v", typ.Name(), got, want)
		}
	}
}

// constantsOf is the string constants of each named type the package in
// dir declares, in order.
func constantsOf(t *testing.T, dir string) map[string][]string {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				ident, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						out[ident.Name] = append(out[ident.Name], strings.Trim(lit.Value, `"`))
					}
				}
			}
		}
	}
	return out
}

// TestPagedRoutesAreTheHandlersThatPage: a route answers a page in the
// document exactly when its handler reads limit and cursor, so a list
// that starts paging documents its parameters.
func TestPagedRoutesAreTheHandlersThatPage(t *testing.T) {
	pages := handlersCalling(t, "pageParams")
	for _, rt := range table() {
		name := handlerName(rt.handle)
		if ownPaging(rt.op) {
			continue
		}
		if pages[name] != shapes[rt.op].list {
			t.Errorf("%s: its handler %s reads limit and cursor: %v; the document answers it as a page: %v", rt.op, name, pages[name], shapes[rt.op].list)
		}
	}
}

// handlersCalling is the methods of call in this package whose bodies
// call method.
func handlersCalling(t *testing.T, method string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
						out[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	return out
}

// handlerName is the method name of a route's handler.
func handlerName(h func(c *call) error) string {
	full := runtime.FuncForPC(reflect.ValueOf(h).Pointer()).Name()
	full = strings.TrimSuffix(full, "-fm")
	return full[strings.LastIndex(full, ".")+1:]
}

// TestNoParameterTwice: no operation lists a parameter twice, as a list
// that pages with bounds of its own would if it also took the shared
// paging parameters.
func TestNoParameterTwice(t *testing.T) {
	raw, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `yaml:"name"`
				In   string `yaml:"in"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for path, ops := range doc.Paths {
		for method, op := range ops {
			seen := map[string]bool{}
			for _, p := range op.Parameters {
				key := p.In + " " + p.Name
				if seen[key] {
					t.Errorf("%s %s lists the %s parameter %s twice", method, path, p.In, p.Name)
				}
				seen[key] = true
			}
		}
	}
}
