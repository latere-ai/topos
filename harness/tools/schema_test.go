// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// value decodes a JSON value as the registry does, with numbers kept.
func value(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestCompileSchemaRefuses(t *testing.T) {
	for _, c := range []struct{ schema, want string }{
		{`{`, "not JSON"},
		{`1`, "/: a schema is an object or a boolean"},
		{`{"properties":{"a":1}}`, "/properties/a: a schema is an object or a boolean"},
		{`{"type":1}`, "/type: not a string or an array of strings"},
		{`{"type":[1]}`, "/type: not a string or an array of strings"},
		{`{"type":"float"}`, `/type: "float" is not a JSON Schema type`},
		{`{"properties":[]}`, "/properties: not an object"},
		{`{"required":"a"}`, "/required: not an array"},
		{`{"required":[1]}`, "/required: not an array of strings"},
		{`{"additionalProperties":{"type":"x"}}`, `/additionalProperties/type: "x" is not a JSON Schema type`},
		{`{"items":{"type":"x"}}`, `/items/type: "x" is not a JSON Schema type`},
		{`{"enum":[]}`, "/enum: not a non-empty array"},
		{`{"enum":"a"}`, "/enum: not a non-empty array"},
		{`{"minimum":"1"}`, "/minimum: not a number"},
		{`{"maximum":true}`, "/maximum: not a number"},
		{`{"minLength":-1}`, "/minLength: not a non-negative integer"},
		{`{"maxLength":1.5}`, "/maxLength: not a non-negative integer"},
		{`{"maxLength":"2"}`, "/maxLength: not a non-negative integer"},
		{`{"pattern":1}`, "/pattern: not a string"},
		{`{"pattern":"("}`, "/pattern: error parsing regexp"},
		{`{"oneOf":[]}`, "/oneOf: not a non-empty array"},
		{`{"anyOf":[{"type":"x"}]}`, `/anyOf/0/type: "x" is not a JSON Schema type`},
		{`{"maxItems":1}`, `/: the keyword "maxItems" is not supported`},
		{`{"properties":{"a/b~":{"minItems":1}}}`, `/properties/a~1b~0: the keyword "minItems" is not supported`},
		{`{"properties":{"a":{"properties":{"b":{"$ref":"#"}}}}}`, `/properties/a/properties/b: the keyword "$ref" is not supported`},
	} {
		_, err := CompileSchema(json.RawMessage(c.schema))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.schema, err, c.want)
		}
	}
	for _, s := range []string{``, `  `, `true`, `{}`, `{"title":"t","description":"d","default":1,"examples":[1],"$schema":"x","$id":"y","format":"uri","deprecated":false,"readOnly":true,"writeOnly":false,"$comment":"c"}`} {
		if _, err := CompileSchema(json.RawMessage(s)); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
}

func TestSchemaValidates(t *testing.T) {
	schema := `{
	  "type": "object",
	  "properties": {
	    "s": {"type": "string", "minLength": 2, "maxLength": 3, "pattern": "^[a-zé]+$"},
	    "n": {"type": "number", "minimum": 0.5, "maximum": 10},
	    "i": {"type": "integer"},
	    "b": {"type": "boolean"},
	    "z": {"type": "null"},
	    "u": {"type": ["string", "null"]},
	    "e": {"enum": [1, "a", {"k": true}]},
	    "c": {"const": "x"},
	    "list": {"type": "array", "items": {"type": "integer"}},
	    "obj": {"type": "object", "required": ["k"], "additionalProperties": {"type": "string"}},
	    "one": {"oneOf": [{"type": "string"}, {"minLength": 1}]},
	    "two": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
	    "any": {"anyOf": [{"type": "integer"}, {"type": "boolean"}]},
	    "never": false,
	    "ever": true,
	    "a~b": {"type": "string"}
	  },
	  "required": ["s"],
	  "additionalProperties": false
	}`
	s, err := CompileSchema(json.RawMessage(schema))
	if err != nil {
		t.Fatal(err)
	}
	ok := `{"s":"éa","n":10,"i":3,"b":false,"z":null,"u":null,"e":{"k":true},"c":"x","list":[1,2],"obj":{"k":"v"},"one":5,"any":true,"ever":[{}],"a~b":"x"}`
	if probs := s.Validate(value(t, ok)); len(probs) != 0 {
		t.Fatalf("a valid value: %v", probs)
	}
	bad := `{"n":0.1,"i":1.5,"b":"no","z":0,"u":1,"e":2,"c":"y","list":[1,"2",3.5],"obj":{"x":1},"one":"two","any":"s","never":1,"a~b":1,"extra":{}}`
	want := []string{
		`/: missing required property "s"`,
		`/any: matches none of the allowed forms`,
		`/a~0b: expected string, got number`,
		`/b: expected boolean, got string`,
		`/c: must be "x"`,
		`/e: must be one of 1, "a", {"k":true}`,
		`/extra: unexpected property`,
		`/i: expected integer, got number`,
		`/list/1: expected integer, got string`,
		`/list/2: expected integer, got number`,
		`/n: less than the minimum 0.5`,
		`/never: no value is allowed here`,
		`/obj: missing required property "k"`,
		`/obj/x: expected string, got number`,
		`/one: matches 2 of the forms, exactly one is allowed`,
		`/u: expected string or null, got number`,
		`/z: expected null, got number`,
	}
	if got := s.Validate(value(t, bad)); !slices.Equal(got, want) {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range []struct{ v, want string }{
		{`{"s":"a"}`, `/s: shorter than 2 characters`},
		{`{"s":"abcd"}`, `/s: longer than 3 characters`},
		{`{"s":"AB"}`, `/s: does not match the pattern ^[a-zé]+$`},
		{`{"s":"ab","n":11}`, `/n: greater than the maximum 10`},
		{`{"s":"ab","two":[]}`, `/two: matches 0 of the forms, exactly one is allowed`},
		{`[]`, `/: expected object, got array`},
		{`true`, `/: expected object, got boolean`},
	} {
		got := s.Validate(value(t, c.v))
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: %v, want %q", c.v, got, c.want)
		}
	}
	empty, err := CompileSchema(nil)
	if err != nil || len(empty.Validate(value(t, `[1]`))) != 0 {
		t.Fatalf("the empty schema %v", err)
	}
	if typeOf(struct{}{}) != "unknown" || !bytes.Equal(canonical(func() {}), nil) {
		t.Fatal("an unknown value")
	}
	if hasType("x", "integer") || hasType(json.Number("x"), "integer") || hasType("x", "number") {
		t.Fatal("hasType")
	}
}
