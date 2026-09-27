// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBuiltinsFollowTheTable(t *testing.T) {
	want := []struct {
		name     string
		parallel bool
		effect   Effect
	}{
		{NameRead, true, EffectRead},
		{NameWrite, false, EffectWrite},
		{NameEdit, false, EffectWrite},
		{NameBash, false, EffectWrite},
		{NameGrep, true, EffectRead},
		{NameGlob, true, EffectRead},
		{NameWebFetch, true, EffectExternal},
		{NameTodo, true, EffectNone},
	}
	got := Builtins()
	if len(got) != len(want) {
		t.Fatalf("%d built-ins", len(got))
	}
	r := NewRegistry()
	for i, tool := range got {
		d, p := tool.Definition(), tool.Properties()
		if d.Name != want[i].name || p.Parallel != want[i].parallel || p.Effect != want[i].effect || p.Repeatable || p.Client || p.OutputLimit != 0 {
			t.Fatalf("%d: %s %+v, want %+v", i, d.Name, p, want[i])
		}
		if d.Description == "" || d.Description != strings.TrimSpace(d.Description) {
			t.Fatalf("%s: the description is empty or untrimmed", d.Name)
		}
		for _, banned := range []string{"—", "–"} {
			if strings.Contains(d.Description, banned) {
				t.Fatalf("%s: the description has a dash %q", d.Name, banned)
			}
		}
		if err := r.AddBuiltin(tool); err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(d.InputSchema, &schema); err != nil {
			t.Fatalf("%s: %v", d.Name, err)
		}
		for _, req := range schema.Required {
			if _, ok := schema.Properties[req]; !ok {
				t.Fatalf("%s requires %s, which it does not define", d.Name, req)
			}
		}
		// Every input the schema names is described for the model.
		for name := range schema.Properties {
			if !strings.Contains(d.Description, "`"+name+"`") {
				t.Fatalf("the description of %s does not name its input %s", d.Name, name)
			}
		}
	}
	if !slices.Equal(r.Names(), []string{"read", "write", "edit", "bash", "grep", "glob", "web_fetch", "todo"}) {
		t.Fatalf("order %v", r.Names())
	}
}

func TestBuiltinsDecodeTheirInput(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	for _, tool := range Builtins() {
		res := run(ctx, t, tool, f.h, State{}, `{"path":[1],"command":[1],"pattern":[1],"url":[1],"todos":1}`)
		if res.Outcome != OutcomeInvalidInput || !strings.HasPrefix(text(res), "The input does not match the schema of "+tool.Definition().Name+":\n/") || strings.Contains(text(res), "Go ") {
			t.Fatalf("%s: %s %q", tool.Definition().Name, res.Outcome, text(res))
		}
	}
	// A whole-number float passes the schema's integer check and is
	// still refused as input by the decoder, never run.
	res := run(ctx, t, builtinTool(t, NameRead), f.h, State{}, `{"path":"a","offset":1.0}`)
	if res.Outcome != OutcomeInvalidInput || text(res) != "The input does not match the schema of read:\n/offset: expected integer, got number 1.0" {
		t.Fatalf("a float offset %s %q", res.Outcome, text(res))
	}
	for input, want := range map[string]string{
		`{"todos":[{"id":1}]}`:                  "/todos/0/id: expected string, got number",
		`{"todos":{}}`:                          "/todos: expected array, got object",
		`{"todos":[true]}`:                      "/todos/0: expected object, got boolean",
		`{"todos":[{"id":"a","status":false}]}`: "/todos/0/status: expected string, got boolean",
		`{"todos":1}`:                           "/todos: expected array, got number",
		`[`:                                     "/: not valid input",
	} {
		res := run(ctx, t, builtinTool(t, NameTodo), f.h, State{}, input)
		if res.Outcome != OutcomeInvalidInput || text(res) != "The input does not match the schema of todo:\n"+want {
			t.Fatalf("%s: %q", input, text(res))
		}
	}
	if jsonKind(reflect.TypeFor[float64]()) != "number" || jsonKind(reflect.TypeFor[bool]()) != "boolean" || jsonKind(reflect.TypeFor[[2]int]()) != "array" {
		t.Fatal("jsonKind")
	}
	// No input at all decodes as an empty object.
	res = run(ctx, t, builtinTool(t, NameGlob), f.h, State{}, ``)
	if res.Outcome != OutcomeError || !strings.HasPrefix(text(res), "The search failed: ") {
		t.Fatalf("empty input %s %q", res.Outcome, text(res))
	}
}
