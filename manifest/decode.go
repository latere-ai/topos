// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-yaml"

	v1 "latere.ai/x/topos/manifest/v1"
)

// parse splits a file into the generic trees of its documents: a stream
// of JSON values when the file starts with {, YAML documents separated
// by --- otherwise. A tree holds map[string]any, []any, string, bool,
// nil and the number types of the two decoders. An empty document is
// skipped.
func parse(body []byte) ([]any, error) {
	if trimmed := bytes.TrimLeft(body, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		return parseJSON(trimmed)
	}
	var docs []any
	dec := yaml.NewDecoder(bytes.NewReader(body))
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, yamlProblem(err)
		}
		if v != nil {
			docs = append(docs, v)
		}
	}
}

func parseJSON(body []byte) ([]any, error) {
	var docs []any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	for {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, newError(CodeInvalidManifest, 1, []Problem{{Detail: fmt.Sprintf("json: not valid JSON at byte %d", dec.InputOffset())}})
		}
		docs = append(docs, v)
	}
}

// yamlProblem renders a parser error by its message and position. The
// parser's own rendering quotes the source around the error, which may
// hold a secret, so it is not used.
func yamlProblem(err error) *Error {
	detail := "yaml: the file does not parse"
	if ye, ok := errors.AsType[yaml.Error](err); ok {
		detail = "yaml: " + ye.GetMessage()
		if tk := ye.GetToken(); tk != nil && tk.Position != nil {
			detail += fmt.Sprintf(" at line %d, column %d", tk.Position.Line, tk.Position.Column)
		}
	}
	return newError(CodeInvalidManifest, 1, []Problem{{Detail: detail}})
}

// object is one decoded document: the typed object of its kind and the
// problems its decoding found.
type object struct {
	doc   int
	kind  string
	name  string
	agent *v1.Agent
	trig  *v1.Trigger
	store *v1.MemoryStore
	conn  *v1.Connection
}

// envelope reads a document's apiVersion and kind. It returns the
// version problem apart, since unsupported_version is decided before
// any other stage.
func envelope(doc int, tree any) (kind string, version, invalid []Problem) {
	m, ok := tree.(map[string]any)
	if !ok {
		return "", nil, []Problem{{Doc: doc, Detail: "a document is a mapping, got " + kindOf(tree)}}
	}
	switch av, ok := m["apiVersion"].(string); {
	case m["apiVersion"] == nil:
		invalid = append(invalid, Problem{Doc: doc, Path: "apiVersion", Detail: "required"})
	case !ok:
		invalid = append(invalid, Problem{Doc: doc, Path: "apiVersion", Detail: "want a string, got " + kindOf(m["apiVersion"])})
	case av != v1.APIVersion:
		version = append(version, Problem{Doc: doc, Path: "apiVersion", Detail: "not " + v1.APIVersion})
	}
	switch k, ok := m["kind"].(string); {
	case m["kind"] == nil:
		invalid = append(invalid, Problem{Doc: doc, Path: "kind", Detail: "required"})
	case !ok:
		invalid = append(invalid, Problem{Doc: doc, Path: "kind", Detail: "want a string, got " + kindOf(m["kind"])})
	case k == v1.KindAgent || k == v1.KindTrigger || k == v1.KindMemoryStore || k == v1.KindConnection:
		kind = k
	default:
		invalid = append(invalid, Problem{Doc: doc, Path: "kind", Detail: "not Agent, Trigger, MemoryStore or Connection"})
	}
	return kind, version, invalid
}

// decodeObject walks a document's tree into its kind's type. keepStatus
// reads status, which Resolve ignores on input and a bundle carries.
func decodeObject(doc int, kind string, tree map[string]any, keepStatus bool) (*object, []Problem) {
	if !keepStatus {
		tree = maps.Clone(tree)
		delete(tree, "status")
	}
	o := &object{doc: doc, kind: kind}
	var target any
	switch kind {
	case v1.KindAgent:
		o.agent = &v1.Agent{}
		target = o.agent
	case v1.KindTrigger:
		o.trig = &v1.Trigger{}
		target = o.trig
	case v1.KindMemoryStore:
		o.store = &v1.MemoryStore{}
		target = o.store
	default:
		o.conn = &v1.Connection{}
		target = o.conn
	}
	w := &walker{doc: doc}
	w.value("", tree, reflect.ValueOf(target).Elem())
	o.name = o.meta().Name
	return o, w.problems
}

func (o *object) meta() *v1.ObjectMeta {
	switch {
	case o.agent != nil:
		return &o.agent.Metadata
	case o.trig != nil:
		return &o.trig.Metadata
	case o.store != nil:
		return &o.store.Metadata
	}
	return &o.conn.Metadata
}

func (o *object) status() *v1.Status {
	switch {
	case o.agent != nil:
		return &o.agent.Status
	case o.trig != nil:
		return &o.trig.Status
	case o.store != nil:
		return &o.store.Status
	}
	return &o.conn.Status
}

// spec is the object's spec, the value its digest covers.
func (o *object) spec() any {
	switch {
	case o.agent != nil:
		return o.agent.Spec
	case o.trig != nil:
		return o.trig.Spec
	case o.store != nil:
		return o.store.Spec
	}
	return o.conn.Spec
}

// walker decodes a generic tree into a typed value by its json tags,
// collecting every unknown field and wrong type with its path instead of
// stopping at the first, so one error lists them all.
type walker struct {
	doc      int
	problems []Problem
}

var (
	typeTool = reflect.TypeFor[v1.Tool]()
	typeRaw  = reflect.TypeFor[json.RawMessage]()
	typeTime = reflect.TypeFor[time.Time]()
)

// removed are the fields manifest/v1 no longer has, by the type that had
// them, with the detail a manifest that still names one is refused with,
// so its author reads why rather than an unknown field.
var removed = map[reflect.Type]map[string]string{
	reflect.TypeFor[v1.AgentSpec](): {
		"identity": "removed: the agent's owner, a person or an organization, decides whose authority it acts with (spec 018)",
	},
}

func (w *walker) add(path, detail string) {
	w.problems = append(w.problems, Problem{Doc: w.doc, Path: path, Detail: detail})
}

// value decodes in into v. A null is an absent field and leaves v zero.
func (w *walker) value(path string, in any, v reflect.Value) {
	if in == nil {
		return
	}
	t := v.Type()
	switch t {
	case typeRaw:
		w.raw(path, in, v)
		return
	case typeTime:
		s, ok := in.(string)
		if !ok {
			w.add(path, "want a timestamp string, got "+kindOf(in))
			return
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			w.add(path, "not an RFC 3339 timestamp")
			return
		}
		v.Set(reflect.ValueOf(ts))
		return
	case typeTool:
		if s, ok := in.(string); ok {
			v.FieldByName("Name").SetString(s)
			return
		}
	}
	switch t.Kind() {
	case reflect.Pointer:
		nv := reflect.New(t.Elem())
		w.value(path, in, nv.Elem())
		v.Set(nv)
	case reflect.Struct:
		w.structure(path, in, v)
	case reflect.Slice:
		list, ok := in.([]any)
		if !ok {
			w.add(path, "want a list, got "+kindOf(in))
			return
		}
		s := reflect.MakeSlice(t, len(list), len(list))
		for i, item := range list {
			w.value(fmt.Sprintf("%s[%d]", path, i), item, s.Index(i))
		}
		v.Set(s)
	case reflect.Map:
		m, ok := in.(map[string]any)
		if !ok {
			w.add(path, "want a mapping, got "+kindOf(in))
			return
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for _, k := range sortedKeys(m) {
			s, ok := m[k].(string)
			if !ok {
				w.add(path+"["+k+"]", "want a string, got "+kindOf(m[k]))
				continue
			}
			out.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(s))
		}
		v.Set(out)
	case reflect.String:
		s, ok := in.(string)
		if !ok {
			w.add(path, "want a string, got "+kindOf(in))
			return
		}
		v.SetString(s)
	case reflect.Bool:
		b, ok := in.(bool)
		if !ok {
			w.add(path, "want a boolean, got "+kindOf(in))
			return
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int64:
		n, ok := integer(in)
		if !ok {
			w.add(path, "want an integer, got "+kindOf(in))
			return
		}
		if v.OverflowInt(n) {
			w.add(path, "the integer is out of range")
			return
		}
		v.SetInt(n)
	case reflect.Float64:
		f, ok := number(in)
		if !ok {
			w.add(path, "want a number, got "+kindOf(in))
			return
		}
		v.SetFloat(f)
	default:
		w.add(path, "the manifest types hold no "+t.Kind().String())
	}
}

// structure decodes a mapping into a struct: unknown keys first, in
// sorted order, then the fields in declaration order.
func (w *walker) structure(path string, in any, v reflect.Value) {
	m, ok := in.(map[string]any)
	if !ok {
		w.add(path, "want a mapping, got "+kindOf(in))
		return
	}
	fields := fieldsOf(v.Type())
	for _, k := range sortedKeys(m) {
		if !slices.ContainsFunc(fields, func(f field) bool { return f.name == k }) {
			detail, gone := removed[v.Type()][k]
			if !gone {
				detail = "unknown field"
			}
			w.add(join(path, k), detail)
		}
	}
	for _, f := range fields {
		if val, ok := m[f.name]; ok {
			w.value(join(path, f.name), val, v.FieldByIndex(f.index))
		}
	}
}

// raw keeps an inputSchema: any JSON object, rendered in canonical form.
func (w *walker) raw(path string, in any, v reflect.Value) {
	if _, ok := in.(map[string]any); !ok {
		w.add(path, "want a mapping, got "+kindOf(in))
		return
	}
	b, err := json.Marshal(jsonable(in))
	if err != nil {
		w.add(path, "not representable as JSON")
		return
	}
	v.SetBytes(b)
}

// jsonable converts the YAML decoder's number types to ones
// encoding/json renders exactly.
func jsonable(in any) any {
	switch x := in.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = jsonable(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = jsonable(v)
		}
		return out
	case uint64:
		return json.Number(strconv.FormatUint(x, 10))
	case int64:
		return json.Number(strconv.FormatInt(x, 10))
	}
	return in
}

// field is one JSON field of a struct, embedded structs flattened as
// encoding/json flattens them.
type field struct {
	name  string
	index []int
}

func fieldsOf(t reflect.Type) []field {
	var out []field
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			for _, inner := range fieldsOf(f.Type) {
				out = append(out, field{name: inner.name, index: append([]int{i}, inner.index...)})
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || !f.IsExported() {
			continue
		}
		out = append(out, field{name: name, index: []int{i}})
	}
	return out
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// integer reads a whole number from either decoder.
func integer(in any) (int64, bool) {
	switch x := in.(type) {
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case int64:
		return x, true
	case float64:
		if x != math.Trunc(x) || math.Abs(x) > 1<<53 {
			return 0, false
		}
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

// number reads any number from either decoder.
func number(in any) (float64, bool) {
	switch x := in.(type) {
	case uint64:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// kindOf names a tree value's type for a problem, never its value.
func kindOf(in any) string {
	switch in.(type) {
	case map[string]any:
		return "a mapping"
	case []any:
		return "a list"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case uint64, int64, float64, json.Number:
		return "a number"
	case nil:
		return "null"
	}
	return fmt.Sprintf("a %T", in)
}
