// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"encoding/json/jsontext"
	"testing"
	"time"
)

// TestAnEnvelopePayloadKeepsItsNumbers: a template reads a payload's
// numbers as written, a webhook's repeated names and invalid UTF-8 are
// read, and a list or object renders as compact JSON with sorted keys.
func TestAnEnvelopePayloadKeepsItsNumbers(t *testing.T) {
	e := Envelope{ID: "e1", Product: "github", Verb: "push", Time: time.Unix(0, 0), Payload: jsontext.Value("{\"n\":12345678901234567890,\"f\":2.50,\"n\":9007199254740993,\"s\":\"a\xff\",\"o\":{\"z\":1,\"a\":\"<b>\"}}")}
	v, err := e.Value()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"payload.n": "9007199254740993", "payload.f": "2.50", "payload.s": "a�", "payload.o": `{"a":"<b>","z":1}`} {
		got, _ := Lookup(Values(v), splitPath(path))
		if got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
	if _, err := (Envelope{Payload: jsontext.Value(`{"a":1} {"b":2}`)}).Value(); err == nil {
		t.Error("text after the payload decoded")
	}
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '.' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	return out
}
