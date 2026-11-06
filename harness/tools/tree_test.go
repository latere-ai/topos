// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"testing"

	"latere.ai/x/topos/models"
)

// TestDecodeTreeKeepsTheDigitsAModelWrote: a number stays as written, a
// repeated name is read with the last value, and text after the value is
// an error.
func TestDecodeTreeKeepsTheDigitsAModelWrote(t *testing.T) {
	v, err := DecodeTree([]byte("{\"id\":12345678901234567890,\"f\":1.50,\"id\":9007199254740993,\"s\":\"\xff\",\"l\":[1,true,null,\"x\"]}"))
	if err != nil {
		t.Fatal(err)
	}
	m := v.(map[string]any)
	if string(m["id"].(jsontext.Value)) != "9007199254740993" || string(m["f"].(jsontext.Value)) != "1.50" || m["s"] != "�" {
		t.Fatalf("%#v", m)
	}
	if l := m["l"].([]any); len(l) != 4 || l[1] != true || l[2] != nil || l[3] != "x" {
		t.Fatalf("%#v", l)
	}
	out, err := json.Marshal(m["l"], models.ToProvider)
	if err != nil || string(out) != `[1,true,null,"x"]` {
		t.Fatalf("%s, %v", out, err)
	}
	for _, bad := range []string{`{"a":1} {"b":2}`, `{"a":1}}`} {
		if _, err := DecodeTree([]byte(bad)); !errors.Is(err, errTrailing) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if _, err := DecodeTree([]byte(`{"a":`)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("a truncated value: %v", err)
	}
}

// TestASchemaComparesNumbersByTheirDigits: an integer past 2^53 is
// compared exactly, not rounded to a float.
func TestASchemaComparesNumbersByTheirDigits(t *testing.T) {
	s, err := CompileSchema(jsontext.Value(`{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740992}}}`))
	if err != nil {
		t.Fatal(err)
	}
	v, err := DecodeTree([]byte(`{"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	if probs := s.Validate(v); len(probs) != 1 {
		t.Fatalf("%v", probs)
	}
}
