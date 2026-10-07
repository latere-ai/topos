// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"maps"
	"strings"
	"testing"
)

// TestMetadataRules holds the key and value rules of spec 057: a key a
// query parameter's name can carry, a value of at most MaxMetadataValue
// bytes without a control character, and at most MaxMetadata entries.
func TestMetadataRules(t *testing.T) {
	for _, c := range []struct {
		key string
		ok  bool
	}{
		{"project", true}, {"a", true}, {"9", true}, {"x.y_z-1", true}, {strings.Repeat("k", 63), true},
		{"", false}, {".dot", false}, {"_u", false}, {"-h", false}, {"a b", false}, {"a=b", false}, {"a&b", false},
		{strings.Repeat("k", 64), false}, {"ключ", false},
	} {
		if got := ValidMetadataKey(c.key); got != c.ok {
			t.Errorf("ValidMetadataKey(%q) = %v, want %v", c.key, got, c.ok)
		}
	}
	for _, c := range []struct {
		value string
		ok    bool
	}{
		{"", true}, {"prj_01", true}, {"a value: with = and &", true}, {"日本語", true}, {strings.Repeat("v", MaxMetadataValue), true},
		{strings.Repeat("v", MaxMetadataValue+1), false}, {"tab\there", false}, {"line\nbreak", false}, {"nul\x00", false}, {"sep ", false},
	} {
		if why := CheckMetadataValue(c.value); (why == "") != c.ok {
			t.Errorf("CheckMetadataValue(%q) = %q, want ok %v", c.value, why, c.ok)
		}
	}
	full := map[string]string{}
	for i := range MaxMetadata {
		full["k"+strings.Repeat("x", i)] = "v"
	}
	if err := CheckMetadata(full); err != nil {
		t.Fatalf("%d entries: %v", MaxMetadata, err)
	}
	full["one-more"] = "v"
	if err := CheckMetadata(full); err == nil {
		t.Fatalf("%d entries passed", len(full))
	}
	if err := CheckMetadata(map[string]string{"ok": "v", "bad key": "v"}); err == nil || !strings.Contains(err.Error(), `"bad key"`) {
		t.Fatalf("a bad key: %v", err)
	}
	if err := CheckMetadata(map[string]string{"k": "a\nb"}); err == nil || !strings.Contains(err.Error(), `"k"`) {
		t.Fatalf("a bad value: %v", err)
	}
	if err := CheckMetadata(nil); err != nil {
		t.Fatalf("no metadata: %v", err)
	}
}

// TestMergeMetadata: a value sets its key, nil deletes it, a key the
// change does not name is kept, the map merged into is not modified, and
// an empty result is nil.
func TestMergeMetadata(t *testing.T) {
	m := map[string]string{"folder": "f1", "pinned": "yes"}
	before := maps.Clone(m)
	got, changed := MergeMetadata(m, map[string]*string{"folder": new("f2"), "pinned": nil, "new": new("1"), "absent": nil})
	if want := map[string]string{"folder": "f2", "new": "1"}; !changed || !maps.Equal(got, want) {
		t.Fatalf("merged %v (changed %v), want %v", got, changed, want)
	}
	if !maps.Equal(m, before) {
		t.Fatalf("the merge modified its input: %v", m)
	}
	if got, changed := MergeMetadata(m, map[string]*string{"folder": new("f1"), "absent": nil}); changed || !maps.Equal(got, m) {
		t.Fatalf("a change to what m holds: %v, changed %v", got, changed)
	}
	if got, changed := MergeMetadata(nil, map[string]*string{"folder": new("f1")}); !changed || got["folder"] != "f1" {
		t.Fatalf("a set on no metadata: %v, %v", got, changed)
	}
	if got, changed := MergeMetadata(map[string]string{"folder": "f1"}, map[string]*string{"folder": nil}); !changed || got != nil {
		t.Fatalf("deleting the last entry: %v, %v; want nil", got, changed)
	}
	s := Session{Metadata: map[string]string{"a": "1"}}
	if changed, err := Relabel(&s, map[string]*string{"a": nil}); err != nil || !changed || s.Metadata != nil {
		t.Fatalf("Relabel: %v, %v, %v", s.Metadata, changed, err)
	}
}
