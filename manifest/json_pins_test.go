// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"encoding/json/jsontext"
	"reflect"
	"testing"

	v1 "latere.ai/x/topos/manifest/v1"
	"latere.ai/x/topos/session"
)

// TestParseJSONKeepsNumbersAndAcceptsWhatAPersonWrote: a number past 2^53
// stays exact, a repeated name takes the last value, and a stream of
// documents is read to its end.
func TestParseJSONKeepsNumbersAndAcceptsWhatAPersonWrote(t *testing.T) {
	docs, err := parseJSON([]byte("{\"n\":9007199254740993,\"n\":9007199254740995,\"f\":0.5,\"s\":\"\xff\"} {\"b\":true}"))
	if err != nil || len(docs) != 2 {
		t.Fatalf("%v, %v", docs, err)
	}
	m := docs[0].(map[string]any)
	n, ok := integer(m["n"])
	if !ok || n != 9007199254740995 {
		t.Fatalf("n = %v, %v", n, ok)
	}
	if f, ok := number(m["f"]); !ok || f != 0.5 || m["s"] != "�" {
		t.Fatalf("%#v", m)
	}
	if _, err := parseJSON([]byte(`{"a":`)); err == nil {
		t.Fatal("a truncated document parsed")
	}
}

// TestAnInputSchemaIsRenderedInCanonicalForm: the schema a manifest holds
// is written with sorted keys and its YAML integers exact.
func TestAnInputSchemaIsRenderedInCanonicalForm(t *testing.T) {
	in := map[string]any{"type": "object", "maximum": int64(9007199254740993), "min": uint64(3), "b": jsontext.Value("1.50"), "a": 0.5}
	w := &walker{}
	var raw jsontext.Value
	w.raw("spec", in, reflect.ValueOf(&raw).Elem())
	if string(raw) != `{"a":0.5,"b":1.50,"maximum":9007199254740993,"min":3,"type":"object"}` {
		t.Fatalf("%s", raw)
	}
}

// TestASpecKeepsTheFormItsDigestWasTakenOver: an empty list of a spec is
// written as null, so the digest of a spec stored earlier is the digest of
// the same spec rendered again, and a bundle read back hashes to its status.
func TestASpecKeepsTheFormItsDigestWasTakenOver(t *testing.T) {
	s := newStore()
	r := one(t, agent("plain", "model: {name: claude-haiku-4-5}"), fixed(s))
	if b, err := session.MarshalDigested(v1.AgentSpec{}); err != nil || !bytes.Contains(b, []byte(`"tools":null`)) {
		t.Fatalf("an empty spec = %s, %v", b, err)
	}
	if r.Digest != string(session.DigestOf(r.Spec)) {
		t.Fatalf("digest %s is not the digest of the spec", r.Digest)
	}
	b, err := r.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ReadBundle(b)
	if err != nil || back.Digest != r.Digest || !bytes.Equal(back.Spec, r.Spec) {
		t.Fatalf("ReadBundle = %s, %v", back.Spec, err)
	}
}
