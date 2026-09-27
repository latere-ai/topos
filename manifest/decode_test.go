// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	v1 "latere.ai/x/topos/manifest/v1"
)

func TestReservedLabelsRefused(t *testing.T) {
	body := "apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata:\n  name: a\n  labels: {team: platform, topos.latere.ai/owner: me}\n  annotations: {topos.latere.ai/pinned: 'yes', note: fine}\nspec: {model: {name: m}}\n"
	e := refused(t, CodeInvalidManifest, body, Options{})
	if len(e.Problems) != 2 || !hasProblem(e, "metadata.labels[topos.latere.ai/owner]", "the core's own") || !hasProblem(e, "metadata.annotations[topos.latere.ai/pinned]", "the core's own") {
		t.Fatalf("problems:\n%s", e.Detail())
	}
	// Another group's keys are fine.
	ok := strings.NewReplacer("topos.latere.ai/owner", "example.com/owner", "topos.latere.ai/pinned", "example.com/pinned").Replace(body)
	one(t, ok, Options{})
}

func TestTheEnvelopeIsCheckedFirst(t *testing.T) {
	// Another apiVersion refuses the file before any other problem.
	body := agent("a", "bogus: 1") + "---\napiVersion: topos.latere.ai/v2\nkind: Agent\nmetadata: {name: b}\nspec: {}\n"
	e := refused(t, CodeUnsupportedVersion, body, Options{})
	if len(e.Problems) != 1 || e.Problems[0].Doc != 1 || !strings.Contains(e.Detail(), "document 2: apiVersion: not topos.latere.ai/v1") {
		t.Fatalf("detail %q", e.Detail())
	}
	for body, want := range map[string]string{
		"kind: Agent\nmetadata: {name: a}\n":                    "apiVersion: required",
		"apiVersion: 1\nkind: Agent\n":                          "apiVersion: want a string, got a number",
		"apiVersion: topos.latere.ai/v1\nmetadata: {name: a}\n": "kind: required",
		"apiVersion: topos.latere.ai/v1\nkind: [Agent]\n":       "kind: want a string, got a list",
		"apiVersion: topos.latere.ai/v1\nkind: Session\n":       "kind: not Agent, Trigger, MemoryStore or Connection",
		"- a\n- b\n": "a document is a mapping, got a list",
		"apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: a}\nspec: {model: {name: [m]}}\n": "spec.model.name: want a string, got a list",
		"": "the file holds no document",
		"apiVersion: topos.latere.ai/v1\nkind: Agent\nmetadata: {name: a\n": "yaml: ",
		`{"apiVersion": "topos.latere.ai/v1", "kind": `:                     "json: not valid JSON",
	} {
		e := refused(t, CodeInvalidManifest, body, Options{})
		if !strings.Contains(e.Detail(), want) {
			t.Errorf("%q: detail %q, want %q", body, e.Detail(), want)
		}
	}
	// Two documents of one kind and name.
	e = refused(t, CodeInvalidManifest, agent("a")+"---\n"+agent("a"), Options{})
	if !strings.Contains(e.Detail(), "document 2: metadata.name: document 1 already declares this Agent") {
		t.Fatalf("detail %q", e.Detail())
	}
	// status is ignored on input.
	r := one(t, agent("a")+"status: {id: agent_x, version: 9}\n", fixed(nil))
	if r.Agent.Status.Version != 1 || r.Agent.Status.ID == "agent_x" {
		t.Fatalf("status %+v", r.Agent.Status)
	}
}

func TestYAMLAndJSONStreamsResolveAlike(t *testing.T) {
	yamlDocs := agent("lead", "model: {name: m}", "subagents: [{name: w, agent: worker}]") + "---\n" + agent("worker", "model: {name: m}", "tools: [read, {name: bash, outputLimit: 1024}]")
	jsonDocs := `{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"lead"},"spec":{"model":{"name":"m"},"subagents":[{"name":"w","agent":"worker"}]}}
	{"apiVersion":"topos.latere.ai/v1","kind":"Agent","metadata":{"name":"worker"},"spec":{"model":{"name":"m"},"tools":["read",{"name":"bash","outputLimit":1024}]}}`
	ry, err := Resolve(t.Context(), []byte(yamlDocs), fixed(nil))
	if err != nil {
		t.Fatal(err)
	}
	rj, err := Resolve(t.Context(), []byte(jsonDocs), fixed(nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := range ry {
		if string(ry[i].Spec) != string(rj[i].Spec) || ry[i].Digest != rj[i].Digest {
			t.Fatalf("document %d:\nYAML %s\nJSON %s", i, ry[i].Spec, rj[i].Spec)
		}
	}
	if !strings.Contains(string(ry[0].Spec), `"tools":["read",{"name":"bash","outputLimit":1024}]`) {
		t.Fatalf("worker %s", ry[0].Spec)
	}
}

func TestReferenceCyclesAndLookupFailures(t *testing.T) {
	cycle := agent("a", "model: {name: m}", "subagents: [{name: b, agent: b}]") + "---\n" + agent("b", "model: {name: m}", "subagents: [{name: a, agent: a}]") + "---\n" + agent("c")
	e := refused(t, CodeInvalidManifest, cycle, Options{})
	if len(e.Problems) != 2 || !strings.Contains(e.Detail(), "in a cycle of references") {
		t.Fatalf("detail %s", e.Detail())
	}

	s := newStore()
	s.fail = errors.New("the store is down")
	for name, body := range map[string]string{
		"status":      agent("a"),
		"credential":  agent("a", "model: {name: m, credential: bot}"),
		"advisor":     agent("a", "model: {name: m}", "advisor: {model: {name: m, credential: bot}}"),
		"subagent":    agent("a", "model: {name: m}", "subagents: [{name: b, agent: b}]"),
		"inline":      agent("a", "model: {name: m}", "subagents: [{name: b, spec: {model: {name: m, credential: bot}}}]"),
		"memoryStore": agent("a", "model: {name: m}", "memoryStores: [{name: n, access: readOnly}]"),
		"connection":  agent("a", "model: {name: m}", "connections: [c]"),
		"mcp":         agent("a", "model: {name: m}", "mcpServers: [{name: s, url: 'https://x', connection: c}]"),
		"trigger":     "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: a, schedule: '@weekly', session: {message: x}}\n",
		"triggerMem":  "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: agent_00000000000000000000000001, schedule: '@hourly', session: {message: x, resources: [{type: memoryStore, memoryStore: n, access: readOnly}]}}\n",
		"memStatus":   "apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: n}\nspec: {description: d}\n",
		"connStatus":  "apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: c}\nspec: {service: s, mode: agent, hosts: [a.com], credential: bot}\n",
	} {
		_, err := Resolve(t.Context(), []byte(body), Options{Lookup: s})
		if !errors.Is(err, s.fail) || Code(err) != "" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStoredObjectsKeepTheirIDs(t *testing.T) {
	s := newStore()
	docs := "apiVersion: topos.latere.ai/v1\nkind: MemoryStore\nmetadata: {name: notes}\nspec: {description: Notes.}\n---\n" +
		"apiVersion: topos.latere.ai/v1\nkind: Connection\nmetadata: {name: web}\nspec: {service: http, mode: agent, hosts: [example.com], credential: github-bot}\n---\n" +
		"apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t}\nspec: {agent: agent_00000000000000000000000009, schedule: '0 0 1 1 0', session: {message: x}}\n"
	s.agents = append(s.agents, &v1.Agent{Metadata: v1.ObjectMeta{Name: "a"}, Status: v1.Status{ID: "agent_00000000000000000000000009", Version: 1}})
	first, err := Resolve(t.Context(), []byte(docs), fixed(s))
	if err != nil {
		t.Fatal(err)
	}
	s.apply(first)
	o := fixed(s)
	o.NewID = func(string) string { t.Fatal("a stored object took a new id"); return "" }
	again, err := Resolve(t.Context(), []byte(docs), o)
	if err != nil {
		t.Fatal(err)
	}
	for i := range first {
		if first[i].Status() != again[i].Status() {
			t.Fatalf("%s: %+v then %+v", first[i].Kind, first[i].Status(), again[i].Status())
		}
	}
	if first[0].Status().ID != "mem_00000000000000000000000001" || first[1].Status().ID != "" || first[1].Connection.Spec.Credential != "cred_"+strings.Repeat("7", 26) {
		t.Fatalf("statuses %+v %+v", first[0].Status(), first[1].Connection)
	}
	if (Resolved{}).Status() != (v1.Status{}) {
		t.Fatal("an empty Resolved has a status")
	}
	// A memory store attached by name pins its id.
	r := one(t, agent("a", "model: {name: m}", "memoryStores: [{name: notes, access: readOnly}]"), fixed(s))
	if r.Agent.Spec.MemoryStores[0].Name != "mem_00000000000000000000000001" {
		t.Fatalf("memory store %+v", r.Agent.Spec.MemoryStores)
	}
	trig := "apiVersion: topos.latere.ai/v1\nkind: Trigger\nmetadata: {name: t2}\nspec: {agent: a, schedule: '@daily', session: {message: x, resources: [{type: memoryStore, memoryStore: notes, access: readOnly}, {type: repository, url: 'https://example.com/r.git'}]}}\n"
	if r := one(t, trig, fixed(s)); r.Trigger.Spec.Session.Resources[0].MemoryStore != "mem_00000000000000000000000001" {
		t.Fatalf("trigger resources %+v", r.Trigger.Spec.Session.Resources)
	}
	// Without a Lookup the default id source mints prefixed ULIDs.
	r = one(t, agent("fresh"), Options{})
	if !strings.HasPrefix(r.Agent.Status.ID, "agent_") || len(r.Agent.Status.ID) != len("agent_")+26 {
		t.Fatalf("id %s", r.Agent.Status.ID)
	}
}

func TestInstructionsFile(t *testing.T) {
	o := fixed(nil)
	r := one(t, agent("a", "model: {name: m}", "instructionsFile: triager.md"), o)
	if !strings.HasPrefix(r.Agent.Spec.Instructions, "Label each new issue") || r.Agent.Spec.InstructionsFile != "" || strings.Contains(string(r.Spec), "instructionsFile") {
		t.Fatalf("spec %s", r.Spec)
	}
	for file, want := range map[string]string{"missing.md": "cannot be read", "../etc/passwd": "not a relative path"} {
		e := refused(t, CodeInvalidManifest, agent("a", "model: {name: m}", "instructionsFile: "+file), o)
		if !hasProblem(e, "spec.instructionsFile", want) {
			t.Fatalf("%s: %s", file, e.Detail())
		}
	}
}

func TestTheWalkerReadsEveryType(t *testing.T) {
	var s struct {
		N  int             `json:"n"`
		F  float64         `json:"f"`
		B  bool            `json:"b"`
		T  time.Time       `json:"t"`
		R  json.RawMessage `json:"r"`
		U  uint            `json:"u"`
		In struct {
			X string `json:"x"`
		} `json:"in"`
		skip string
		Dash string `json:"-"`
	}
	w := &walker{}
	tree := map[string]any{
		"n": json.Number("7"), "f": json.Number("0.5"), "b": true, "t": "2026-09-27T09:00:00Z",
		"r": map[string]any{"k": uint64(1), "l": []any{int64(-1)}}, "in": map[string]any{"x": "y"},
	}
	w.value("", tree, reflect.ValueOf(&s).Elem())
	if len(w.problems) != 0 || s.N != 7 || s.F != 0.5 || !s.B || s.T.Year() != 2026 || string(s.R) != `{"k":1,"l":[-1]}` || s.In.X != "y" {
		t.Fatalf("%+v %v", s, w.problems)
	}
	w = &walker{}
	w.value("", map[string]any{
		"n": 1.5, "f": "x", "b": uint64(1), "t": uint64(5), "r": "x", "u": uint64(1), "in": []any{}, "skip": "x", "Dash": "x",
	}, reflect.ValueOf(&s).Elem())
	want := map[string]string{
		"n": "want an integer, got a number", "f": "want a number, got a string", "b": "want a boolean, got a number",
		"t": "want a timestamp string, got a number", "r": "want a mapping, got a string", "u": "the manifest types hold no uint",
		"in": "want a mapping, got a list", "skip": "unknown field", "Dash": "unknown field",
	}
	if len(w.problems) != len(want) {
		t.Fatalf("problems %v", w.problems)
	}
	for _, p := range w.problems {
		if want[p.Path] != p.Detail {
			t.Errorf("%s: %s", p.Path, p.Detail)
		}
	}
	w = &walker{}
	w.value("", map[string]any{"t": "yesterday", "n": uint64(1 << 63)}, reflect.ValueOf(&s).Elem())
	if len(w.problems) != 2 || w.problems[0].Detail != "want an integer, got a number" || w.problems[1].Detail != "not an RFC 3339 timestamp" {
		t.Fatalf("problems %v", w.problems)
	}
	var small struct {
		I8 int32 `json:"i"`
	}
	w = &walker{}
	w.value("", map[string]any{"i": int64(1)}, reflect.ValueOf(&small).Elem())
	if len(w.problems) != 1 {
		t.Fatalf("an int32 decoded: %v", w.problems)
	}
	var i64 struct {
		N int64 `json:"n"`
	}
	for _, in := range []any{int64(-3), float64(4), json.Number("x"), 1e300} {
		w = &walker{}
		w.value("", map[string]any{"n": in}, reflect.ValueOf(&i64).Elem())
		if _, ok := integer(in); ok == (len(w.problems) > 0) {
			t.Fatalf("%v: %v", in, w.problems)
		}
	}
	for _, in := range []any{uint64(1), int64(1), json.Number("x"), nil, map[string]string{}} {
		if _, ok := number(in); ok != (in == uint64(1) || in == int64(1)) {
			t.Fatalf("number %v", in)
		}
	}
	if kindOf(map[string]string{}) != "a map[string]string" || kindOf(nil) != "null" || kindOf(true) != "a boolean" {
		t.Fatal("kindOf")
	}
}

func TestErrorsRender(t *testing.T) {
	e := newError(CodeUnknownReference, 1, nil)
	if e.Error() != "unknown_reference: The manifest references an object that does not exist." {
		t.Fatalf("%q", e.Error())
	}
	e = newError(CodeInvalidManifest, 2, []Problem{{Doc: 1, Detail: "whole"}})
	if e.Detail() != "document 2: whole" {
		t.Fatalf("%q", e.Detail())
	}
	if Code(e) != CodeInvalidManifest || Code(errors.New("x")) != "" {
		t.Fatal("Code")
	}
}

func TestTheInputCheck(t *testing.T) {
	for text, want := range map[string]string{
		"sk-ant-api03-Zq8vXk2Lr9TnB4wYc7HdM1pF":          "a token of the form sk-ant-",
		"token=gho_R8dK2mQ9xT4vB7nL1cF6":                 "a token of the form gho_",
		"xoxp-1234567890-abcdefghij":                     "a token of the form xoxp-",
		"-----BEGIN OPENSSH PRIVATE KEY-----":            "a PEM private key",
		"aGVsbG8gd29ybGQgdGhpcyBpcyBhIHNlY3JldA1234==":   "a high-entropy string",
		"0123456789abcdef0123456789abcdef":               "a high-entropy hex string",
		"0000000000000000000000000000000000000000":       "",
		"TestStrictDecodingCollectsEveryProblem":         "",
		"internal/toposcli/toposcli_test.go":             "",
		"12345678901234567890123456789":                  "a high-entropy hex string",
		"aaaaaaaaaaaaaaaaaaaaaaaa1aaaaaaaaaaaaaaaaaaaa":  "",
		"the risk-assessment-procedures of the task-sk-": "",
	} {
		got := strings.Join(secretKinds(text), ", ")
		if !strings.Contains(got, want) || (want == "") != (got == "") {
			t.Errorf("%q: %q, want %q", text, got, want)
		}
	}
	e := refused(t, CodeInvalidManifest, agent("a", "model: {name: m}", "tools: [{name: ask, client: true, description: x, inputSchema: {min: .nan}}]"), Options{})
	if !hasProblem(e, "spec.tools[0].inputSchema", "not representable as JSON") {
		t.Fatalf("NaN schema: %s", e.Detail())
	}
}
