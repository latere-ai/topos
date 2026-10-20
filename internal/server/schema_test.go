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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"

	"latere.ai/x/topos/manifest/trigger"
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
		return
	}
	// A member held as json.RawMessage fits anything, so it is decoded
	// again into the type the document describes it by.
	if len(rawFields[typ]) == 0 {
		return
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &members); err != nil {
		t.Errorf("%s: %v", what, err)
		return
	}
	for name, stand := range rawFields[typ] {
		raw, ok := members[name]
		if !ok {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(reflect.New(stand).Interface()); err != nil {
			t.Errorf("%s: %s does not fit %s: %v", what, name, stand, err)
		}
	}
}

// TestRawFieldsAreWhatTheHandlersRead: each member described by a type
// in rawFields is a json.RawMessage of its type, and a full value of the
// describing type is one its handler takes, so the document describes
// what the server reads.
func TestRawFieldsAreWhatTheHandlersRead(t *testing.T) {
	for typ, members := range rawFields {
		for name := range members {
			f, ok := memberOf(typ, name)
			if !ok {
				t.Errorf("rawFields names %s.%s, which %s does not have", typ, name, typ)
			} else if f.Type != reflect.TypeFor[json.RawMessage]() {
				t.Errorf("%s.%s is %s, not json.RawMessage; its own type describes it", typ, name, f.Type)
			}
		}
	}
	marshal := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	machine := createMachine{Kind: session.MachineDevice, Environment: "laptop", Workdir: "/home/ann/widgets"}
	if _, err := requestedMachine(marshal(machine)); err != nil {
		t.Errorf("a create's machine as the document describes it is refused: %v", err)
	}
	repos := []repositoryRequest{{Type: session.ResourceRepository, URL: "https://git.example.com/example/widgets.git", Ref: "main"}}
	if _, err := repositories(marshal(repos)); err != nil {
		t.Errorf("a create's resources as the document describes them are refused: %v", err)
	}
	delivery := trigger.Envelope{ID: "d1", Product: "github", Verb: "push", Resource: "example/widgets", Time: time.Unix(0, 0),
		Payload: marshal(map[string]any{"ref": "refs/heads/main"})}
	if err := delivery.Check(); err != nil {
		t.Errorf("an envelope's payload as the document describes it is refused: %v", err)
	}
	for typ, members := range rawFields {
		for name, stand := range members {
			if typ == reflect.TypeFor[createBody]() && (name == "machine" || name == "resources") {
				continue
			}
			if typ == reflect.TypeFor[trigger.Envelope]() && name == "payload" {
				continue
			}
			t.Errorf("%s.%s is described by %s, which no case here holds to its handler", typ, name, stand)
		}
	}
}

// memberOf is the field of struct typ that encoding/json reads as name.
func memberOf(typ reflect.Type, name string) (reflect.StructField, bool) {
	for f := range typ.Fields() {
		if tag, _, _ := strings.Cut(f.Tag.Get("json"), ","); tag == name {
			return f, true
		}
	}
	return reflect.StructField{}, false
}

// TestDescriptionsSpeakDurations: no description of the document writes
// a duration as Go writes it, 24h0m0s, where a reader says 24 hours.
func TestDescriptionsSpeakDurations(t *testing.T) {
	doc, err := OpenAPI(DefaultBasePath)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := yaml.Unmarshal(doc, &v); err != nil {
		t.Fatal(err)
	}
	goDuration := regexp.MustCompile(`\b\d+(\.\d+)?(h|m|s|ms)(\d+(\.\d+)?(m|s))*\b`)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if s, ok := e.(string); ok && k == "description" {
					if m := goDuration.FindString(s); m != "" {
						t.Errorf("%s/description writes %s; write it with spoken", path, m)
					}
					continue
				}
				walk(path+"/"+k, e)
			}
		case []any:
			for i, e := range v {
				walk(path+"/"+strconv.Itoa(i), e)
			}
		}
	}
	walk("", v)
}

func TestSpoken(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:          "24 hours",
		time.Hour:               "1 hour",
		2 * time.Minute:         "2 minutes",
		90 * time.Minute:        "90 minutes",
		17 * time.Second:        "17 seconds",
		1500 * time.Millisecond: "1.5s",
		0:                       "0s",
	} {
		if got := spoken(d); got != want {
			t.Errorf("spoken(%s) = %q, want %q", d, got, want)
		}
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

// TestRequestTypesStateTheirRequired: every request type of this package
// a body reaches states its required members in requestRequired, and
// every payload a person sends in sentRequired or requestRequired, so no
// request's required members are read from its tags, which say what the
// server writes, not what it requires.
func TestRequestTypesStateTheirRequired(t *testing.T) {
	server := reflect.TypeFor[route]().PkgPath()
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		if typ == nil || seen[typ] {
			return
		}
		seen[typ] = true
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem())
		case reflect.Struct:
			if typ.PkgPath() == server {
				if _, ok := requestRequired[typ]; !ok {
					t.Errorf("%s is a request type and states no required members in requestRequired", typ)
				}
			}
			for f := range typ.Fields() {
				walk(f.Type)
			}
			for _, stand := range rawFields[typ] {
				walk(stand)
			}
		}
	}
	for _, sh := range shapes {
		walk(sh.request)
	}
	for typ, payload := range sentPayloads {
		_, sent := sentRequired[payload]
		_, req := requestRequired[payload]
		_, byHand := written[payload]
		if !sent && !req && !byHand {
			t.Errorf("the payload of %s states no required members", typ)
		}
		walk(payload)
	}
}
